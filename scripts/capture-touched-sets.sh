#!/usr/bin/env bash
set -euo pipefail

# Captures the exact set of chunks each corpus image's cold start brings into
# the cluster cache. For every image: reset the cluster, cold-start the
# service once, then read the cache's chunk listing -- after a reset the cache
# holds precisely the chunks that one cold start touched. The output uses the
# same schema as chunks-startup.json so the simulator loads it unchanged.
#
# Defaults target the local k3d rig; set REGISTRY, RESET_SCRIPT and CACHE_IP
# to run the same capture against AKS.
#
# Usage: [REGISTRY=<login-server>] capture-touched-sets.sh [outfile]
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REGISTRY="${REGISTRY:-registry.localhost:5000}"
RESET_SCRIPT="${RESET_SCRIPT:-${REPO_ROOT}/scripts/cluster-reset.sh}"
CACHE_IP="${CACHE_IP:-10.43.0.50:8080}"
PORT="${PORT:-8080}"
OUTFILE="${1:-${REPO_ROOT}/analysis/results/chunks-live-touched.json}"
IMAGES=(fn-go-hello fn-python-hello fn-java-springboot fn-node-api fn-python-ml fn-pytorch-cpu)

# A long-lived pod does the reading, so each capture is one exec rather than a
# fresh pod launch. The reset deletes it along with everything else, so this
# runs again after every reset
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

  # min-scale 1 forces the single cold start this capture is about, instead of
  # waiting for a request to trigger one
  ACR="${REGISTRY}" SERVICE="${service}" TAG="nydus-none" PORT="${PORT}" \
    "${REPO_ROOT}/scripts/render-workload.sh" \
    | sed 's|autoscaling.knative.dev/min-scale: "0"|autoscaling.knative.dev/min-scale: "1"|' \
    >"${workload}"
  kubectl apply -f "${workload}" >/dev/null
  kubectl wait ksvc "${service}" --for=condition=Ready --timeout=420s >/dev/null

  # every chunk now stored is one this cold start fetched through the cache
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
