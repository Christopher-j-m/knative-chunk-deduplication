#!/usr/bin/env bash
# Checks for the local experiments that an eager pull really moved the image across the shaped link, sourced by the experiment drivers
compressed_layer_bytes() {
  local image="$1"

  python3 - "${REGISTRY}" "${image}" <<'PYEOF'
import json
import sys
import urllib.request

registry, image = sys.argv[1], sys.argv[2]
accept = ",".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
])

def manifest(reference):
    request = urllib.request.Request(
        f"http://{registry}/v2/{image}/manifests/{reference}",
        headers={"Accept": accept})
    return json.load(urllib.request.urlopen(request, timeout=30))

document = manifest("oci")
if "manifests" in document:
    amd64 = [entry for entry in document["manifests"]
             if entry.get("platform", {}).get("architecture") == "amd64"]
    if not amd64:
        raise SystemExit(1)
    document = manifest(amd64[0]["digest"])

print(sum(layer["size"] for layer in document["layers"]))
PYEOF
}

assert_cell_cold() {
  local csv="$1" image="$2" configured_bps="$3" bytes floor violations
  bytes="$(compressed_layer_bytes "${image}")"
  if [[ -z "${bytes}" ]]; then
    echo "cold guard: no compressed layer size known for ${image}, so its cold rows cannot be checked" >&2
    return 1
  fi

  floor="$(awk -v b="${bytes}" -v r="${configured_bps}" 'BEGIN { printf "%.3f", 0.6 * b / r }')"
  violations="$(awk -F, -v floor="${floor}" \
    'NR > 1 && $5 == "no" && $7 + 0 < floor { print "  rep " $2 " " $3 " " $4 " pull_seconds " $7 }' \
    "${csv}")"
  if [[ -n "${violations}" ]]; then
    echo "cold guard: cold rows below the ${floor}s floor for ${image}, so this cell is not cold:" >&2
    echo "${violations}" >&2
    mv "${csv}" "${csv%.csv}.failed-cold"
    echo "cold guard: kept the evidence at ${csv%.csv}.failed-cold" >&2
    return 1
  fi
}

rate_bytes_per_second() {
  local rate="$1"
  case "${rate}" in
    *gbit) awk -v r="${rate%gbit}" 'BEGIN { printf "%.0f", r * 1000000000 / 8 }' ;;
    *mbit) awk -v r="${rate%mbit}" 'BEGIN { printf "%.0f", r * 1000000 / 8 }' ;;
    *) echo "cold guard: unknown rate ${rate}" >&2; return 1 ;;
  esac
}
