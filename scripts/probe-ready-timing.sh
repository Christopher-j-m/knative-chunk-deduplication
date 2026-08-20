#!/usr/bin/env bash
set -euo pipefail

# Reads two independent ready times for every pod of one Knative revision:
# the one Kubernetes recorded itself, and the readiness probe period that
# governs how coarsely it could have been recorded, since a pod whose
# Kubernetes ready time is a whole probe period later than the container's
# own readiness is a pod whose measurement is quantized rather than slow
#
# Usage: probe-ready-timing.sh <service> <config>
SERVICE="${1:?usage: probe-ready-timing.sh <service> <config>}"
CONFIG="${2:?usage: probe-ready-timing.sh <service> <config>}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${REPO_ROOT}/analysis/results/benchmarks/envelope-diagnostic.csv"

[[ -f "${OUT}" ]] || echo "pod,node,config,harness_ready_s,k8s_ready_s,probe_period_s,mode" >"${OUT}"

for pod in $(kubectl get pods -l "serving.knative.dev/service=${SERVICE}" -o name); do
  name="${pod#pod/}"
  node="$(kubectl get "${pod}" -o jsonpath='{.spec.nodeName}')"
  start="$(kubectl get "${pod}" -o jsonpath='{.status.startTime}')"
  ready_status="$(kubectl get "${pod}" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  ready="$(kubectl get "${pod}" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].lastTransitionTime}')"
  period="$(kubectl get "${pod}" \
    -o jsonpath='{.spec.containers[*].readinessProbe.periodSeconds}')"

  # A pod caught mid-teardown can carry a Ready condition that has already
  # flipped back to False, and its lastTransitionTime then belongs to that
  # later flip rather than to the cold start we are trying to measure, so
  # only a condition currently reading True is trustworthy
  if [[ "${ready_status}" == "True" ]]; then
    elapsed="$(python3 - "${start}" "${ready}" <<'PY'
import datetime, sys
parse = lambda value: datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
print(f"{(parse(sys.argv[2]) - parse(sys.argv[1])).total_seconds():.3f}")
PY
)"
  else
    elapsed=""
  fi

  echo "${name},${node},${CONFIG},,${elapsed},${period:-none},"  >>"${OUT}"
  echo "${name} on ${node}: kubernetes ready after ${elapsed:-unknown}s, probe periods '${period:-none}'"
done

# The full injected probe specification, which is what decides whether a
# ten-second offset can be an artifact at all
kubectl get pods -l "serving.knative.dev/service=${SERVICE}" \
  -o jsonpath='{range .items[0].spec.containers[*]}{.name}{": "}{.readinessProbe}{"\n"}{end}'
