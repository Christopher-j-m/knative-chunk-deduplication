#!/usr/bin/env bash
set -euo pipefail

# Runs the whole prefetch campaign against stock nydusd in one go, in three
# parts: stock nydusd on the compressed conversion of the two images that have
# one, stock nydusd on the uncompressed conversion of three images, both with
# prefetch off and on, and the cluster cache with prefetch off as a reference
# measured the same day. The compressed part runs first because its images
# live only in the rig's registry, so their absence stops the run at once.
# Every part leaves the rig on config 4 with the link unshaped, and a rerun
# skips the cells that already have a CSV
#
# Usage: [DRY_RUN=1] [REPS=5] run-prefetch-stock-campaign.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CAMPAIGN="${REPO_ROOT}/scripts/benchmark-prefetch-sensitivity.sh"
UNCOMPRESSED_IMAGES="fn-python-hello:8080 fn-python-ml:8080 fn-pytorch-cpu:8080"
COMPRESSED_IMAGES="fn-python-hello:8080 fn-pytorch-cpu:8080"

PARTS=(
  "stock nydusd, compressed|CONFIGS=2s MODES=off,all NYDUS_TAG=nydus-zstd IMAGES=${COMPRESSED_IMAGES// /,}"
  "stock nydusd, uncompressed|CONFIGS=2s MODES=off,all NYDUS_TAG=nydus-none IMAGES=${UNCOMPRESSED_IMAGES// /,}"
  "cluster cache reference|CONFIGS=4 MODES=off NYDUS_TAG=nydus-none IMAGES=${UNCOMPRESSED_IMAGES// /,}"
)

# Runs one part with its settings; lists inside a setting are written with
# commas and handed to the campaign with spaces
run_part() {
  local label="${1%%|*}" settings="${1#*|}" setting assignments=()
  shift
  for setting in ${settings}; do
    assignments+=("${setting//,/ }")
  done
  echo "### campaign: ${label}"
  env "${assignments[@]}" "$@" "${CAMPAIGN}"
}

# Every part is planned before the first one measures, so a refused cell in
# the last part stops the campaign now rather than hours in
for part in "${PARTS[@]}"; do
  run_part "${part}" DRY_RUN=1
done
if [[ -n "${DRY_RUN:-}" ]]; then
  exit 0
fi

for part in "${PARTS[@]}"; do
  run_part "${part}"
done
echo "### campaign: all parts complete"
