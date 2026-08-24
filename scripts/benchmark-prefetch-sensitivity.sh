#!/usr/bin/env bash
set -euo pipefail

# Bounds how much of the cache's measured advantage over uncached lazy pulling
# comes from the cache and how much from a Nydus feature the campaigns turned
# off. Every published cell ran with filesystem prefetch disabled, which makes
# the uncached baseline slower than a stock deployment would be. This runs the
# same cells in both prefetch modes so the difference can be reported instead
# of argued.
#
# The corpus is converted without prefetch patterns, so the only mode that
# differs from "off" is whole-blob prefetch. That is also the strongest form
# the uncached baseline can take, which makes it the conservative comparison.
#
# Usage: [MODES=..] [CONFIGS=..] [IMAGES=..] [RTT=..] [RATE=..] [REPS=..] benchmark-prefetch-sensitivity.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODES="${MODES:-off all}"
CONFIGS="${CONFIGS:-2 4}"
IMAGES="${IMAGES:-fn-python-hello:8080 fn-python-ml:8080}"
RTT="${RTT:-80}"
RATE="${RATE:-600mbit}"
REPS="${REPS:-5}"
PREFIX="${PREFIX:-prefetch}"
REGISTRY="registry.localhost:5000"
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
SCRATCH="$(mktemp -d)"

CELLS_RUN=0
CELLS_SKIPPED=0

# Leave the rig in the published state whatever happens, so the next campaign
# does not inherit a prefetching snapshotter without noticing
restore() {
  rm -rf "${SCRATCH}"
  PREFETCH=off "${REPO_ROOT}/scripts/node-snapshotter.sh" install >/dev/null 2>&1 || true
  "${REPO_ROOT}/scripts/node-snapshotter.sh" stop >/dev/null 2>&1 || true
  "${REPO_ROOT}/scripts/node-snapshotter.sh" start >/dev/null 2>&1 || true
  "${REPO_ROOT}/scripts/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap restore EXIT

mapfile -t NODES < <("${REPO_ROOT}/scripts/cluster-nodes.sh")
if [[ "${#NODES[@]}" -eq 0 ]]; then
  echo "prefetch: no cluster nodes found; is the k3d cluster running?" >&2
  exit 1
fi

# The prefetch mode lives in a file on the nodes, and a bring-up run between
# two cells would quietly put the published mode back without failing
# anything. Every cell is therefore measured only after the nodes are seen to
# carry the mode under test
assert_prefetch_mode() {
  local wanted="$1" expected node actual
  case "${wanted}" in
    off) expected=false ;;
    all) expected=true ;;
    *) echo "prefetch: unknown mode ${wanted}" >&2; return 1 ;;
  esac

  for node in "${NODES[@]}"; do
    actual="$(docker exec "${node}" cat /etc/nydus/nydusd-config.json 2>/dev/null \
      | python3 -c 'import json,sys; print(str(json.load(sys.stdin)["fs_prefetch"]["prefetch_all"]).lower())' 2>/dev/null || echo unknown)"
    if [[ "${actual}" != "${expected}" ]]; then
      echo "prefetch: ${node} reports prefetch_all=${actual}, want ${expected} for mode ${wanted}" >&2
      return 1
    fi
  done
}

mkdir -p "${OUTDIR}"

"${REPO_ROOT}/scripts/registry-netem.sh" on "${RTT}ms" "${RATE}"

for mode in ${MODES}; do
  echo "### prefetch: installing the nodes with prefetch ${mode}"
  PREFETCH="${mode}" "${REPO_ROOT}/scripts/node-snapshotter.sh" install
  "${REPO_ROOT}/scripts/node-snapshotter.sh" stop
  "${REPO_ROOT}/scripts/node-snapshotter.sh" start
  "${REPO_ROOT}/scripts/node-snapshotter.sh" status

  for config in ${CONFIGS}; do
    echo "### prefetch: switching to config ${config}"
    "${REPO_ROOT}/scripts/config-switch.sh" "${config}"

    # A configuration switch cycles the nodes and restarts the snapshotter,
    # so the mode under test is re-checked on the far side of every switch
    assert_prefetch_mode "${mode}"

    for entry in ${IMAGES}; do
      image="${entry%%:*}"
      port="${entry##*:}"

      if [[ -z "${FORCE:-}" ]] && ls "${OUTDIR}/${PREFIX}-config${config}-${image}-prefetch${mode}-rtt${RTT}ms-rate${RATE}-"*.csv >/dev/null 2>&1; then
        echo "### prefetch: skip config ${config} ${image} prefetch ${mode} (CSV exists)"
        CELLS_SKIPPED=$((CELLS_SKIPPED + 1))
        continue
      fi
      CELLS_RUN=$((CELLS_RUN + 1))

      workload="${SCRATCH}/workload-${config}-${image}.yaml"
      ACR="${REGISTRY}" SERVICE="${image}" TAG="nydus-none" PORT="${port}" \
        "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"

      csv="${OUTDIR}/${PREFIX}-config${config}-${image}-prefetch${mode}-rtt${RTT}ms-rate${RATE}-$(date +%Y%m%dT%H%M%S).csv"
      if ! SERVICE="${image}" WORKLOAD="${workload}" \
          "${REPO_ROOT}/scripts/benchmark-scaleout.sh" "${config}" "${REPS}" "${csv}"; then
        echo "### prefetch: FAILED config ${config} ${image} prefetch ${mode}"
        [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
        kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      fi
    done
  done
done

if (( CELLS_RUN == 0 )); then
  echo "### prefetch campaign measured nothing: all ${CELLS_SKIPPED} cells already had a CSV" >&2
  echo "### move them aside, or rerun with FORCE=1 to overwrite" >&2
  exit 1
fi

echo "### prefetch campaign complete: ${CELLS_RUN} cells measured, ${CELLS_SKIPPED} skipped"
