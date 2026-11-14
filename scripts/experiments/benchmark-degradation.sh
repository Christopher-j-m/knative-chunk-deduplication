#!/usr/bin/env bash
# Measures cold starts when the cache is emptied, unreachable or killed between the first start and the scale-out
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODES="${MODES:-empty unreachable killed}"
IMAGE="${IMAGE:-fn-python-hello}"
PORT="${PORT:-8080}"
RTT="${RTT:-80}"
RATE="${RATE:-600mbit}"
REPS="${REPS:-5}"
NODES="${NODES:-3}"
KILL_DELAY="${KILL_DELAY:-3}"
PREFIX="${PREFIX:-degradation}"
REGISTRY="registry.localhost:5000"
CACHE_DEPLOYMENT="${CACHE_DEPLOYMENT:-nydus-cluster-cache}"
OUTDIR="$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" "${PREFIX}")"
SCRATCH="$(mktemp -d)"

CELLS_RUN=0
CELLS_SKIPPED=0

restore_cache() {
  kubectl scale deploy "${CACHE_DEPLOYMENT}" --replicas=1 >/dev/null 2>&1 || true
  kubectl rollout status deploy/"${CACHE_DEPLOYMENT}" --timeout=180s >/dev/null 2>&1 || true
}

cleanup() {
  rm -rf "${SCRATCH}"
  "${REPO_ROOT}/scripts/cluster/cache-netem.sh" off >/dev/null 2>&1 || true
  restore_cache
  "${REPO_ROOT}/scripts/cluster/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "${OUTDIR}"

# shellcheck source=slow-cache-lib.sh
source "${REPO_ROOT}/scripts/experiments/slow-cache-lib.sh"

"${REPO_ROOT}/scripts/cluster/config-switch.sh" 4
"${REPO_ROOT}/scripts/cluster/registry-netem.sh" on "${RTT}ms" "${RATE}"

workload="${SCRATCH}/workload.yaml"
ACR="${REGISTRY}" SERVICE="${IMAGE}" TAG="nydus-none" PORT="${PORT}" \
  "${REPO_ROOT}/scripts/cluster/render-workload.sh" >"${workload}"

for mode in ${MODES}; do
  post_hook=""
  case "${mode}" in
    empty)
      hook="kubectl delete pod -l app=${CACHE_DEPLOYMENT} --wait=true --timeout=120s >/dev/null 2>&1; \
            kubectl rollout status deploy/${CACHE_DEPLOYMENT} --timeout=180s >/dev/null 2>&1"
      ;;
    unreachable)
      hook="kubectl scale deploy ${CACHE_DEPLOYMENT} --replicas=0 >/dev/null 2>&1; \
            kubectl wait --for=delete pod -l app=${CACHE_DEPLOYMENT} --timeout=120s >/dev/null 2>&1 || true"
      ;;
    killed)
      hook="( sleep ${KILL_DELAY}; kubectl scale deploy ${CACHE_DEPLOYMENT} --replicas=0 >/dev/null 2>&1 ) &"
      ;;
    slow*)
      if ! delay_ms="$(slow_mode_delay_ms "${mode}")"; then
        echo "degradation: unknown mode ${mode}" >&2
        exit 1
      fi
      hook="SLOW_DELAY_MS=${delay_ms} SLOW_RECORD=${OUTDIR}/${PREFIX}-${mode}-${IMAGE}-rtt${RTT}ms-rate${RATE}.delay.csv \
            ${REPO_ROOT}/scripts/experiments/slow-cache-hook.sh"
      post_hook="${REPO_ROOT}/scripts/cluster/cache-netem.sh off"
      ;;
    *)
      echo "degradation: unknown mode ${mode}" >&2
      exit 1
      ;;
  esac

  if [[ -z "${FORCE:-}" ]] && ls "${OUTDIR}/${PREFIX}-${mode}-${IMAGE}-rtt${RTT}ms-rate${RATE}-"*.csv >/dev/null 2>&1; then
    echo "### degradation: skip ${mode} (CSV exists)"
    CELLS_SKIPPED=$((CELLS_SKIPPED + 1))
    continue
  fi
  CELLS_RUN=$((CELLS_RUN + 1))

  echo "### degradation: mode ${mode}"
  restore_cache

  csv="${OUTDIR}/${PREFIX}-${mode}-${IMAGE}-rtt${RTT}ms-rate${RATE}-$(date +%Y%m%dT%H%M%S).csv"
  if ! SERVICE="${IMAGE}" WORKLOAD="${workload}" PRE_SCALEOUT_HOOK="${hook}" POST_SCALEOUT_HOOK="${post_hook}" \
      "${REPO_ROOT}/scripts/experiments/benchmark-scale-out.sh" 4 "${REPS}" "${csv}" "${NODES}"; then
    echo "### degradation: FAILED mode ${mode}"
    [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
  fi

  "${REPO_ROOT}/scripts/cluster/cache-netem.sh" off >/dev/null 2>&1 || true
  restore_cache
done

if (( CELLS_RUN == 0 )); then
  echo "### degradation measured nothing: all ${CELLS_SKIPPED} modes already had a CSV" >&2
  exit 1
fi

echo "### degradation complete: ${CELLS_RUN} modes measured, ${CELLS_SKIPPED} skipped"
