#!/usr/bin/env bash
# Measures the download rate the local registry really delivers through the shaped link
set -euo pipefail

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

blob="$(printf '%s' "${manifest}" | python3 -c '
import json, sys

layers = json.load(sys.stdin)["layers"]
print(max(layers, key=lambda layer: layer["size"])["digest"])
')"

curl -o /dev/null -s -m 300 -w '%{speed_download}' \
  "http://${REGISTRY_IP}:5000/v2/${IMAGE}/blobs/${blob}"
echo
