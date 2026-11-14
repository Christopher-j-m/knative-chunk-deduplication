#!/usr/bin/env bash
# Slows the cluster cache down between the first start and the scale-out and records the delay it measured
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SLOW_DELAY_MS="${SLOW_DELAY_MS:?set SLOW_DELAY_MS to the delay in milliseconds}"
CACHE_IP="${CACHE_IP:-10.43.0.50:8080}"
SAMPLES="${SAMPLES:-7}"

# shellcheck source=slow-cache-lib.sh
source "${REPO_ROOT}/scripts/experiments/slow-cache-lib.sh"

timed_requests() {
  kubectl exec bench-probe -- sh -c "for i in \$(seq 1 ${SAMPLES}); do \
    curl -o /dev/null -s -m 5 -w '%{time_connect} %{time_starttransfer}\n' http://${CACHE_IP}/healthz; done"
}

before_lines="$(timed_requests)"
"${REPO_ROOT}/scripts/cluster/cache-netem.sh" on "${SLOW_DELAY_MS}ms" >/dev/null
after_lines="$(timed_requests)"

before_ms="$(median_answer_ms <<<"${before_lines}")"
after_ms="$(median_answer_ms <<<"${after_lines}")"
added_ms="$(awk -v a="${after_ms}" -v b="${before_ms}" 'BEGIN { printf "%.1f", a - b }')"
verified="yes"
delay_verified "${SLOW_DELAY_MS}" "${added_ms}" || verified="no"

echo "  slow   asked ${SLOW_DELAY_MS}ms, answers took ${before_ms}ms before and ${after_ms}ms after (verified=${verified})"
if [[ -n "${SLOW_RECORD:-}" ]]; then
  [[ -f "${SLOW_RECORD}" ]] || echo "rep,delay_ms,answer_ms_before,answer_ms_after,added_ms,verified" >"${SLOW_RECORD}"
  echo "${REPETITION:-},${SLOW_DELAY_MS},${before_ms},${after_ms},${added_ms},${verified}" >>"${SLOW_RECORD}"
fi

if [[ "${verified}" != "yes" ]]; then
  echo "slow-cache-hook: the cache answers ${added_ms}ms later, not the ${SLOW_DELAY_MS}ms asked for" >&2
  exit 1
fi
