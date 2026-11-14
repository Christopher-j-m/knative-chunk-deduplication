#!/usr/bin/env bash
# Measures how many chunks a second image finds in the cache after a different image filled it on another node
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SERVICE_A="${1:?usage: ACR=<acr> benchmark-cross-image.sh <service-a> <service-b> <reps> [outfile]}"
SERVICE_B="${2:?usage: ACR=<acr> benchmark-cross-image.sh <service-a> <service-b> <reps> [outfile]}"
REPS="${3:?usage: ACR=<acr> benchmark-cross-image.sh <service-a> <service-b> <reps> [outfile]}"
OUTFILE="${4:-$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" aks-cross-image)/aks-cross-image-${SERVICE_A}-${SERVICE_B}-$(date +%Y%m%dT%H%M%S).csv}"

ACR="${ACR:?set ACR to the ACR login server}"
CACHE_IP="${CACHE_IP:-10.0.0.50:8080}"
RESET_SCRIPT="${RESET_SCRIPT:-${REPO_ROOT}/scripts/cluster/cluster-reset-aks.sh}"
PORT="${PORT:-8080}"
READY_TIMEOUT=420

proxy_args="$(kubectl -n kube-system get ds chunk-proxy -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)"
if [[ "${proxy_args}" != *"--cache-enabled=true"* ]]; then
  echo "chunk-proxy runs ${proxy_args} but cross-image reuse needs config 4; run config-switch first" >&2
  exit 1
fi

WORKLOAD_A="$(mktemp)"
WORKLOAD_B="$(mktemp)"
trap 'rm -f "${WORKLOAD_A}" "${WORKLOAD_B}"' EXIT
ACR="${ACR}" SERVICE="${SERVICE_A}" TAG="nydus-none" PORT="${PORT}" \
  "${REPO_ROOT}/scripts/cluster/render-workload.sh" >"${WORKLOAD_A}"
ACR="${ACR}" SERVICE="${SERVICE_B}" TAG="nydus-none" PORT="${PORT}" \
  "${REPO_ROOT}/scripts/cluster/render-workload.sh" >"${WORKLOAD_B}"

ensure_probe_pod() {
  if ! kubectl get pod bench-probe >/dev/null 2>&1; then
    kubectl run bench-probe --image=curlimages/curl --restart=Never \
      --command -- sleep infinity >/dev/null
  fi
  kubectl wait pod/bench-probe --for=condition=Ready --timeout=120s >/dev/null
}

cache_metric() {
  kubectl exec bench-probe -- curl -s --max-time 5 "http://${CACHE_IP}/metrics" 2>/dev/null \
    | awk -v k="$1" '$1==k || index($1, k"{")==1 {v=$NF} END {print v+0}'
}

pull_seconds() {
  local pod="$1" service="$2" message duration
  message="$(kubectl get events --field-selector "involvedObject.name=${pod},reason=Pulled" \
    -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' | grep -F "${service}" | head -1)"
  if [[ -z "${message}" ]] || [[ "${message}" == *"already present"* ]]; then
    echo 0
    return
  fi
  duration="$(echo "${message}" | sed -n 's/.* in \([0-9.]*m\{0,1\}s\).*/\1/p')"
  case "${duration}" in
    *ms) awk -v ms="${duration%ms}" 'BEGIN{printf "%.3f", ms/1000}' ;;
    *s) echo "${duration%s}" ;;
    *) echo "" ;;
  esac
}

deploy_and_time() {
  local workload="$1" service="$2" start revision deadline pod status
  start="$(date +%s.%N)"
  sed 's|autoscaling.knative.dev/min-scale: "0"|autoscaling.knative.dev/min-scale: "1"|' \
    "${workload}" | kubectl apply -f - >/dev/null 2>&1
  revision=""
  until [[ -n "${revision}" ]]; do
    revision="$(kubectl get ksvc "${service}" -o jsonpath='{.status.latestCreatedRevisionName}' 2>/dev/null)"
    sleep 0.2
  done
  deadline=$((SECONDS + READY_TIMEOUT))
  while true; do
    if (( SECONDS > deadline )); then
      echo "timed out waiting for a ready pod of ${revision}" >&2
      return 1
    fi
    while read -r pod status; do
      if [[ "${status}" == "True" ]]; then
        echo "${pod} $(awk -v a="$(date +%s.%N)" -v b="${start}" 'BEGIN{printf "%.3f", a-b}')"
        return 0
      fi
    done < <(kubectl get pods -l "serving.knative.dev/revision=${revision}" \
      -o jsonpath='{range .items[*]}{.metadata.name} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null)
    sleep 0.2
  done
}

node_of_pod() {
  kubectl get pod "$1" -o jsonpath='{.spec.nodeName}'
}

measure_phase() {
  local phase="$1" workload="$2" service="$3" hits_before misses_before ready pod node hits misses
  hits_before="$(cache_metric cache_hits_total)"
  misses_before="$(cache_metric cache_misses_total)"
  ready="$(deploy_and_time "${workload}" "${service}")"
  pod="${ready%% *}"
  node="$(node_of_pod "${pod}")"
  hits="$(awk -v a="$(cache_metric cache_hits_total)" -v b="${hits_before}" 'BEGIN{print a-b}')"
  misses="$(awk -v a="$(cache_metric cache_misses_total)" -v b="${misses_before}" 'BEGIN{print a-b}')"
  echo "${SERVICE_A}+${SERVICE_B},${rep},${phase},${service},${node},${ready#* },$(pull_seconds "${pod}" "${service}"),${hits},${misses}" >>"${OUTFILE}"
  echo "  ${phase} service=${service} node=${node} ready=${ready#* }s hits+=${hits} misses+=${misses}"
  LAST_NODE="${node}"
}

mkdir -p "$(dirname "${OUTFILE}")"
echo "pair,rep,phase,service,node,ready_seconds,pull_seconds,cache_hits_delta,cache_misses_delta" >"${OUTFILE}"
echo "Writing results to ${OUTFILE}"

for rep in $(seq 1 "${REPS}"); do
  echo "=== cross-image ${SERVICE_A} -> ${SERVICE_B} rep ${rep}/${REPS}"

  kubectl delete ksvc "${SERVICE_B}" --ignore-not-found --timeout=120s >/dev/null
  kubectl wait --for=delete pod -l "serving.knative.dev/service=${SERVICE_B}" --timeout=120s 2>/dev/null || true
  SERVICE="${SERVICE_A}" "${RESET_SCRIPT}" >/dev/null
  kubectl delete pod bench-probe --ignore-not-found --timeout=60s >/dev/null
  ensure_probe_pod

  measure_phase seed "${WORKLOAD_A}" "${SERVICE_A}"
  seed_node="${LAST_NODE}"

  kubectl cordon "${seed_node}" >/dev/null
  trap 'kubectl uncordon "${seed_node}" >/dev/null 2>&1 || true; rm -f "${WORKLOAD_A}" "${WORKLOAD_B}"' EXIT
  measure_phase cross "${WORKLOAD_B}" "${SERVICE_B}"
  kubectl uncordon "${seed_node}" >/dev/null

  if [[ "${LAST_NODE}" == "${seed_node}" ]]; then
    echo "service B landed on the seed node despite the cordon; repetition ${rep} is invalid" >&2
    exit 1
  fi
done

echo "Done: ${OUTFILE}"
