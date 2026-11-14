#!/usr/bin/env bash
# Runs plain nydusd at its default settings next to the other conversions and the cluster cache in one run
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXPERIMENT="${REPO_ROOT}/scripts/experiments/benchmark-plain-prefetch.sh"
REMAINING_IMAGES="${EXPERIMENT_IMAGES:-fn-go-hello:8080 fn-java-springboot:8080 fn-node-api:8080 fn-python-ml:8080}"

CONTROL_IMAGES="${CONTROL_IMAGES:-${REMAINING_IMAGES}}"

REPEAT_IMAGE="${REPEAT_IMAGE-fn-python-hello:8080}"
COMPRESSED_IMAGES="${REPEAT_IMAGE:+${REPEAT_IMAGE} }${REMAINING_IMAGES}"

export PREFIX="${PREFIX:-plain-prefetch-zstd100k}"
export LIMIT="${LIMIT:-100000}"

case "${PREFIX}" in
  plain-prefetch|plain-prefetch100k|plain-prefetch-uncompressed100k)
    echo "experiment: prefix ${PREFIX} holds published cells; choose another one" >&2
    exit 1
    ;;
esac

PARTS=(
  "plain nydusd, uncompressed|CONFIGS=2s MODES=off,all NYDUS_TAG=nydus-none IMAGES=${CONTROL_IMAGES// /,}"
  "plain nydusd, compressed|CONFIGS=2s MODES=off,all NYDUS_TAG=nydus-zstd IMAGES=${COMPRESSED_IMAGES// /,}"
  "cluster cache reference|CONFIGS=4 MODES=off NYDUS_TAG=nydus-none IMAGES=${REMAINING_IMAGES// /,}"
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

part_has_work() {
  local plan
  plan="$(run_part "$1" DRY_RUN=1)"
  grep -q '^plan: measure ' <<<"${plan}"
}

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
    echo "### experiment: ${part%%|*} already measured"
  fi
done
echo "### experiment: all parts complete"
