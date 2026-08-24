#!/usr/bin/env bash
set -euo pipefail

# Varies the cluster size so the registry-traffic saving and the fan-out cost
# become measured curves instead of an arithmetic claim about n nodes. The rig
# is created once at its full size and cordoned back down per point, which
# keeps host load constant across the sweep and makes the node count the only
# variable that moves. Cordoned nodes keep running their DaemonSets, so the
# chunk proxy stays on every node while only the uncordoned ones can take a
# function pod.
#
# Usage: [NODE_COUNTS=..] [CONFIGS=..] [IMAGES=..] [RTT=..] [RATE=..] [REPS=..] benchmark-node-sweep.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODE_COUNTS="${NODE_COUNTS:-2 3 5 7}"
CONFIGS="${CONFIGS:-4 2 1}"
IMAGES="${IMAGES:-fn-python-hello:8080}"
RTT="${RTT:-80}"
RATE="${RATE:-600mbit}"
REPS="${REPS:-5}"
PREFIX="${PREFIX:-node-sweep}"
REGISTRY="registry.localhost:5000"
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
PROVENANCE="${OUTDIR}/node-sweep-provenance.csv"
SCRATCH="$(mktemp -d)"

CELLS_RUN=0
CELLS_SKIPPED=0

mapfile -t ALL_NODES < <("${REPO_ROOT}/scripts/cluster-nodes.sh")
AVAILABLE="${#ALL_NODES[@]}"
if (( AVAILABLE == 0 )); then
  echo "sweep: no cluster nodes found; is the k3d cluster running?" >&2
  exit 1
fi

# Every k3d node container maps to one Kubernetes node whose name is the
# container name, so the cordon list can be built without a second lookup
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

# Keeps the first n nodes schedulable and cordons the rest. The server node
# comes first in the list, so every size includes it and the sizes nest
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
    echo "sweep: asked for ${keep} schedulable nodes but the cluster reports ${schedulable}" >&2
    return 1
  fi
}

REGISTRY_IP="$(docker inspect \
  -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' \
  registry.localhost)"
measured_connect() {
  curl -o /dev/null -s -m 15 "http://${REGISTRY_IP}:5000/v2/" || true
  curl -o /dev/null -s -m 15 -w '%{time_connect}' "http://${REGISTRY_IP}:5000/v2/"
}

# Say up front which requested sizes the cluster cannot serve, rather than
# letting them disappear one at a time inside the campaign's own output
for count in ${NODE_COUNTS}; do
  if (( count > AVAILABLE )); then
    echo "### sweep: n=${count} will be skipped, the cluster has ${AVAILABLE} nodes" >&2
  fi
done

mkdir -p "${OUTDIR}"
[[ -f "${PROVENANCE}" ]] || \
  echo "timestamp,config,nodes,image,rtt_ms,rate,measured_connect_seconds" >"${PROVENANCE}"

"${REPO_ROOT}/scripts/registry-netem.sh" on "${RTT}ms" "${RATE}"
connect_seconds="$(measured_connect)"
if ! awk -v connect="${connect_seconds}" -v rtt="${RTT}" \
    'BEGIN { ms = connect * 1000; exit !(ms >= rtt - 5 && ms <= rtt + 30) }'; then
  echo "### sweep: ABORT, connect ${connect_seconds}s does not reflect the ${RTT}ms shaping" >&2
  exit 1
fi

for config in ${CONFIGS}; do
  echo "### sweep: switching to config ${config}"
  "${REPO_ROOT}/scripts/config-switch.sh" "${config}"

  case "${config}" in
    1) tag="oci" ;;
    *) tag="nydus-none" ;;
  esac

  for count in ${NODE_COUNTS}; do
    if (( count > AVAILABLE )); then
      continue
    fi

    echo "### sweep: cordoning to ${count} of ${AVAILABLE} nodes"
    cordon_to "${count}"

    for entry in ${IMAGES}; do
      image="${entry%%:*}"
      port="${entry##*:}"

      if [[ -z "${FORCE:-}" ]] && ls "${OUTDIR}/${PREFIX}-config${config}-${image}-n${count}-rtt${RTT}ms-rate${RATE}-"*.csv >/dev/null 2>&1; then
        echo "### sweep: skip config ${config} ${image} n=${count} (CSV exists)"
        CELLS_SKIPPED=$((CELLS_SKIPPED + 1))
        continue
      fi
      CELLS_RUN=$((CELLS_RUN + 1))

      workload="${SCRATCH}/workload-${config}-${image}.yaml"
      ACR="${REGISTRY}" SERVICE="${image}" TAG="${tag}" PORT="${port}" \
        "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"

      echo "$(date -u +%FT%TZ),${config},${count},${image},${RTT},${RATE},${connect_seconds}" >>"${PROVENANCE}"

      csv="${OUTDIR}/${PREFIX}-config${config}-${image}-n${count}-rtt${RTT}ms-rate${RATE}-$(date +%Y%m%dT%H%M%S).csv"

      # The scale target is the whole cluster: the seed takes one node and the
      # remaining n-1 nodes each cold-start the same image, which is the
      # scenario whose registry traffic the (n-1)/n claim is about
      if ! SERVICE="${image}" WORKLOAD="${workload}" \
          "${REPO_ROOT}/scripts/benchmark-scaleout.sh" "${config}" "${REPS}" "${csv}" "${count}"; then
        echo "### sweep: FAILED config ${config} ${image} n=${count}"
        [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
        kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      fi
    done
  done
done

if (( CELLS_RUN == 0 )); then
  echo "### sweep measured nothing: all ${CELLS_SKIPPED} cells already had a CSV in ${OUTDIR}" >&2
  echo "### move them aside, or rerun with FORCE=1 to overwrite" >&2
  exit 1
fi

echo "### sweep complete: ${CELLS_RUN} cells measured, ${CELLS_SKIPPED} skipped"
