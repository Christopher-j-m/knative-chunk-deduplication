#!/usr/bin/env bash
# Runs the scale-out benchmark over a range of emulated registry round-trip times for every configuration and image
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RTTS="${RTTS:-0 10 25 50 80 100}"
RATE="${RATE:-600mbit}"
CONFIGS="${CONFIGS:-4 2}"
IMAGES="${IMAGES:-fn-python-hello:8080 fn-python-ml:8080}"
REPS="${REPS:-5}"
PREFIX="${PREFIX:-distance}"
REGISTRY="registry.localhost:5000"
OUTDIR="$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" "${PREFIX}")"
NETWORK_CHECK="${OUTDIR}/${PREFIX}-network-check.csv"
SCRATCH="$(mktemp -d)"

CELLS_RUN=0
CELLS_SKIPPED=0

cleanup() {
  rm -rf "${SCRATCH}"
  "${REPO_ROOT}/scripts/cluster/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "${OUTDIR}"
[[ -f "${NETWORK_CHECK}" ]] || \
  echo "timestamp,config,rtt_ms,rate,measured_connect_seconds" >"${NETWORK_CHECK}"

REGISTRY_IP="$(docker inspect \
  -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' \
  registry.localhost)"
measured_connect() {
  curl -o /dev/null -s -m 15 "http://${REGISTRY_IP}:5000/v2/" || true
  curl -o /dev/null -s -m 15 -w '%{time_connect}' "http://${REGISTRY_IP}:5000/v2/"
}

require_snapshotter() {
  local status running
  if ! status="$("${REPO_ROOT}/scripts/cluster/node-snapshotter.sh" status)"; then
    echo "distance: could not read the snapshotter status from the nodes" >&2
    return 1
  fi

  local expected
  expected="$("${REPO_ROOT}/scripts/cluster/cluster-nodes.sh" | grep -c . || true)"
  if [[ "${expected}" == "0" ]]; then
    echo "distance: no cluster nodes found; is the k3d cluster running?" >&2
    return 1
  fi
  running="$(grep -c "running" <<<"${status}")" || true
  if [[ "${running}" != "${expected}" ]]; then
    echo "distance: the nydus snapshotter runs on ${running} of ${expected} nodes" >&2
    echo "${status}" >&2
    return 1
  fi
}

# shellcheck source=cold-guard.sh
source "${REPO_ROOT}/scripts/experiments/cold-guard.sh"

CONFIGURED_BPS="$(rate_bytes_per_second "${RATE}")"

for config in ${CONFIGS}; do
  echo "### distance: switching to config ${config}"
  "${REPO_ROOT}/scripts/cluster/config-switch.sh" "${config}"

  case "${config}" in
    1) tag="oci" ;;
    *) tag="${NYDUS_TAG:-nydus-none}"; require_snapshotter ;;
  esac

  for rtt in ${RTTS}; do
    "${REPO_ROOT}/scripts/cluster/registry-netem.sh" on "${rtt}ms" "${RATE}"
    connect_seconds="$(measured_connect)"

    if ! awk -v connect="${connect_seconds}" -v rtt="${rtt}" \
        'BEGIN { ms = connect * 1000; exit !(ms >= rtt - 5 && ms <= rtt + 30) }'; then
      echo "### distance: ABORT config ${config} rtt ${rtt}ms:" \
        "connect ${connect_seconds}s does not reflect the shaping" >&2
      exit 1
    fi

    echo "$(date -u +%FT%TZ),${config},${rtt},${RATE},${connect_seconds}" >>"${NETWORK_CHECK}"
    echo "### distance: config ${config} rtt ${rtt}ms rate ${RATE} (connect ${connect_seconds}s)"

    for entry in ${IMAGES}; do
      image="${entry%%:*}"
      port="${entry##*:}"

      if [[ -z "${FORCE:-}" ]] && ls "${OUTDIR}/${PREFIX}-config${config}-${image}-rtt${rtt}ms-rate${RATE}-"*.csv >/dev/null 2>&1; then
        echo "### distance: skip config ${config} ${image} rtt ${rtt}ms (CSV exists)"
        CELLS_SKIPPED=$((CELLS_SKIPPED + 1))
        continue
      fi
      CELLS_RUN=$((CELLS_RUN + 1))

      workload="${SCRATCH}/workload-${config}-${image}.yaml"
      ACR="${REGISTRY}" SERVICE="${image}" TAG="${tag}" PORT="${port}" \
        "${REPO_ROOT}/scripts/cluster/render-workload.sh" >"${workload}"

      csv="${OUTDIR}/${PREFIX}-config${config}-${image}-rtt${rtt}ms-rate${RATE}-$(date +%Y%m%dT%H%M%S).csv"
      if SERVICE="${image}" WORKLOAD="${workload}" \
          "${REPO_ROOT}/scripts/experiments/benchmark-scale-out.sh" "${config}" "${REPS}" "${csv}"; then
        if [[ "${config}" == "1" && "${REPS}" != "0" ]]; then
          assert_cell_cold "${csv}" "${image}" "${CONFIGURED_BPS}"
        fi
      else
        echo "### distance: FAILED config ${config} ${image} rtt ${rtt}ms"
        [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
        kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      fi
    done
  done
done

if (( CELLS_RUN == 0 )); then
  echo "### distance measured nothing: all ${CELLS_SKIPPED} cells already had a CSV in ${OUTDIR}" >&2
  echo "### results from a different machine living in that directory will do this" >&2
  echo "### move them aside, or rerun with FORCE=1 to overwrite" >&2
  exit 1
fi

echo "### distance experiment complete: ${CELLS_RUN} cells measured, ${CELLS_SKIPPED} skipped"
