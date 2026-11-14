#!/usr/bin/env bash
# Runs the prefetch experiment for plain nydusd on compressed and uncompressed images with the cluster cache as reference
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXPERIMENT="${REPO_ROOT}/scripts/experiments/benchmark-plain-prefetch.sh"
UNCOMPRESSED_IMAGES="fn-python-hello:8080 fn-python-ml:8080 fn-pytorch-cpu:8080"
COMPRESSED_IMAGES="fn-python-hello:8080 fn-pytorch-cpu:8080"

PARTS=(
  "plain nydusd, compressed|CONFIGS=2s MODES=off,all NYDUS_TAG=nydus-zstd IMAGES=${COMPRESSED_IMAGES// /,}"
  "plain nydusd, uncompressed|CONFIGS=2s MODES=off,all NYDUS_TAG=nydus-none IMAGES=${UNCOMPRESSED_IMAGES// /,}"
  "cluster cache reference|CONFIGS=4 MODES=off NYDUS_TAG=nydus-none IMAGES=${UNCOMPRESSED_IMAGES// /,}"
)

run_part() {
  local label="${1%%|*}" settings="${1#*|}" setting assignments=()
  shift
  for setting in ${settings}; do
    assignments+=("${setting//,/ }")
  done
  echo "### experiment: ${label}"
  env "${assignments[@]}" "$@" "${EXPERIMENT}"
}

for part in "${PARTS[@]}"; do
  run_part "${part}" DRY_RUN=1
done
if [[ -n "${DRY_RUN:-}" ]]; then
  exit 0
fi

for part in "${PARTS[@]}"; do
  run_part "${part}"
done
echo "### experiment: all parts complete"
