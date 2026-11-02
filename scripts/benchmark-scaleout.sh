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
# Usage: benchmark-scaleout.sh <1|2|2s|3|4> <repetitions> [outfile] [scale-target]
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${1:?usage: benchmark-scaleout.sh <1|2|2s|3|4> <repetitions> [outfile] [scale-target]}"
REPS="${2:?usage: benchmark-scaleout.sh <1|2|2s|3|4> <repetitions> [outfile] [scale-target]}"
OUTFILE="${3:-${REPO_ROOT}/analysis/results/benchmarks/scaleout-config${CONFIG}-$(date +%Y%m%dT%H%M%S).csv}"
# Three replicas put one cold pod on each remaining node, which is the published
# protocol. Two replicas start exactly one cold pod, so a cold start can be
# measured without a second one competing for the same host
SCALE_TARGET="${4:-3}"

# Overridable so the same harness runs on AKS (cache at 10.0.0.50, reset via
# cluster-reset-aks.sh) and locally with the defaults
CACHE_IP="${CACHE_IP:-10.43.0.50:8080}"
RESET_SCRIPT="${RESET_SCRIPT:-${REPO_ROOT}/scripts/cluster-reset.sh}"
SERVICE="${SERVICE:-fn-python-hello}"
READY_TIMEOUT="${READY_TIMEOUT:-420}"
case "${CONFIG}" in
  1) WORKLOAD="${WORKLOAD:-${REPO_ROOT}/deploy/k8s/knative/workload-oci.yaml}" ;;
  2|2s|3|4) WORKLOAD="${WORKLOAD:-${REPO_ROOT}/deploy/k8s/knative/workload-nydus.yaml}" ;;
  *) echo "unknown config ${CONFIG}; choose 1, 2, 2s, 3, or 4" >&2; exit 1 ;;
esac

# The p2p disk caches survive a plain workload reset, so the reset script
# additionally recycles the Dragonfly pods when this configuration runs
[[ "${CONFIG}" == "3" ]] && export RESET_DRAGONFLY=1

# The cache toggle lives in the chunk-proxy DaemonSet and any manifest
# re-apply quietly resets it to the default, which would invalidate every
# sample without failing anything, so refuse to measure until the flag
# matches the configuration under test
case "${CONFIG}" in
  2) wanted_cache_flag="--cache-enabled=false" ;;
  4) wanted_cache_flag="--cache-enabled=true" ;;
  *) wanted_cache_flag="" ;;
esac
if [[ -n "${wanted_cache_flag}" ]]; then
  proxy_args="$(kubectl -n kube-system get ds chunk-proxy -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)"
  if [[ "${proxy_args}" != *"${wanted_cache_flag}"* ]]; then
    echo "chunk-proxy runs ${proxy_args} but config ${CONFIG} needs ${wanted_cache_flag}; run config-switch first" >&2
    exit 1
  fi
fi

# The nydusd backend template decides whether chunk reads go through the
# chunk-proxy or through Dragonfly, and a leftover template from the other
# nydus configuration would silently measure the wrong system. Only the AKS
# cluster carries the node-setup DaemonSet, so local runs skip this check
if kubectl -n kube-system get ds nydus-node-setup >/dev/null 2>&1; then
  case "${CONFIG}" in
    2|4) wanted_backend='"type": "http-proxy"' ;;
    3) wanted_backend='"type": "registry"' ;;
    *) wanted_backend="" ;;
  esac
  if [[ -n "${wanted_backend}" ]]; then
    for setup_pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o name); do
      if ! kubectl -n kube-system exec "${setup_pod#pod/}" -- nsenter -t 1 -m -p -- \
          grep -q "${wanted_backend}" /etc/nydus/nydusd-config.json; then
        echo "nydusd template on ${setup_pod} does not match config ${CONFIG}; run config-switch first" >&2
        exit 1
      fi
      if [[ "${CONFIG}" == "3" ]] && ! kubectl -n kube-system exec "${setup_pod#pod/}" -- \
          nsenter -t 1 -m -p -n -- curl -sf -m 5 http://127.0.0.1:4003/healthy >/dev/null; then
        echo "dfdaemon is not answering on ${setup_pod}; Dragonfly must be healthy for config 3" >&2
        exit 1
      fi
    done
  fi
else
  # On the local rig the template sits in each node container. Configuration
  # 2s differs from 2 only in this file, and 3 differs from 2s only in the
  # Dragonfly mirror, so a leftover would go unnoticed
  case "${CONFIG}" in
    2|4) wanted_backend='"type": "http-proxy"' ;;
    2s|3) wanted_backend='"type": "registry"' ;;
    *) wanted_backend="" ;;
  esac
  if [[ -n "${wanted_backend}" ]]; then
    for node in $("${REPO_ROOT}/scripts/cluster-nodes.sh"); do
      if ! docker exec "${node}" grep -q "${wanted_backend}" /etc/nydus/nydusd-config.json; then
        echo "nydusd template on ${node} does not match config ${CONFIG}; run config-switch first" >&2
        exit 1
      fi
      has_mirror="no"
      docker exec "${node}" grep -q '127.0.0.1:4001' /etc/nydus/nydusd-config.json && has_mirror="yes"
      if [[ "${CONFIG}" == "3" && "${has_mirror}" == "no" ]] || [[ "${CONFIG}" == "2s" && "${has_mirror}" == "yes" ]]; then
        echo "nydusd template on ${node} has Dragonfly mirror=${has_mirror}, wrong for config ${CONFIG}" >&2
        exit 1
      fi
    done
  fi
  if [[ "${CONFIG}" == "3" ]] && ! "${REPO_ROOT}/scripts/dragonfly-rig.sh" healthy; then
    echo "a Dragonfly client is not answering; Dragonfly must be healthy for config 3" >&2
    exit 1
  fi
fi

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

# A cache that has been scaled away or killed answers nothing, and the
# degradation experiments put it in exactly that state on purpose. The reading
# is taken in two steps rather than one pipeline because pipefail would
# otherwise turn an unreachable probe into a failed assignment and abort the
# run, which is the one outcome these experiments must not produce: a missing
# cache has to read as no traffic, not as a dead harness
cache_metric() {
  local raw
  raw="$(kubectl exec bench-probe -- curl -s --max-time 5 "http://${CACHE_IP}/metrics" 2>/dev/null || true)"
  awk -v k="$1" '$1==k || index($1, k"{")==1 {v=$NF} END {print v+0}' <<<"${raw}"
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

# How often the kubelet restarted a pod's function container before it came
# up, with the exit code and reason of the last crash, as "count,code,reason".
# A pod that never crashed gives "0,,", a pod that is already gone ",,"
container_restarts() {
  local fields
  fields="$(kubectl get pod "$1" -o jsonpath='{range .status.containerStatuses[?(@.name=="user-container")]}{.restartCount},{.lastState.terminated.exitCode},{.lastState.terminated.reason}{end}' 2>/dev/null || true)"
  echo "${fields:-,,}"
}

# Keeps the seed pod's full status and its events, the kubelet's back-off and
# kill notices among them, so every restart can be timed afterwards. The next
# reset deletes the pod and its events with it
capture_seed_pod_record() {
  local rep="$1" pod="$2" directory
  directory="${OUTFILE%.csv}.logs/rep${rep}"
  mkdir -p "${directory}"
  kubectl get pod "${pod}" -o yaml >"${directory}/seed-pod-status.yaml" 2>/dev/null || true
  kubectl get events --field-selector "involvedObject.name=${pod}" \
    -o custom-columns='FIRST:.firstTimestamp,LAST:.lastTimestamp,COUNT:.count,TYPE:.type,REASON:.reason,MESSAGE:.message' \
    --sort-by=.lastTimestamp >"${directory}/seed-pod-events.txt" 2>/dev/null || true
  kubectl get events --field-selector "involvedObject.name=${pod}" -o yaml \
    >"${directory}/seed-pod-events.yaml" 2>/dev/null || true
}

node_of_pod() {
  kubectl get pod "$1" -o jsonpath='{.spec.nodeName}'
}

# Total bytes a node has received on every interface except loopback. On the
# local rig a node is a docker container; on AKS the node-setup pod enters the
# host's network namespace and reads eth0 alone, because accelerated
# networking counts every packet twice, once on eth0 and once on the network
# card behind it, and pod interfaces carry no pull traffic. An empty answer
# means the counter is unavailable
node_rx_total() {
  local node="$1" raw setup_pod only=""
  if docker inspect "${node}" >/dev/null 2>&1; then
    raw="$(docker exec "${node}" cat /proc/net/dev 2>/dev/null || true)"
  else
    only="eth0"
    setup_pod="$(kubectl -n kube-system get pods -l app=nydus-node-setup \
      --field-selector "spec.nodeName=${node}" -o name 2>/dev/null | head -1)"
    [[ -n "${setup_pod}" ]] || { echo ""; return; }
    raw="$(kubectl -n kube-system exec "${setup_pod#pod/}" -- nsenter -t 1 -n -- \
      cat /proc/net/dev 2>/dev/null || true)"
  fi
  awk -F'[: ]+' -v only="${only}" 'NR > 2 { name = ($1 == "") ? $2 : $1; value = ($1 == "") ? $3 : $2
    if (name != "lo" && (only == "" || name == only)) { total += value; found = 1 } }
    END { if (found) printf "%.0f", total }' <<<"${raw}"
}

# The wall-clock moment a phase began, so a per-second trace recorded next to
# the cell, such as the host processor sampler, can be cut to that phase
utc_stamp() {
  date -u -d "@$1" +%FT%T.%3NZ
}

# Remembers every node's receive counter at the start of a phase, so the
# bytes a pull really moved can be checked against what the image demands
snapshot_rx() {
  RX_BEFORE=()
  local node
  for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
    RX_BEFORE["${node}"]="$(node_rx_total "${node}")"
  done
}

rx_delta() {
  local node="$1" before after
  before="${RX_BEFORE[${node}]:-}"
  after="$(node_rx_total "${node}")"
  if [[ -z "${before}" || -z "${after}" ]]; then
    echo ""
    return
  fi
  echo $((after - before))
}
declare -A RX_BEFORE

# The local rig's nodes are docker containers, AKS nodes are not
ON_LOCAL_RIG="no"
first_node="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [[ -n "${first_node}" ]] && docker inspect "${first_node}" >/dev/null 2>&1; then
  ON_LOCAL_RIG="yes"
fi

# Failed registry fetches a node's chunk proxy has counted so far, one
# "reason<TAB>count" line per reason after a first line "answered" that shows
# the proxy exports the counter at all; a proxy built before the counter
# existed gives no answer rather than a false zero.
# The proxy listens on the node's loopback address, so the reading is taken
# from inside the node: through docker on the local rig, through the
# node-setup pod's host namespace on AKS
proxy_fetch_errors() {
  local node="$1" raw setup_pod
  if docker inspect "${node}" >/dev/null 2>&1; then
    raw="$(docker exec "${node}" wget -qO- -T 5 http://127.0.0.1:18000/metrics 2>/dev/null || true)"
  else
    setup_pod="$(kubectl -n kube-system get pods -l app=nydus-node-setup \
      --field-selector "spec.nodeName=${node}" -o name 2>/dev/null | head -1)"
    [[ -n "${setup_pod}" ]] || return 0
    raw="$(kubectl -n kube-system exec "${setup_pod#pod/}" -- nsenter -t 1 -m -n -- \
      curl -s -m 5 http://127.0.0.1:18000/metrics 2>/dev/null || true)"
  fi
  grep -q '^# HELP registry_fetch_errors_total' <<<"${raw}" || return 0
  echo "answered"
  sed -n 's/^registry_fetch_errors_total{reason="\([^"]*\)"} \([0-9.e+]*\)$/\1\t\2/p' <<<"${raw}"
}

# Remembers every node's failed-fetch counters at the start of a phase
snapshot_fetch_errors() {
  FETCH_ERRORS_BEFORE=()
  local node
  for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
    FETCH_ERRORS_BEFORE["${node}"]="$(proxy_fetch_errors "${node}")"
  done
}

# Prints "total,reasons" for the failed fetches a node's proxy counted since
# the snapshot, the reasons as "reason=count" joined by "|". When the proxy
# did not answer both times, as under eager pulling, both fields stay empty
fetch_errors_delta() {
  local node="$1" before after
  before="${FETCH_ERRORS_BEFORE[${node}]:-}"
  after="$(proxy_fetch_errors "${node}")"
  if [[ "${before}" != answered* || "${after}" != answered* ]]; then
    echo ","
    return
  fi
  awk -F'\t' -v before="${before}" '
    BEGIN {
      count = split(before, lines, "\n")
      for (i = 2; i <= count; i++) { split(lines[i], parts, "\t"); old[parts[1]] = parts[2] }
    }
    NR > 1 {
      delta = $2 - old[$1]; total += delta
      if (delta > 0) reasons = reasons (reasons == "" ? "" : "|") $1 "=" delta
    }
    END { printf "%d,%s", total, reasons }' <<<"${after}"
}
declare -A FETCH_ERRORS_BEFORE

# Packets the emulated registry link has thrown away because its queue was
# full, read from the shaping rule on the local rig. The count only grows, so
# two readings around a phase give that phase's drops. On AKS the answer is
# empty, even while the rig on the same host keeps its registry running
netem_dropped_total() {
  [[ "${ON_LOCAL_RIG}" == "yes" ]] || { echo ""; return; }
  docker inspect registry.localhost >/dev/null 2>&1 || { echo ""; return; }
  "${REPO_ROOT}/scripts/registry-netem.sh" status 2>/dev/null \
    | grep -o 'dropped [0-9]*' | head -1 | awk '{ print $2 }'
}

netem_dropped_since() {
  local before="$1" after
  after="$(netem_dropped_total)"
  if [[ -z "${before}" || -z "${after}" ]]; then
    echo ""
    return
  fi
  echo $((after - before))
}

mkdir -p "$(dirname "${OUTFILE}")"
echo "config,rep,phase,node,warm,ready_seconds,pull_seconds,cache_hits_delta,cache_misses_delta,node_rx_bytes,phase_started_at,netem_dropped,registry_fetch_errors,registry_fetch_error_reasons,restart_count,last_exit_code,last_exit_reason" >"${OUTFILE}"
echo "Writing results to ${OUTFILE}"

reset_probe_pod

# One discarded deployment first: the first cold start after a configuration
# switch absorbs control-plane warm-up from the node cycle and measures tens
# of seconds instead of the steady state
echo "=== config ${CONFIG} warmup (discarded)"
"${RESET_SCRIPT}" >/dev/null
ensure_probe_pod
apply_with_min_scale 1
kubectl wait ksvc/${SERVICE} --for=condition=Ready --timeout=300s >/dev/null

# With CAPTURE_LOGS set, the chunk proxies' logs and every nydusd log of a
# repetition are kept next to the CSV, together with that repetition's rows,
# so failed registry fetches can be read per node and phase afterwards. The
# next reset removes the nydusd logs from the nodes, so they are copied
# before it. Only AKS has the node-setup pods that reach the node's files
capture_repetition_logs() {
  local rep="$1" since="$2" directory pod node setup_pod
  directory="${OUTFILE%.csv}.logs/rep${rep}"
  mkdir -p "${directory}"
  awk -F, -v rep="${rep}" 'NR == 1 || $2 == rep' "${OUTFILE}" >"${directory}/config${CONFIG}-rep${rep}.csv"
  for pod in $(kubectl -n kube-system get pods -l app=chunk-proxy -o name 2>/dev/null); do
    node="$(kubectl -n kube-system get "${pod}" -o jsonpath='{.spec.nodeName}')"
    kubectl -n kube-system logs "${pod#pod/}" --since-time="${since}" >"${directory}/proxy-${node}.log" 2>/dev/null || true
  done
  for setup_pod in $(kubectl -n kube-system get pods -l app=nydus-node-setup -o name 2>/dev/null); do
    node="$(kubectl -n kube-system get "${setup_pod}" -o jsonpath='{.spec.nodeName}')"
    kubectl -n kube-system exec "${setup_pod#pod/}" -- nsenter -t 1 -m -- sh -c \
      'for log in /var/lib/containerd-nydus/logs/*/nydusd*.log; do [ -f "$log" ] && echo "== $log" && cat "$log"; done' \
      >"${directory}/nydusd-${node}.log" 2>/dev/null || true
  done
}

for rep in $(seq 1 "${REPS}"); do
  echo "=== config ${CONFIG} rep ${rep}/${REPS}"
  rep_started_at="$(date -u +%FT%TZ)"
  "${RESET_SCRIPT}" >/dev/null

  # A campaign can insist on proof that the reset left every node cold. One
  # more reset is allowed before the cell gives up, because a purge racing a
  # pod that is still terminating is the usual reason for a leftover
  if [[ -n "${PRE_SEED_CHECK:-}" ]] && ! bash -c "${PRE_SEED_CHECK}"; then
    echo "  check  nodes not cold after the reset, resetting once more" >&2
    "${RESET_SCRIPT}" >/dev/null
    if ! bash -c "${PRE_SEED_CHECK}"; then
      echo "nodes are still not cold before rep ${rep}; refusing to measure" >&2
      exit 1
    fi
  fi
  ensure_probe_pod

  # Seed phase: one pod cold-starts on a fresh cluster and, in the cached
  # configuration, leaves its chunks behind for the other nodes
  hits_before="$(cache_metric cache_hits_total)"
  misses_before="$(cache_metric cache_misses_total)"
  snapshot_rx
  snapshot_fetch_errors
  dropped_before="$(netem_dropped_total)"
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
  seed_rx="$(rx_delta "${seed_node}")"
  seed_dropped="$(netem_dropped_since "${dropped_before}")"
  seed_restarts="$(container_restarts "${seed_pod}")"
  echo "${CONFIG},${rep},seed,${seed_node},no,${seed_ready#* },$(pull_seconds "${seed_pod}"),${hits_delta},${misses_delta},${seed_rx},$(utc_stamp "${start}"),${seed_dropped},$(fetch_errors_delta "${seed_node}"),${seed_restarts}" >>"${OUTFILE}"
  echo "  seed   node=${seed_node} ready=${seed_ready#* }s hits+=${hits_delta} misses+=${misses_delta} netem-dropped+=${seed_dropped} restarts=${seed_restarts%%,*}"

  # A seed-only campaign measures the first cold start alone, with its logs,
  # and leaves the scale-out out entirely
  if [[ -n "${SEED_ONLY:-}" ]]; then
    capture_seed_pod_record "${rep}" "${seed_pod}"
    if [[ -n "${CAPTURE_LOGS:-}" ]]; then
      capture_repetition_logs "${rep}" "${rep_started_at}"
    fi
    continue
  fi

  # A hook between the two phases lets a campaign disturb the cache once the
  # seed has filled it and before the other nodes ask for it. That is the only
  # moment at which the degradation experiments can take the cache away from a
  # cold start that is about to depend on it
  if [[ -n "${PRE_SCALEOUT_HOOK:-}" ]]; then
    echo "  hook   ${PRE_SCALEOUT_HOOK}"
    bash -c "${PRE_SCALEOUT_HOOK}" || echo "  hook   returned non-zero, continuing" >&2
  fi

  # Scale-out phase: two more replicas land on the remaining nodes and
  # cold-start the same image; a pod returning to the seed node is marked
  # warm because that node already holds the chunks locally
  hits_before="$(cache_metric cache_hits_total)"
  misses_before="$(cache_metric cache_misses_total)"
  snapshot_rx
  snapshot_fetch_errors
  dropped_before="$(netem_dropped_total)"
  start="$(date +%s.%N)"
  apply_with_min_scale "${SCALE_TARGET}"
  scale_revision="$(revision_of_latest)"
  until [[ "${scale_revision}" != "${seed_revision}" && -n "${scale_revision}" ]]; do
    scale_revision="$(revision_of_latest)"
    sleep 0.2
  done
  scale_ready="$(wait_pods_ready "${scale_revision}" "${SCALE_TARGET}" "${start}")"
  hits_delta="$(awk -v a="$(cache_metric cache_hits_total)" -v b="${hits_before}" 'BEGIN{print a-b}')"
  misses_delta="$(awk -v a="$(cache_metric cache_misses_total)" -v b="${misses_before}" 'BEGIN{print a-b}')"
  scale_dropped="$(netem_dropped_since "${dropped_before}")"
  while read -r pod ready; do
    node="$(node_of_pod "${pod}")"
    warm="no"
    [[ "${node}" == "${seed_node}" ]] && warm="yes"
    echo "${CONFIG},${rep},scaleout,${node},${warm},${ready},$(pull_seconds "${pod}"),${hits_delta},${misses_delta},$(rx_delta "${node}"),$(utc_stamp "${start}"),${scale_dropped},$(fetch_errors_delta "${node}"),$(container_restarts "${pod}")" >>"${OUTFILE}"
    echo "  scale  node=${node} warm=${warm} ready=${ready}s"
  done <<<"${scale_ready}"
  echo "  scale  hits+=${hits_delta} misses+=${misses_delta} netem-dropped+=${scale_dropped}"

  if [[ -n "${CAPTURE_LOGS:-}" ]]; then
    capture_repetition_logs "${rep}" "${rep_started_at}"
  fi

  # Without proof that Dragonfly actually moved the bytes, config 3 is
  # indistinguishable from a plain registry rerun, so the download activity
  # of every peer is captured next to the CSV while the pods still exist —
  # the next repetition's reset recycles them and the logs vanish with them
  if [[ "${CONFIG}" == "3" ]]; then
    evidence="${OUTFILE%.csv}.dragonfly-evidence.log"
    {
      echo "=== rep ${rep} $(date -u +%FT%TZ)"
      for client_pod in $(kubectl -n dragonfly-system get pods -l component=client -o name) \
                        $(kubectl -n dragonfly-system get pods -l component=seed-client -o name); do
        echo "--- ${client_pod}"
        kubectl -n dragonfly-system logs "${client_pod#pod/}" --since=20m 2>/dev/null \
          | grep -aE "download (task|piece).*(succeeded|finished)|back-to-source|proxy_via_dfdaemon" \
          | tail -400
      done
    } >>"${evidence}" 2>/dev/null || true
  fi
done

echo "Done: ${OUTFILE}"
