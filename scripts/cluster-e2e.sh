#!/usr/bin/env bash
set -euo pipefail

# Validates the local cluster under configuration 4: a function
# cold-starts, a second node reuses cached chunks (cache hits rise), and the
# function still starts when the cache is down (registry fallback).
#
# RUN-TIME NOTE: not executed in the authoring environment. Requires a live
# k3d cluster brought up per deploy/local/bootstrap.sh with the corpus loaded.
CACHE_IP="10.43.0.50:8080"

# metric <name> prints the latest value of a cache counter, tolerating an
# optional Prometheus label set on the sample line.
metric() {
  kubectl run "m-$RANDOM" --rm -i --restart=Never --image=curlimages/curl -- \
    -s "http://${CACHE_IP}/metrics" \
    | awk -v k="$1" '$1==k || index($1, k"{")==1 {v=$NF} END {print v+0}'
}

echo "Switching to config 4 and resetting"
"$(dirname "$0")/config-switch.sh" 4
"$(dirname "$0")/cluster-reset.sh"

echo "Cold start on first node"
kubectl apply -f deploy/k8s/knative/workload-nydus.yaml
kubectl wait ksvc/fn-python-hello --for=condition=Ready --timeout=180s
HITS_AFTER_FIRST="$(metric cache_hits_total)"
echo "cache_hits_total after first node: ${HITS_AFTER_FIRST}"

echo "Forcing a second-node cold start by scaling out"
kubectl patch ksvc/fn-python-hello --type=merge -p \
  '{"spec":{"template":{"metadata":{"annotations":{"autoscaling.knative.dev/min-scale":"3"}}}}}'
sleep 45
HITS_AFTER_SECOND="$(metric cache_hits_total)"
echo "cache_hits_total after scale-out: ${HITS_AFTER_SECOND}"

if [[ "${HITS_AFTER_SECOND}" -le "${HITS_AFTER_FIRST}" ]]; then
  echo "FAIL: expected cache hits to rise on cross-node reuse" >&2
  exit 1
fi
echo "PASS: cross-node cache reuse observed"

echo "Degradation: delete the cache and confirm the function still starts"
kubectl scale deploy/nydus-cluster-cache --replicas=0
kubectl delete ksvc/fn-python-hello
kubectl apply -f deploy/k8s/knative/workload-nydus.yaml
if kubectl wait ksvc/fn-python-hello --for=condition=Ready --timeout=180s; then
  echo "PASS: function cold-starts with cache down (registry fallback)"
else
  echo "FAIL: function did not start with cache down" >&2
  exit 1
fi
kubectl scale deploy/nydus-cluster-cache --replicas=1
echo "All Stage A validation checks passed"
