#!/usr/bin/env bash
set -euo pipefail

# Measures what the cluster actually asks the registry for, at the registry
# rather than at the cache. The cache's own counters cannot answer this: with
# the cache switched off, configuration 2's chunk fetches go straight from the
# node to the registry and no cache counter moves, so the comparison that the
# (n-1)/n claim rests on has no denominator. The registry logs every blob
# request with a timestamp and a byte count, so this driver records the wall
# clock window of each phase and lets the summariser count the requests that
# fell inside it.
#
# One repetition per cell is enough here. The quantity is a count of fetches,
# which the fingerprints show to be deterministic, not a latency that needs
# repetitions to average.
#
# Usage: [NODE_COUNTS=..] [CONFIGS=..] [IMAGES=..] [RTT=..] [RATE=..] benchmark-registry-traffic.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODE_COUNTS="${NODE_COUNTS:-2 3 5 7}"
CONFIGS="${CONFIGS:-2 4}"
# One entry per image as name:port. The default is the single image the
# node-count sweep used; the corpus-wide run passes the whole ladder
IMAGES="${IMAGES:-fn-python-hello:8080}"
RTT="${RTT:-80}"
RATE="${RATE:-600mbit}"
REGISTRY="registry.localhost:5000"
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
WINDOWS="${OUTDIR}/registry-traffic-windows-$(date +%Y%m%dT%H%M%S).csv"
SCRATCH="$(mktemp -d)"

mapfile -t ALL_NODES < <("${REPO_ROOT}/scripts/cluster-nodes.sh")
AVAILABLE="${#ALL_NODES[@]}"
if (( AVAILABLE == 0 )); then
  echo "traffic: no cluster nodes found; is the k3d cluster running?" >&2
  exit 1
fi

uncordon_all() {
  for node in "${ALL_NODES[@]}"; do
    kubectl uncordon "${node}" >/dev/null 2>&1 || true
  done
}

cleanup() {
  rm -rf "${SCRATCH}"
  uncordon_all
  "${REPO_ROOT}/scripts/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap cleanup EXIT

cordon_to() {
  local keep="$1" index=0 schedulable
  for node in "${ALL_NODES[@]}"; do
    if (( index < keep )); then
      kubectl uncordon "${node}" >/dev/null
    else
      kubectl cordon "${node}" >/dev/null
    fi
    index=$((index + 1))
  done

  schedulable="$(kubectl get nodes \
    -o jsonpath='{range .items[*]}{.spec.unschedulable}{"\n"}{end}' \
    | grep -c '^$' || true)"
  if [[ "${schedulable}" != "${keep}" ]]; then
    echo "traffic: asked for ${keep} schedulable nodes but the cluster reports ${schedulable}" >&2
    return 1
  fi
}

# Knative reports a revision ready once its pods are; the phase boundary we
# want is the moment every expected pod of that revision has turned
wait_ready_pods() {
  local revision="$1" expected="$2" deadline ready
  deadline=$((SECONDS + 420))
  while true; do
    ready="$(kubectl get pods -l "serving.knative.dev/revision=${revision}" \
      -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null \
      | grep -c "True" || true)"
    [[ "${ready}" -ge "${expected}" ]] && return 0
    if (( SECONDS > deadline )); then
      echo "traffic: only ${ready} of ${expected} pods ready for ${revision}" >&2
      return 1
    fi
    sleep 0.5
  done
}

apply_with_min_scale() {
  sed "s|autoscaling.knative.dev/min-scale: \"0\"|autoscaling.knative.dev/min-scale: \"$1\"|" \
    "$2" | kubectl apply -f - >/dev/null 2>&1
}

mkdir -p "${OUTDIR}"
echo "config,nodes,image,rtt_ms,rate,reset_end,seed_end,scaleout_end" >"${WINDOWS}"

"${REPO_ROOT}/scripts/registry-netem.sh" on "${RTT}ms" "${RATE}"

for config in ${CONFIGS}; do
  echo "### traffic: switching to config ${config}"
  "${REPO_ROOT}/scripts/config-switch.sh" "${config}"

  case "${config}" in
    1) tag="oci" ;;
    *) tag="nydus-none" ;;
  esac

  for entry in ${IMAGES}; do
   IMAGE="${entry%%:*}"
   PORT="${entry##*:}"

   workload="${SCRATCH}/workload-${config}-${IMAGE}.yaml"
   ACR="${REGISTRY}" SERVICE="${IMAGE}" TAG="${tag}" PORT="${PORT}" \
     "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"

   for count in ${NODE_COUNTS}; do
    if (( count > AVAILABLE )); then
      continue
    fi

    echo "### traffic: config ${config}, ${IMAGE}, ${count} nodes"
    cordon_to "${count}"
    SERVICE="${IMAGE}" "${REPO_ROOT}/scripts/cluster-reset.sh" >/dev/null

    # the registry clock is what the log is stamped with, so the boundaries
    # are read in UTC to the second
    reset_end="$(date -u +%FT%TZ)"

    apply_with_min_scale 1 "${workload}"
    revision=""
    until [[ -n "${revision}" ]]; do
      revision="$(kubectl get ksvc "${IMAGE}" -o jsonpath='{.status.latestCreatedRevisionName}' 2>/dev/null || true)"
      sleep 0.2
    done
    wait_ready_pods "${revision}" 1
    seed_end="$(date -u +%FT%TZ)"

    apply_with_min_scale "${count}" "${workload}"
    scale_revision="${revision}"
    until [[ "${scale_revision}" != "${revision}" && -n "${scale_revision}" ]]; do
      scale_revision="$(kubectl get ksvc "${IMAGE}" -o jsonpath='{.status.latestCreatedRevisionName}' 2>/dev/null || true)"
      sleep 0.2
    done
    wait_ready_pods "${scale_revision}" "${count}"
    scaleout_end="$(date -u +%FT%TZ)"

    echo "${config},${count},${IMAGE},${RTT},${RATE},${reset_end},${seed_end},${scaleout_end}" >>"${WINDOWS}"
    echo "    seed ${reset_end} to ${seed_end}, scale-out to ${scaleout_end}"

    kubectl delete ksvc "${IMAGE}" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
   done
  done
done

echo "### traffic windows written to ${WINDOWS}"
echo "### now capture the registry log and summarise:"
echo "###   docker logs registry.localhost 2>&1 | grep -a 'level=info' | grep -a 'blobs/sha256' > ${OUTDIR}/registry-requests.log"
echo "###   python3 scripts/summarize-registry-traffic.py --windows ${WINDOWS} --log ${OUTDIR}/registry-requests.log"
