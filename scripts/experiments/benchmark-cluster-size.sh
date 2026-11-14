#!/usr/bin/env bash
# Runs the scale-out benchmark at different cluster sizes by cordoning nodes of one large rig
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NODE_COUNTS="${NODE_COUNTS:-2 3 5 7}"
CONFIGS="${CONFIGS:-4 2 1}"
IMAGES="${IMAGES:-fn-python-hello:8080}"
RTT="${RTT:-80}"
RATE="${RATE:-600mbit}"
REPS="${REPS:-5}"
PREFIX="${PREFIX:-cluster-size}"
REGISTRY="registry.localhost:5000"
OUTDIR="$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" "${PREFIX}")"
NETWORK_CHECK="${OUTDIR}/${PREFIX}-network-check.csv"
SCRATCH="$(mktemp -d)"

CELLS_RUN=0
CELLS_SKIPPED=0

# shellcheck source=cold-guard.sh
source "${REPO_ROOT}/scripts/experiments/cold-guard.sh"
CONFIGURED_BPS="$(rate_bytes_per_second "${RATE}")"

mapfile -t ALL_NODES < <("${REPO_ROOT}/scripts/cluster/cluster-nodes.sh")
AVAILABLE="${#ALL_NODES[@]}"
if (( AVAILABLE == 0 )); then
  echo "cluster size: no cluster nodes found; is the k3d cluster running?" >&2
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
  "${REPO_ROOT}/scripts/cluster/registry-netem.sh" off >/dev/null 2>&1 || true
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
    echo "cluster size: asked for ${keep} schedulable nodes but the cluster reports ${schedulable}" >&2
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

for count in ${NODE_COUNTS}; do
  if (( count > AVAILABLE )); then
    echo "### cluster size: n=${count} will be skipped, the cluster has ${AVAILABLE} nodes" >&2
  fi
done

mkdir -p "${OUTDIR}"
[[ -f "${NETWORK_CHECK}" ]] || \
  echo "timestamp,config,nodes,image,rtt_ms,rate,measured_connect_seconds" >"${NETWORK_CHECK}"

"${REPO_ROOT}/scripts/cluster/registry-netem.sh" on "${RTT}ms" "${RATE}"
connect_seconds="$(measured_connect)"
if ! awk -v connect="${connect_seconds}" -v rtt="${RTT}" \
    'BEGIN { ms = connect * 1000; exit !(ms >= rtt - 5 && ms <= rtt + 30) }'; then
  echo "### cluster size: ABORT, connect ${connect_seconds}s does not reflect the ${RTT}ms shaping" >&2
  exit 1
fi

for config in ${CONFIGS}; do
  echo "### cluster size: switching to config ${config}"
  "${REPO_ROOT}/scripts/cluster/config-switch.sh" "${config}"

  case "${config}" in
    1) tag="oci" ;;
    *) tag="nydus-none" ;;
  esac

  for count in ${NODE_COUNTS}; do
    if (( count > AVAILABLE )); then
      continue
    fi

    echo "### cluster size: cordoning to ${count} of ${AVAILABLE} nodes"
    cordon_to "${count}"

    for entry in ${IMAGES}; do
      image="${entry%%:*}"
      port="${entry##*:}"

      if [[ -z "${FORCE:-}" ]] && ls "${OUTDIR}/${PREFIX}-config${config}-${image}-n${count}-rtt${RTT}ms-rate${RATE}-"*.csv >/dev/null 2>&1; then
        echo "### cluster size: skip config ${config} ${image} n=${count} (CSV exists)"
        CELLS_SKIPPED=$((CELLS_SKIPPED + 1))
        continue
      fi
      CELLS_RUN=$((CELLS_RUN + 1))

      workload="${SCRATCH}/workload-${config}-${image}.yaml"
      ACR="${REGISTRY}" SERVICE="${image}" TAG="${tag}" PORT="${port}" \
        "${REPO_ROOT}/scripts/cluster/render-workload.sh" >"${workload}"

      echo "$(date -u +%FT%TZ),${config},${count},${image},${RTT},${RATE},${connect_seconds}" >>"${NETWORK_CHECK}"

      csv="${OUTDIR}/${PREFIX}-config${config}-${image}-n${count}-rtt${RTT}ms-rate${RATE}-$(date +%Y%m%dT%H%M%S).csv"

      "${REPO_ROOT}/scripts/experiments/sample-host-cpu.sh" "${csv%.csv}.cpu.csv" &
      sampler=$!

      sleep 2
      if ! kill -0 "${sampler}" 2>/dev/null || [[ ! -s "${csv%.csv}.cpu.csv" ]]; then
        echo "### cluster size: ABORT, the host CPU sampler did not start for ${csv}" >&2
        exit 1
      fi

      if ! SERVICE="${image}" WORKLOAD="${workload}" \
          "${REPO_ROOT}/scripts/experiments/benchmark-scale-out.sh" "${config}" "${REPS}" "${csv}" "${count}"; then
        echo "### cluster size: FAILED config ${config} ${image} n=${count}"
        [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
        kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      elif [[ "${config}" == "1" ]]; then
        assert_cell_cold "${csv}" "${image}" "${CONFIGURED_BPS}" || true
      fi
      kill "${sampler}" 2>/dev/null || true
      wait "${sampler}" 2>/dev/null || true
    done
  done
done

if (( CELLS_RUN == 0 )); then
  echo "### cluster size measured nothing: all ${CELLS_SKIPPED} cells already had a CSV in ${OUTDIR}" >&2
  echo "### move them aside, or rerun with FORCE=1 to overwrite" >&2
  exit 1
fi

echo "### cluster size experiment complete: ${CELLS_RUN} cells measured, ${CELLS_SKIPPED} skipped"
