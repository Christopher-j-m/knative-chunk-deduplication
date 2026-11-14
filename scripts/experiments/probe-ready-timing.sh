#!/usr/bin/env bash
# Prints for every pod of a Knative revision the ready time Kubernetes recorded and the readiness probe period
set -euo pipefail

SERVICE="${1:?usage: probe-ready-timing.sh <service> <config>}"
CONFIG="${2:?usage: probe-ready-timing.sh <service> <config>}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" readiness-timing)/readiness-timing.csv"

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

kubectl get pods -l "serving.knative.dev/service=${SERVICE}" \
  -o jsonpath='{range .items[0].spec.containers[*]}{.name}{": "}{.readinessProbe}{"\n"}{end}'
