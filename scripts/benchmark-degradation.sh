#!/usr/bin/env bash
set -euo pipefail

# Tests the design goal that no failure of the cache may fail a cold start.
# The cache sits on the critical path of every cached cold start, so the claim
# that its worst case is the uncached system is worth measuring rather than
# arguing. Three failure modes are covered, each disturbing the cache in the
# window between the seed filling it and the other nodes asking for it:
#
#   empty        the cache is reachable but its contents are gone, so every
#                scale-out chunk misses and has to come from the registry
#   unreachable  the cache is scaled away entirely, so the proxy's circuit
#                breaker opens and every chunk goes to the registry
#   killed       the cache is deleted a few seconds into the scale-out, so the
#                cold starts lose it midway through depending on it
#
# The reference for all three is configuration 2, the uncached system. A mode
# that lands at or below it has kept the promise; a mode that lands above it
# has not.
#
# Usage: [MODES=..] [IMAGE=..] [RTT=..] [RATE=..] [REPS=..] [KILL_DELAY=..] benchmark-degradation.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
SCRATCH="$(mktemp -d)"

CELLS_RUN=0
CELLS_SKIPPED=0

# However a mode leaves the cache, the next one has to start from a healthy
# single replica, and so does whatever campaign runs after this one
restore_cache() {
  kubectl scale deploy "${CACHE_DEPLOYMENT}" --replicas=1 >/dev/null 2>&1 || true
  kubectl rollout status deploy/"${CACHE_DEPLOYMENT}" --timeout=180s >/dev/null 2>&1 || true
}

cleanup() {
  rm -rf "${SCRATCH}"
  restore_cache
  "${REPO_ROOT}/scripts/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "${OUTDIR}"

"${REPO_ROOT}/scripts/config-switch.sh" 4
"${REPO_ROOT}/scripts/registry-netem.sh" on "${RTT}ms" "${RATE}"

workload="${SCRATCH}/workload.yaml"
ACR="${REGISTRY}" SERVICE="${IMAGE}" TAG="nydus-none" PORT="${PORT}" \
  "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"

for mode in ${MODES}; do
  case "${mode}" in
    empty)
      # deleting the pod loses the in-memory store; the replacement comes back
      # reachable and empty, which is the state a restarted cache is in
      hook="kubectl delete pod -l app=${CACHE_DEPLOYMENT} --wait=true --timeout=120s >/dev/null 2>&1; \
            kubectl rollout status deploy/${CACHE_DEPLOYMENT} --timeout=180s >/dev/null 2>&1"
      ;;
    unreachable)
      hook="kubectl scale deploy ${CACHE_DEPLOYMENT} --replicas=0 >/dev/null 2>&1; \
            kubectl wait --for=delete pod -l app=${CACHE_DEPLOYMENT} --timeout=120s >/dev/null 2>&1 || true"
      ;;
    killed)
      # backgrounded so the scale-out starts with the cache still present and
      # loses it partway through
      hook="( sleep ${KILL_DELAY}; kubectl scale deploy ${CACHE_DEPLOYMENT} --replicas=0 >/dev/null 2>&1 ) &"
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
  if ! SERVICE="${IMAGE}" WORKLOAD="${workload}" PRE_SCALEOUT_HOOK="${hook}" \
      "${REPO_ROOT}/scripts/benchmark-scaleout.sh" 4 "${REPS}" "${csv}" "${NODES}"; then
    echo "### degradation: FAILED mode ${mode}"
    [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
  fi

  restore_cache
done

if (( CELLS_RUN == 0 )); then
  echo "### degradation measured nothing: all ${CELLS_SKIPPED} modes already had a CSV" >&2
  exit 1
fi

echo "### degradation complete: ${CELLS_RUN} modes measured, ${CELLS_SKIPPED} skipped"
