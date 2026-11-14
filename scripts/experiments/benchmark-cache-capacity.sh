#!/usr/bin/env bash
# Checks the simulator's capacity predictions on the live cluster by capping the cache size and alternating cold starts of two images
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SERVICE_A="${1:?usage: ACR=<acr> benchmark-cache-capacity.sh <service-a> <service-b> <capacity-bytes> <reps> [outfile]}"
SERVICE_B="${2:?usage: ACR=<acr> benchmark-cache-capacity.sh <service-a> <service-b> <capacity-bytes> <reps> [outfile]}"
CAPACITY_BYTES="${3:?capacity in bytes, 0 means unbounded}"
REPS="${4:?number of repetitions}"
OUTFILE="${5:-$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" aks-cache-capacity)/aks-cache-capacity-${CAPACITY_BYTES}-${SERVICE_A}-${SERVICE_B}-$(date +%Y%m%dT%H%M%S).csv}"

ACR="${ACR:?set ACR to the ACR login server}"
CACHE_IP="${CACHE_IP:-10.0.0.50:8080}"
RESET_SCRIPT="${RESET_SCRIPT:-${REPO_ROOT}/scripts/cluster/cluster-reset-aks.sh}"
PORT="${PORT:-8080}"
READY_TIMEOUT=420

proxy_args="$(kubectl -n kube-system get ds chunk-proxy -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)"
if [[ "${proxy_args}" != *"--cache-enabled=true"* ]]; then
  echo "chunk-proxy runs ${proxy_args} but capacity validation needs config 4; run config-switch first" >&2
  exit 1
fi

set_capacity() {
  local args
  args="$(kubectl get deploy nydus-cluster-cache -o json \
    | python3 -c "
import json, sys
deploy = json.load(sys.stdin)
args = deploy['spec']['template']['spec']['containers'][0]['args']
args = [a for a in args if not a.startswith('--capacity-bytes=')]
args.append('--capacity-bytes=${CAPACITY_BYTES}')
print(json.dumps(args))
")"
  kubectl patch deploy nydus-cluster-cache --type=json \
    -p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":${args}}]" >/dev/null
  kubectl rollout status deploy/nydus-cluster-cache --timeout=180s >/dev/null
}

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
  local body
  for _ in $(seq 1 30); do
    if body="$(kubectl exec bench-probe -- curl -s --max-time 5 "http://${CACHE_IP}/metrics" 2>/dev/null)"; then
      awk -v k="$1" '$1==k || index($1, k"{")==1 {v=$NF} END {print v+0}' <<<"${body}"
      return 0
    fi
    sleep 2
  done
  echo "cache metrics unreachable at ${CACHE_IP}" >&2
  return 1
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
  local phase="$1" workload="$2" service="$3"
  local hits_before misses_before evict_ref stored ready pod node hits misses
  hits_before="$(cache_metric cache_hits_total)"
  misses_before="$(cache_metric cache_misses_total)"
  ready="$(deploy_and_time "${workload}" "${service}")"
  pod="${ready%% *}"
  node="$(node_of_pod "${pod}")"
  hits="$(awk -v a="$(cache_metric cache_hits_total)" -v b="${hits_before}" 'BEGIN{print a-b}')"
  misses="$(awk -v a="$(cache_metric cache_misses_total)" -v b="${misses_before}" 'BEGIN{print a-b}')"
  stored="$(cache_metric cache_stored_bytes)"
  echo "${SERVICE_A}+${SERVICE_B},${CAPACITY_BYTES},${rep},${phase},${service},${node},${ready#* },$(pull_seconds "${pod}" "${service}"),${hits},${misses},${stored}" >>"${OUTFILE}"
  echo "  ${phase} service=${service} node=${node} ready=${ready#* }s hits+=${hits} misses+=${misses} stored=${stored}"
  LAST_NODE="${node}"
}

mkdir -p "$(dirname "${OUTFILE}")"
echo "pair,capacity_bytes,rep,phase,service,node,ready_seconds,pull_seconds,cache_hits_delta,cache_misses_delta,cache_stored_bytes" >"${OUTFILE}"
echo "Writing results to ${OUTFILE}"

echo "setting cache capacity to ${CAPACITY_BYTES} bytes"
set_capacity

for rep in $(seq 1 "${REPS}"); do
  echo "=== cache capacity ${SERVICE_A} / ${SERVICE_B} capacity=${CAPACITY_BYTES} rep ${rep}/${REPS}"

  kubectl delete ksvc "${SERVICE_B}" --ignore-not-found --timeout=120s >/dev/null
  kubectl wait --for=delete pod -l "serving.knative.dev/service=${SERVICE_B}" --timeout=120s 2>/dev/null || true
  reset_ok=""
  for _ in 1 2 3; do
    if SERVICE="${SERVICE_A}" "${RESET_SCRIPT}" >/dev/null; then
      reset_ok=1
      break
    fi
    echo "  cluster reset failed, retrying" >&2
    sleep 10
  done
  [[ -n "${reset_ok}" ]] || { echo "cluster reset failed three times" >&2; exit 1; }
  kubectl delete pod bench-probe --ignore-not-found --timeout=60s >/dev/null
  ensure_probe_pod

  kubectl rollout restart deploy/nydus-cluster-cache >/dev/null
  kubectl rollout status deploy/nydus-cluster-cache --timeout=180s >/dev/null

  cordoned=()
  restore_nodes() {
    local n
    for n in "${cordoned[@]:-}"; do
      [[ -n "${n}" ]] && kubectl uncordon "${n}" >/dev/null 2>&1 || true
    done
  }
  trap 'restore_nodes; rm -f "${WORKLOAD_A}" "${WORKLOAD_B}"' EXIT

  measure_phase seed "${WORKLOAD_A}" "${SERVICE_A}"
  cordoned+=("${LAST_NODE}")
  kubectl cordon "${LAST_NODE}" >/dev/null

  measure_phase cross "${WORKLOAD_B}" "${SERVICE_B}"
  cordoned+=("${LAST_NODE}")
  kubectl cordon "${LAST_NODE}" >/dev/null

  kubectl delete ksvc "${SERVICE_A}" --ignore-not-found --timeout=120s >/dev/null
  kubectl wait --for=delete pod -l "serving.knative.dev/service=${SERVICE_A}" --timeout=120s 2>/dev/null || true
  measure_phase alt "${WORKLOAD_A}" "${SERVICE_A}"

  restore_nodes
  cordoned=()
done

echo "Done: ${OUTFILE}"
