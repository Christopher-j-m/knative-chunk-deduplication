#!/usr/bin/env bash
set -euo pipefail

# Runs stock nydusd the way a stock deployment runs it, on the zstd
# conversion with prefetch on, next to everything needed to read that cell:
# both conversions with prefetch off and on, and the cluster cache as the
# reference for the lead, all measured in one session. The default images are
# the four that had no compressed cells, at the campaign's 80 ms; RTT and
# CAMPAIGN_IMAGES move it to another distance or image list. Every part
# leaves the rig on config 4 with the link unshaped, and a rerun measures
# only the cells without a CSV
#
# Usage: [DRY_RUN=1] [REPS=5] [RTT=80] [PREFIX=..] [CAMPAIGN_IMAGES=..] [CONTROL_IMAGES=..]
#        [ANCHOR_IMAGE=..] run-stock-defaults-campaign.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CAMPAIGN="${REPO_ROOT}/scripts/benchmark-prefetch-sensitivity.sh"
REMAINING_IMAGES="${CAMPAIGN_IMAGES:-fn-go-hello:8080 fn-java-springboot:8080 fn-node-api:8080 fn-python-ml:8080}"

# The uncompressed stock cells tie this session to the published rows; a
# shorter list measures fewer of them
CONTROL_IMAGES="${CONTROL_IMAGES:-${REMAINING_IMAGES}}"

# One published zstd image is measured again so the new compressed rows can
# be held against the published ones; an empty value leaves it out
ANCHOR_IMAGE="${ANCHOR_IMAGE-fn-python-hello:8080}"
COMPRESSED_IMAGES="${ANCHOR_IMAGE:+${ANCHOR_IMAGE} }${REMAINING_IMAGES}"

# Prefetching nodes overflow the emulator's default queue, so the published
# cells ran with a deep one and these do too
export PREFIX="${PREFIX:-prefetch-zstd100k}"
export LIMIT="${LIMIT:-100000}"

# A published prefix would make the benchmark skip cells and the summary
# replace published rows with these
case "${PREFIX}" in
  prefetch-stock|prefetch-deepqueue100k|prefetch-ladder100k)
    echo "campaign: prefix ${PREFIX} holds published cells; choose another one" >&2
    exit 1
    ;;
esac

PARTS=(
  "stock nydusd, uncompressed|CONFIGS=2s MODES=off,all NYDUS_TAG=nydus-none IMAGES=${CONTROL_IMAGES// /,}"
  "stock nydusd, compressed|CONFIGS=2s MODES=off,all NYDUS_TAG=nydus-zstd IMAGES=${COMPRESSED_IMAGES// /,}"
  "cluster cache reference|CONFIGS=4 MODES=off NYDUS_TAG=nydus-none IMAGES=${REMAINING_IMAGES// /,}"
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

# A part whose cells all exist would stop with "measured nothing", so a rerun
# has to know which parts still have work
part_has_work() {
  local plan
  plan="$(run_part "$1" DRY_RUN=1)"
  grep -q '^plan: measure ' <<<"${plan}"
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
  if part_has_work "${part}"; then
    run_part "${part}"
  else
    echo "### campaign: ${part%%|*} already measured"
  fi
done
echo "### campaign: all parts complete"
