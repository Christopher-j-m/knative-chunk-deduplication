#!/usr/bin/env bash
set -euo pipefail

# Finds the ceiling of the single cache service pod: how many chunk reads per
# second it serves, at what latency, as the number of simultaneous requesters
# grows. The generator runs as a pod inside the cluster so the measurement
# crosses the same path a chunk proxy crosses, and the cache's own process
# metrics are captured on either side of the sweep so a saturated pod can be
# told from an idle one.
#
# Usage: [CHUNK_SIZES=..] [CHUNKS=..] [CONCURRENCY=..] [DURATION=..] benchmark-cache-saturation.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHUNK_SIZES="${CHUNK_SIZES:-32768 262144}"
CHUNKS="${CHUNKS:-512}"
CONCURRENCY="${CONCURRENCY:-1,2,4,8,16,32,64,128,256}"
DURATION="${DURATION:-10s}"
CACHE_ADDR="${CACHE_ADDR:-http://10.43.0.50:8080}"
IMAGE="${IMAGE:-registry.localhost:5000/nydus-cluster-cache:0.1.0}"
CACHE_DEPLOYMENT="${CACHE_DEPLOYMENT:-nydus-cluster-cache}"
CACHE_NAMESPACE="${CACHE_NAMESPACE:-default}"
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
POD=cache-loadgen

mkdir -p "${OUTDIR}"

cleanup() {
  kubectl delete pod "${POD}" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A long-lived probe pod reads the cache's metrics endpoint, the same way the
# scale-out harness does, so the counters come from inside the cluster network
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

# The service must actually be the single-replica deployment the thesis
# describes, or the ceiling being measured belongs to something else
replicas="$(kubectl -n "${CACHE_NAMESPACE}" get deploy "${CACHE_DEPLOYMENT}" \
  -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
if [[ "${replicas}" != "1" ]]; then
  echo "saturation: ${CACHE_DEPLOYMENT} reports ${replicas:-no} replicas; this measures a single pod" >&2
  exit 1
fi

ensure_probe_pod

# The process collector is what separates a CPU-bound ceiling from a
# network-bound one, and an older cache image simply does not export it
if [[ "$(cache_metric process_cpu_seconds_total)" == "0" ]]; then
  echo "saturation: the cache service exports no process CPU metric" >&2
  echo "saturation: rebuild and redeploy the cache image before measuring" >&2
  exit 1
fi

for chunk_size in ${CHUNK_SIZES}; do
  stamp="$(date +%Y%m%dT%H%M%S)"
  csv="${OUTDIR}/cache-saturation-${chunk_size}-${stamp}.csv"
  cpu="${csv%.csv}.cpu"

  echo "### saturation: chunk size ${chunk_size} bytes, concurrency ${CONCURRENCY}"

  cpu_before="$(cache_metric process_cpu_seconds_total)"
  hits_before="$(cache_metric cache_hits_total)"

  cleanup
  kubectl run "${POD}" --image="${IMAGE}" --restart=Never --command -- \
    /usr/local/bin/cache-loadgen \
      -addr "${CACHE_ADDR}" \
      -chunks "${CHUNKS}" \
      -chunk-size "${chunk_size}" \
      -concurrency "${CONCURRENCY}" \
      -duration "${DURATION}" \
      -label "chunk${chunk_size}" >/dev/null

  # the generator writes its CSV to stdout and its progress to stderr, so the
  # pod has to finish before the log is complete
  if ! kubectl wait "pod/${POD}" --for=jsonpath='{.status.phase}'=Succeeded --timeout=3600s >/dev/null; then
    echo "saturation: the generator pod did not finish" >&2
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
    echo "saturation: no rows captured for chunk size ${chunk_size}" >&2
    exit 1
  fi
  echo "### saturation: ${rows} rows in ${csv}, CPU delta in ${cpu}"
done

echo "### saturation complete"
