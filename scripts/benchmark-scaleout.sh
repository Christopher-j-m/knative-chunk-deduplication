#!/usr/bin/env bash
set -euo pipefail

# Measures cold scale-out for the current benchmark configuration in two
# phases per repetition: a seed deployment cold-starts the function on one
# node (and, with the cache enabled, fills the cluster cache), then the
# service is scaled to three replicas so the remaining nodes cold-start the
# same image. The per-pod ready times of those later pods isolate what the
# thesis cares about: whether a node pays the registry round trips again or
# reuses chunks already in the cluster cache. Run registry-netem.sh first to
# give registry traffic realistic latency, otherwise the local registry is
# too fast for the configurations to differ.
#
# Usage: benchmark-scaleout.sh <1|2|4> <repetitions> [outfile]
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${1:?usage: benchmark-scaleout.sh <1|2|4> <repetitions> [outfile]}"
REPS="${2:?usage: benchmark-scaleout.sh <1|2|4> <repetitions> [outfile]}"
OUTFILE="${3:-${REPO_ROOT}/analysis/results/benchmarks/scaleout-config${CONFIG}-$(date +%Y%m%dT%H%M%S).csv}"

CACHE_IP="10.43.0.50:8080"
SERVICE="fn-python-hello"
READY_TIMEOUT=420
case "${CONFIG}" in
  1) WORKLOAD="${REPO_ROOT}/deploy/k8s/knative/workload-oci.yaml" ;;
  2|4) WORKLOAD="${REPO_ROOT}/deploy/k8s/knative/workload-nydus.yaml" ;;
  *) echo "unknown config ${CONFIG}; choose 1, 2, or 4" >&2; exit 1 ;;
esac

# A long-lived in-cluster pod doubles as the metrics scraper, so each sample
# is one fast exec instead of a fresh pod launch. It is recreated per
# invocation: a probe pod left over from another configuration sits on
# snapshots of the previous snapshotter and never comes back Ready
ensure_probe_pod() {
  if ! kubectl get pod bench-probe >/dev/null 2>&1; then
    kubectl run bench-probe --image=curlimages/curl --restart=Never \
      --command -- sleep infinity >/dev/null
  fi
  kubectl wait pod/bench-probe --for=condition=Ready --timeout=120s >/dev/null
}

reset_probe_pod() {
  kubectl delete pod bench-probe --ignore-not-found --timeout=60s >/dev/null
}

cache_metric() {
  kubectl exec bench-probe -- curl -s --max-time 5 "http://${CACHE_IP}/metrics" 2>/dev/null \
    | awk -v k="$1" '$1==k || index($1, k"{")==1 {v=$NF} END {print v+0}'
}

# Reads the kubelet's own pull duration for the function image out of the
# Pulled event message, normalising to seconds. "Already present" pulls
# report zero
pull_seconds() {
  local pod="$1" message duration
  message="$(kubectl get events --field-selector "involvedObject.name=${pod},reason=Pulled" \
    -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' | grep -F "${SERVICE}" | head -1)"
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

# Applies the workload with the requested minimum replica count by rewriting
# the min-scale annotation on the way in, so each phase is one revision and
# one simultaneous set of cold starts
apply_with_min_scale() {
  sed "s|autoscaling.knative.dev/min-scale: \"0\"|autoscaling.knative.dev/min-scale: \"$1\"|" \
    "${WORKLOAD}" | kubectl apply -f - >/dev/null 2>&1
}

revision_of_latest() {
  kubectl get ksvc "${SERVICE}" -o jsonpath='{.status.latestCreatedRevisionName}' 2>/dev/null
}

# Watches the pods of one revision until the expected number report Ready,
# printing "pod elapsed-seconds" the moment each one turns. The caller's
# start timestamp anchors the clock
wait_pods_ready() {
  local revision="$1" expected="$2" start="$3" deadline seen line pod status
  deadline=$((SECONDS + READY_TIMEOUT))
  seen=""
  while (( $(echo "${seen}" | grep -c .) < expected )); do
    if (( SECONDS > deadline )); then
      echo "timed out waiting for ${expected} ready pods of ${revision}" >&2
      return 1
    fi
    while read -r pod status; do
      [[ "${status}" == "True" ]] || continue
      case "${seen}" in *"${pod}"*) continue ;; esac
      seen="${seen}${pod}
"
      echo "${pod} $(awk -v a="$(date +%s.%N)" -v b="${start}" 'BEGIN{printf "%.3f", a-b}')"
    done < <(kubectl get pods -l "serving.knative.dev/revision=${revision}" \
      -o jsonpath='{range .items[*]}{.metadata.name} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null)
    sleep 0.2
  done
}

node_of_pod() {
  kubectl get pod "$1" -o jsonpath='{.spec.nodeName}'
}

mkdir -p "$(dirname "${OUTFILE}")"
echo "config,rep,phase,node,warm,ready_seconds,pull_seconds,cache_hits_delta,cache_misses_delta" >"${OUTFILE}"
echo "Writing results to ${OUTFILE}"

reset_probe_pod

# One discarded deployment first: the first cold start after a configuration
# switch absorbs control-plane warm-up from the node cycle and measures tens
# of seconds instead of the steady state
echo "=== config ${CONFIG} warmup (discarded)"
"${REPO_ROOT}/scripts/cluster-reset.sh" >/dev/null
ensure_probe_pod
apply_with_min_scale 1
kubectl wait ksvc/${SERVICE} --for=condition=Ready --timeout=300s >/dev/null

for rep in $(seq 1 "${REPS}"); do
  echo "=== config ${CONFIG} rep ${rep}/${REPS}"
  "${REPO_ROOT}/scripts/cluster-reset.sh" >/dev/null
  ensure_probe_pod

  # Seed phase: one pod cold-starts on a fresh cluster and, in the cached
  # configuration, leaves its chunks behind for the other nodes
  hits_before="$(cache_metric cache_hits_total)"
  misses_before="$(cache_metric cache_misses_total)"
  start="$(date +%s.%N)"
  apply_with_min_scale 1
  seed_revision=""
  until [[ -n "${seed_revision}" ]]; do
    seed_revision="$(revision_of_latest)"
    sleep 0.2
  done
  seed_ready="$(wait_pods_ready "${seed_revision}" 1 "${start}")"
  seed_pod="${seed_ready%% *}"
  seed_node="$(node_of_pod "${seed_pod}")"
  hits_delta="$(awk -v a="$(cache_metric cache_hits_total)" -v b="${hits_before}" 'BEGIN{print a-b}')"
  misses_delta="$(awk -v a="$(cache_metric cache_misses_total)" -v b="${misses_before}" 'BEGIN{print a-b}')"
  echo "${CONFIG},${rep},seed,${seed_node},no,${seed_ready#* },$(pull_seconds "${seed_pod}"),${hits_delta},${misses_delta}" >>"${OUTFILE}"
  echo "  seed   node=${seed_node} ready=${seed_ready#* }s hits+=${hits_delta} misses+=${misses_delta}"

  # Scale-out phase: two more replicas land on the remaining nodes and
  # cold-start the same image; a pod returning to the seed node is marked
  # warm because that node already holds the chunks locally
  hits_before="$(cache_metric cache_hits_total)"
  misses_before="$(cache_metric cache_misses_total)"
  start="$(date +%s.%N)"
  apply_with_min_scale 3
  scale_revision="$(revision_of_latest)"
  until [[ "${scale_revision}" != "${seed_revision}" && -n "${scale_revision}" ]]; do
    scale_revision="$(revision_of_latest)"
    sleep 0.2
  done
  scale_ready="$(wait_pods_ready "${scale_revision}" 3 "${start}")"
  hits_delta="$(awk -v a="$(cache_metric cache_hits_total)" -v b="${hits_before}" 'BEGIN{print a-b}')"
  misses_delta="$(awk -v a="$(cache_metric cache_misses_total)" -v b="${misses_before}" 'BEGIN{print a-b}')"
  while read -r pod ready; do
    node="$(node_of_pod "${pod}")"
    warm="no"
    [[ "${node}" == "${seed_node}" ]] && warm="yes"
    echo "${CONFIG},${rep},scaleout,${node},${warm},${ready},$(pull_seconds "${pod}"),${hits_delta},${misses_delta}" >>"${OUTFILE}"
    echo "  scale  node=${node} warm=${warm} ready=${ready}s"
  done <<<"${scale_ready}"
  echo "  scale  hits+=${hits_delta} misses+=${misses_delta}"
done

echo "Done: ${OUTFILE}"
