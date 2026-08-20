#!/usr/bin/env bash
set -euo pipefail

# Measures what the registry actually delivers, so a shaped benchmark cell can
# prove its rate the way the RTT sweep proved its delay through the TCP
# handshake. The registry must be reached on its cluster-network address: the
# published host port goes through the docker proxy, which is not shaped and
# would report full speed. Prints bytes per second on stdout
IMAGE="${IMAGE:-fn-pytorch-cpu}"
TAG="${TAG:-oci}"
REGISTRY_IP="$(docker inspect \
  -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' \
  registry.localhost)"
if [[ -z "${REGISTRY_IP}" ]]; then
  echo "the registry has no address on the cluster network" >&2
  exit 1
fi

MANIFEST_TYPES='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'

fetch_manifest() {
  curl -sf -m 30 -H "Accept: ${MANIFEST_TYPES}" \
    "http://${REGISTRY_IP}:5000/v2/${IMAGE}/manifests/$1"
}

# The corpus images are published as multi-platform indexes, so the tag names
# a list of manifests rather than the layers themselves and the platform the
# cluster runs has to be resolved before any layer is visible
manifest="$(fetch_manifest "${TAG}")"
child="$(printf '%s' "${manifest}" | python3 -c '
import json, sys

manifest = json.load(sys.stdin)
if "manifests" not in manifest:
    sys.exit(0)
for entry in manifest["manifests"]:
    platform = entry.get("platform") or {}
    if platform.get("architecture") == "amd64" and platform.get("os") == "linux":
        print(entry["digest"])
        sys.exit(0)
print("no linux/amd64 manifest in the index", file=sys.stderr)
sys.exit(1)
')"
if [[ -n "${child}" ]]; then
  manifest="$(fetch_manifest "${child}")"
fi

# The largest layer of a large image is the only blob big enough that the
# token bucket reaches steady state before the transfer ends
blob="$(printf '%s' "${manifest}" | python3 -c '
import json, sys

layers = json.load(sys.stdin)["layers"]
print(max(layers, key=lambda layer: layer["size"])["digest"])
')"

curl -o /dev/null -s -m 300 -w '%{speed_download}' \
  "http://${REGISTRY_IP}:5000/v2/${IMAGE}/blobs/${blob}"
echo
