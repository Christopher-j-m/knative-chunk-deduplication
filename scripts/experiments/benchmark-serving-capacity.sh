#!/usr/bin/env bash
# Measures how many chunk reads per second the single cache pod serves as the number of parallel requesters grows
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHUNK_SIZES="${CHUNK_SIZES:-32768 262144}"
CHUNKS="${CHUNKS:-512}"
CONCURRENCY="${CONCURRENCY:-1,2,4,8,16,32,64,128,256}"
DURATION="${DURATION:-10s}"
CACHE_ADDR="${CACHE_ADDR:-http://10.43.0.50:8080}"
IMAGE="${IMAGE:-registry.localhost:5000/nydus-cluster-cache:0.1.0}"
CACHE_DEPLOYMENT="${CACHE_DEPLOYMENT:-nydus-cluster-cache}"
CACHE_NAMESPACE="${CACHE_NAMESPACE:-default}"
OUTDIR="$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" serving-capacity)"
POD=cache-load-generator

mkdir -p "${OUTDIR}"

cleanup() {
  kubectl delete pod "${POD}" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
}
trap cleanup EXIT

ensure_probe_pod() {
  if ! kubectl get pod bench-probe >/dev/null 2>&1; then
    kubectl run bench-probe --image=curlimages/curl --restart=Never \
      --command -- sleep infinity >/dev/null
  fi
  kubectl wait pod/bench-probe --for=condition=Ready --timeout=120s >/dev/null
}

cache_metric() {
  kubectl exec bench-probe -- curl -s --max-time 5 "${CACHE_ADDR#http://}/metrics" 2>/dev/null \
    | awk -v k="$1" '$1==k || index($1, k"{")==1 {v=$NF} END {print v+0}'
}

replicas="$(kubectl -n "${CACHE_NAMESPACE}" get deploy "${CACHE_DEPLOYMENT}" \
  -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
if [[ "${replicas}" != "1" ]]; then
  echo "serving capacity: ${CACHE_DEPLOYMENT} reports ${replicas:-no} replicas; this measures a single pod" >&2
  exit 1
fi

ensure_probe_pod

if [[ "$(cache_metric process_cpu_seconds_total)" == "0" ]]; then
  echo "serving capacity: the cache service exports no process CPU metric" >&2
  echo "serving capacity: rebuild and redeploy the cache image before measuring" >&2
  exit 1
fi

for chunk_size in ${CHUNK_SIZES}; do
  stamp="$(date +%Y%m%dT%H%M%S)"
  csv="${OUTDIR}/serving-capacity-${chunk_size}-${stamp}.csv"
  cpu="${csv%.csv}.cpu"

  echo "### serving capacity: chunk size ${chunk_size} bytes, concurrency ${CONCURRENCY}"

  cpu_before="$(cache_metric process_cpu_seconds_total)"
  hits_before="$(cache_metric cache_hits_total)"

  cleanup
  kubectl run "${POD}" --image="${IMAGE}" --restart=Never --command -- \
    /usr/local/bin/cache-load-generator \
      -addr "${CACHE_ADDR}" \
      -chunks "${CHUNKS}" \
      -chunk-size "${chunk_size}" \
      -concurrency "${CONCURRENCY}" \
      -duration "${DURATION}" \
      -label "chunk${chunk_size}" >/dev/null

  if ! kubectl wait "pod/${POD}" --for=jsonpath='{.status.phase}'=Succeeded --timeout=3600s >/dev/null; then
    echo "serving capacity: the generator pod did not finish" >&2
    kubectl describe "pod/${POD}" >&2 || true
    kubectl logs "${POD}" >&2 || true
    exit 1
  fi

  kubectl logs "${POD}" | grep -E '^(label|chunk)' >"${csv}"
  cleanup

  cpu_after="$(cache_metric process_cpu_seconds_total)"
  hits_after="$(cache_metric cache_hits_total)"
  {
    echo "metric,before,after,delta"
    echo "process_cpu_seconds_total,${cpu_before},${cpu_after},$(awk -v a="${cpu_after}" -v b="${cpu_before}" 'BEGIN{printf "%.3f", a-b}')"
    echo "cache_hits_total,${hits_before},${hits_after},$(awk -v a="${hits_after}" -v b="${hits_before}" 'BEGIN{printf "%.0f", a-b}')"
  } >"${cpu}"

  rows="$(( $(wc -l <"${csv}") - 1 ))"
  if (( rows < 1 )); then
    echo "serving capacity: no rows captured for chunk size ${chunk_size}" >&2
    exit 1
  fi
  echo "### serving capacity: ${rows} rows in ${csv}, CPU delta in ${cpu}"
done

echo "### serving capacity complete"
