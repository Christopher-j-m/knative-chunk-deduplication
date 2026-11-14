#!/usr/bin/env bash
# Records which chunks one cold start of each corpus image brings into the cluster cache
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
REGISTRY="${REGISTRY:-registry.localhost:5000}"
RESET_SCRIPT="${RESET_SCRIPT:-${REPO_ROOT}/scripts/cluster/cluster-reset.sh}"
CACHE_IP="${CACHE_IP:-10.43.0.50:8080}"
PORT="${PORT:-8080}"
OUTFILE="${1:-${REPO_ROOT}/analysis/results/chunks-live-touched.json}"
IMAGES=(fn-go-hello fn-python-hello fn-java-springboot fn-node-api fn-python-ml fn-pytorch-cpu)

ensure_probe_pod() {
  if ! kubectl get pod bench-probe >/dev/null 2>&1; then
    kubectl run bench-probe --image=curlimages/curl --restart=Never \
      --command -- sleep infinity >/dev/null
  fi
  kubectl wait pod/bench-probe --for=condition=Ready --timeout=120s >/dev/null
}

capture_one() {
  local service="$1" workload
  workload="$(mktemp)"

  SERVICE="${service}" "${RESET_SCRIPT}" >/dev/null
  ensure_probe_pod

  ACR="${REGISTRY}" SERVICE="${service}" TAG="nydus-none" PORT="${PORT}" \
    "${REPO_ROOT}/scripts/cluster/render-workload.sh" \
    | sed 's|autoscaling.knative.dev/min-scale: "0"|autoscaling.knative.dev/min-scale: "1"|' \
    >"${workload}"
  kubectl apply -f "${workload}" >/dev/null
  kubectl wait ksvc "${service}" --for=condition=Ready --timeout=420s >/dev/null

  kubectl exec bench-probe -- curl -s --max-time 30 "http://${CACHE_IP}/debug/chunks"

  kubectl delete ksvc "${service}" --ignore-not-found --timeout=120s >/dev/null
  rm -f "${workload}"
}

mkdir -p "$(dirname "${OUTFILE}")"
echo "[" >"${OUTFILE}.tmp"
first=1
for image in "${IMAGES[@]}"; do
  echo "capturing ${image}" >&2
  listing="$(capture_one "${image}")"
  count="$(echo "${listing}" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"
  echo "  ${image}: ${count} chunks" >&2
  [[ "${first}" = 1 ]] || echo "," >>"${OUTFILE}.tmp"
  first=0
  printf '{"image": "%s", "chunks": %s}' "${image}" "${listing}" >>"${OUTFILE}.tmp"
done
echo "]" >>"${OUTFILE}.tmp"
python3 -m json.tool "${OUTFILE}.tmp" >"${OUTFILE}"
rm -f "${OUTFILE}.tmp"
echo "wrote ${OUTFILE}"
