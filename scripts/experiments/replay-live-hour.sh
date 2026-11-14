#!/usr/bin/env bash
# Replays the one-hour trace extract on the local rig in real time and records latencies, pods, cache counters and registry requests
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

CONFIG="${1:?usage: replay-live-hour.sh <2|2s|4>}"
case "${CONFIG}" in 2|2s|4) ;; *) echo "config must be 2, 2s or 4" >&2; exit 1 ;; esac
RTT="${RTT:-80ms}"
RATE="${RATE:-600mbit}"
TRACE="${TRACE:-analysis/traces/live-hour.csv}"
APPS="${APPS:-analysis/traces/live-hour-apps.json}"
ACR="${ACR:-registry.localhost:5000}"
TAG="${TAG:-nydus-none}"
PYTHON="${PYTHON:-python3}"
REGISTRY_CONTAINER=registry.localhost
INGRESS_PORT=18080
CACHE_PORT=18081

OUT="$(python3 scripts/summaries/result_paths.py live-hour)/live-hour-config${CONFIG}-$(date -u +%Y%m%dT%H%M%S)"
mkdir -p "${OUT}"
echo "writing to ${OUT}"

PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do kill "${pid}" 2>/dev/null || true; done
  kubectl delete ksvc -l replay=live-hour --ignore-not-found --timeout=180s >/dev/null 2>&1 || true
}
trap cleanup EXIT

app_names() { "${PYTHON}" -c "import json,sys; print('\n'.join(json.load(open(sys.argv[1]))['apps']))" "${APPS}"; }
image_of() { "${PYTHON}" -c "import json,sys; print(json.load(open(sys.argv[1]))['apps'][sys.argv[2]]['image'])" "${APPS}" "$1"; }

cache_metric() {
  curl -s --max-time 5 "http://127.0.0.1:${CACHE_PORT}/metrics" \
    | awk -v k="$1" '$1==k || index($1, k"{")==1 {v=$NF} END {print v+0}'
}

registry_lines() { docker logs "${REGISTRY_CONTAINER}" 2>&1 | grep -ac 'blobs/sha256' || true; }

echo "== configuration ${CONFIG}, reset, registry netem ${RTT} at ${RATE} =="
scripts/cluster/config-switch.sh "${CONFIG}"
scripts/cluster/cluster-reset.sh
scripts/cluster/registry-netem.sh on "${RTT}" "${RATE}"

kubectl -n knative-serving patch configmap config-autoscaler --type merge \
  -p '{"data":{"allow-zero-initial-scale":"true"}}' >/dev/null

echo "== one service per application =="
while read -r app; do
  ACR="${ACR}" SERVICE="$(image_of "${app}")" NAME="${app}" TAG="${TAG}" INITIAL_SCALE=0 \
    scripts/cluster/render-workload.sh \
    | sed 's|^  namespace: default$|  namespace: default\n  labels:\n    replay: live-hour|' \
    | kubectl apply -f - >/dev/null
done < <(app_names)
kubectl wait ksvc -l replay=live-hour --for=condition=Ready --timeout=300s >/dev/null \
  || echo "warning: not every service reported Ready; continuing" >&2

kubectl get ksvc -l replay=live-hour -o json \
  | "${PYTHON}" -c "import json,sys; d=json.load(sys.stdin); json.dump({i['metadata']['name']: i['status']['url'] for i in d['items']}, sys.stdout)" \
  >"${OUT}/urls.json"

echo "== port-forwards, pod watch, counters =="
kubectl -n kourier-system port-forward svc/kourier-internal "${INGRESS_PORT}:80" >/dev/null 2>&1 &
PIDS+=("$!")
kubectl port-forward svc/nydus-cluster-cache "${CACHE_PORT}:8080" >/dev/null 2>&1 &
PIDS+=("$!")
sleep 3

( kubectl get pods -l serving.knative.dev/service -w --no-headers \
    -o custom-columns='NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase,CREATED:.metadata.creationTimestamp' \
  | while read -r line; do echo "$(date -u +%s),${line}"; done >"${OUT}/pods.csv" ) &
PIDS+=("$!")

hits_before="$(cache_metric cache_hits_total)"
misses_before="$(cache_metric cache_misses_total)"
put_bytes_before="$(cache_metric cache_put_bytes_total)"
registry_before="$(registry_lines)"
cp "${APPS}" "${OUT}/apps.json"
echo "config=${CONFIG} rtt=${RTT} rate=${RATE} started=$(date -u +%FT%TZ)" >"${OUT}/run.txt"

echo "== replaying $(tail -n +2 "${TRACE}" | wc -l) invocations in real time =="
go run ./cmd/trace-replayer -trace "${TRACE}" -urls "${OUT}/urls.json" \
  -gateway "http://127.0.0.1:${INGRESS_PORT}" >"${OUT}/requests.csv" || echo "warning: some requests failed" >&2

sleep 30
hits_after="$(cache_metric cache_hits_total)"
misses_after="$(cache_metric cache_misses_total)"
put_bytes_after="$(cache_metric cache_put_bytes_total)"
stored_bytes="$(cache_metric cache_stored_bytes)"
"${PYTHON}" -c "import json,sys; json.dump({'hits_before': $hits_before, 'hits_after': $hits_after, 'misses_before': $misses_before, 'misses_after': $misses_after, 'put_bytes_before': $put_bytes_before, 'put_bytes_after': $put_bytes_after, 'stored_bytes_after': $stored_bytes}, open(sys.argv[1], 'w'), indent=1)" "${OUT}/cache-counters.json"
docker logs "${REGISTRY_CONTAINER}" 2>&1 | grep -a 'blobs/sha256' | tail -n "+$((registry_before + 1))" >"${OUT}/registry-requests.log" || true
echo "finished=$(date -u +%FT%TZ)" >>"${OUT}/run.txt"

echo "== teardown =="
cleanup
trap - EXIT
scripts/cluster/registry-netem.sh off
echo "done: ${OUT}"
