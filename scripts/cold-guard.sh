#!/usr/bin/env bash
# Shared check that a configuration-1 cell really pulled its image across the
# shaped link. Sourced by the sweep drivers; expects REGISTRY to be set.

# What actually crosses the wire for an eager pull, measured from the registry
# manifests. The thesis size ladder is uncompressed on-disk size and is roughly
# four times larger, so it cannot be used to reason about transfer time
compressed_layer_bytes() {
  local image="$1"

  # asked of the registry rather than kept in a table here, because a table
  # goes stale the moment the corpus is rebuilt and, worse, silently has no
  # answer for an image nobody added to it -- which aborts the whole sweep
  # on an image that measured perfectly well
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

# Defence in depth for the configuration that has to pay the registry. The reset
# gates coldness before the measurement; this re-checks it afterwards from the
# measurement itself, because during development a run once produced warm pulls
# while the reset's own gate reported clean and that was never explained. A pull
# far below what the shaped link allows did not cross the wire, whatever the
# reset believed. Pods that land back on the seed node are marked warm and are
# legitimately near zero, so only the cold rows are held to the floor
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

# Converts a netem rate such as 600mbit or 10gbit into bytes per second
rate_bytes_per_second() {
  local rate="$1"
  case "${rate}" in
    *gbit) awk -v r="${rate%gbit}" 'BEGIN { printf "%.0f", r * 1000000000 / 8 }' ;;
    *mbit) awk -v r="${rate%mbit}" 'BEGIN { printf "%.0f", r * 1000000 / 8 }' ;;
    *) echo "cold guard: unknown rate ${rate}" >&2; return 1 ;;
  esac
}
