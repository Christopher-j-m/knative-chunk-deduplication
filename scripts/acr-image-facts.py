"""Collect what the cold guards need to know about every corpus image in an ACR.

For each image and tag it records the compressed bytes of the layers (what an
eager pull moves across the wire), the layer digests (what the content store
would hold after a pull) and the chain IDs (the names under which containerd
keeps unpacked layers as snapshots). The registry is asked anonymously, the
same way the cluster nodes pull.

Layers the cluster's own service images already hold are left out of the
digest lists and subtracted from the bytes a pull has to move.

FACT_TAGS, a comma-separated list, names the tags to describe; it defaults to
the eager and the uncompressed lazy conversion.

Usage: python3 acr-image-facts.py [--infrastructure=[<registry>/]<image:tag>,...] <acr-login-server> <image> [<image> ...] > facts.json
"""

import hashlib
import json
import os
import sys
import urllib.error
import urllib.request

MANIFEST_TYPES = ",".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
])
TAGS = tuple(tag for tag in os.environ.get("FACT_TAGS", "oci,nydus-none").split(",") if tag)


# ACR's anonymous pull still answers a bare request with a bearer challenge,
# so every repository needs its own short-lived anonymous token first
def anonymous_token(registry, image):
    url = f"https://{registry}/oauth2/token?service={registry}&scope=repository:{image}:pull"
    with urllib.request.urlopen(url, timeout=30) as response:
        return json.load(response)["access_token"]


# Lets a redirect surface as an error instead of being followed with the
# registry token attached
class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


_registry_opener = urllib.request.build_opener(_NoRedirect)


# ACR answers every blob read with a redirect to its storage backend, which
# rejects the registry's bearer token, so the redirect is followed by hand
# without it
def fetch(registry, image, path, token, accept=MANIFEST_TYPES):
    request = urllib.request.Request(
        f"https://{registry}/v2/{image}/{path}",
        headers={"Accept": accept, "Authorization": f"Bearer {token}"})
    try:
        with _registry_opener.open(request, timeout=60) as response:
            return json.load(response)
    except urllib.error.HTTPError as redirect:
        if redirect.code not in (301, 302, 303, 307, 308):
            raise
        location = redirect.headers["Location"]

    with urllib.request.urlopen(location, timeout=60) as response:
        return json.load(response)


# A multi-platform index wraps the manifest the amd64 nodes actually pull
def platform_manifest(registry, image, tag, token):
    document = fetch(registry, image, f"manifests/{tag}", token)
    if "manifests" not in document:
        return document

    amd64 = [entry for entry in document["manifests"]
             if entry.get("platform", {}).get("architecture") == "amd64"]
    if not amd64:
        raise SystemExit(f"{image}:{tag} has no amd64 manifest")
    return fetch(registry, image, f"manifests/{amd64[0]['digest']}", token)


# containerd names an unpacked layer by the hash of its parent's name and its
# own uncompressed digest, so the names can be rebuilt from the image config
def chain_ids(diff_ids):
    names = []
    for diff_id in diff_ids:
        if not names:
            names.append(diff_id)
            continue
        joined = f"{names[-1]} {diff_id}".encode()
        names.append("sha256:" + hashlib.sha256(joined).hexdigest())
    return names


def describe_tag(registry, image, tag, token):
    manifest = platform_manifest(registry, image, tag, token)
    config = fetch(registry, image, f"blobs/{manifest['config']['digest']}", token,
                   accept="application/json, */*")
    diff_ids = config.get("rootfs", {}).get("diff_ids", [])
    return manifest["layers"], chain_ids(diff_ids)


# Layers our own service images share with the corpus (an alpine base layer,
# for instance) sit on every node for as long as the cluster runs, so a pull
# never has to move them and the guards must not demand that they be gone
def infrastructure_fingerprints(registry, references):
    digests, chains = set(), set()
    for reference in references:
        # A reference may name its own registry, since the service images can
        # live elsewhere than the corpus
        host = registry
        if "/" in reference:
            host, reference = reference.split("/", 1)
        image, tag = reference.rsplit(":", 1)
        layers, chain_names = describe_tag(host, image, tag, anonymous_token(host, image))
        digests.update(layer["digest"] for layer in layers)
        chains.update(chain_names)
    return digests, chains


def describe(registry, image, infrastructure):
    infrastructure_digests, infrastructure_chains = infrastructure
    token = anonymous_token(registry, image)
    facts = {}

    for tag in TAGS:
        layers, chain_names = describe_tag(registry, image, tag, token)
        held = [layer for layer in layers if layer["digest"] in infrastructure_digests]
        compressed = sum(layer["size"] for layer in layers)

        facts[tag] = {
            "compressed_bytes": compressed,
            "transfer_bytes": compressed - sum(layer["size"] for layer in held),
            "infrastructure_layers": [layer["digest"] for layer in held],
            "layer_digests": [layer["digest"] for layer in layers
                              if layer["digest"] not in infrastructure_digests],
            "chain_ids": [name for name in chain_names if name not in infrastructure_chains],
        }
    return facts


def main():
    arguments = sys.argv[1:]
    references = []
    if arguments and arguments[0].startswith("--infrastructure="):
        references = [entry for entry in arguments.pop(0).split("=", 1)[1].split(",") if entry]
    if len(arguments) < 2:
        raise SystemExit(__doc__)

    registry = arguments[0]
    infrastructure = infrastructure_fingerprints(registry, references)
    report = {image: describe(registry, image, infrastructure) for image in arguments[1:]}
    json.dump(report, sys.stdout, indent=1)
    print()


if __name__ == "__main__":
    main()
