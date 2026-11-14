#!/usr/bin/env bash
# Runs the scale-out benchmark over a range of emulated registry bandwidths for every configuration and image
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RATES="${RATES:-100mbit 250mbit 400mbit 600mbit 1000mbit 2500mbit 10000mbit}"
CONFIGS="${CONFIGS:-4 1 2}"
IMAGES="${IMAGES:-fn-python-hello:8080 fn-java-springboot:8080 fn-pytorch-cpu:8080}"
REPS="${REPS:-5}"
SCALE="${SCALE:-3}"
PREFIX="${PREFIX:-bandwidth-two-pods}"
REGISTRY="registry.localhost:5000"
NODES="k3d-nydus-cache-server-0 k3d-nydus-cache-agent-0 k3d-nydus-cache-agent-1"
OUTDIR="$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" "${PREFIX}")"
NETWORK_CHECK="${OUTDIR}/${PREFIX}-network-check.csv"
SCRATCH="$(mktemp -d)"

cleanup() {
  rm -rf "${SCRATCH}"
  "${REPO_ROOT}/scripts/cluster/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "${OUTDIR}"
[[ -f "${NETWORK_CHECK}" ]] || \
  echo "timestamp,config,rate,measured_bytes_per_second,ratio" >"${NETWORK_CHECK}"

preflight() {
  local ready node
  ready="$(kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready ')" || true
  if [[ "${ready}" != "3" ]]; then
    echo "preflight: ${ready} of 3 nodes Ready; check the k3d-nydus-cache context" >&2
    return 1
  fi
  if ! kubectl -n kube-system rollout status ds/chunk-proxy --timeout=60s >/dev/null; then
    echo "preflight: chunk-proxy DaemonSet is not ready" >&2
    return 1
  fi
  if ! kubectl rollout status deploy/nydus-cluster-cache --timeout=60s >/dev/null; then
    echo "preflight: cache-service is not ready" >&2
    return 1
  fi

  for node in ${NODES}; do
    kubectl delete pod "preflight-${node}" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
    kubectl run "preflight-${node}" --image=docker.io/rancher/mirrored-pause:3.6 \
      --image-pull-policy=IfNotPresent --restart=Never \
      --overrides="{\"spec\":{\"nodeName\":\"${node}\"}}" >/dev/null 2>&1 || true
    if ! kubectl wait "pod/preflight-${node}" --for=condition=Ready --timeout=120s >/dev/null 2>&1; then
      echo "preflight: ${node} cannot start a pod; check its containerd content store" >&2
      kubectl delete pod "preflight-${node}" --ignore-not-found >/dev/null 2>&1 || true
      return 1
    fi
    kubectl delete pod "preflight-${node}" --ignore-not-found >/dev/null 2>&1 || true
  done

  "${REPO_ROOT}/scripts/cluster/registry-netem.sh" off >/dev/null 2>&1 || true
  echo "preflight: unshaped registry throughput $("${REPO_ROOT}/scripts/experiments/registry-throughput.sh") B/s"
}

require_snapshotter() {
  local status running
  if ! status="$("${REPO_ROOT}/scripts/cluster/node-snapshotter.sh" status)"; then
    echo "bandwidth: could not read the snapshotter status from the nodes" >&2
    return 1
  fi

  running="$(grep -c "running" <<<"${status}")" || true
  if [[ "${running}" != "3" ]]; then
    echo "bandwidth: the nydus snapshotter runs on ${running} of 3 nodes" >&2
    echo "${status}" >&2
    return 1
  fi
}

compressed_layer_bytes() {
  case "$1" in
    fn-python-hello) echo 48738419 ;;
    fn-java-springboot) echo 94041621 ;;
    fn-pytorch-cpu) echo 267478092 ;;
    *) echo "" ;;
  esac
}

assert_cell_cold() {
  local csv="$1" image="$2" configured_bps="$3" bytes floor violations
  bytes="$(compressed_layer_bytes "${image}")"
  if [[ -z "${bytes}" ]]; then
    echo "bandwidth: no compressed layer size known for ${image}, so its cold rows cannot be checked" >&2
    return 1
  fi

  floor="$(awk -v b="${bytes}" -v r="${configured_bps}" 'BEGIN { printf "%.3f", 0.6 * b / r }')"
  violations="$(awk -F, -v floor="${floor}" \
    'NR > 1 && $5 == "no" && $7 + 0 < floor { print "  rep " $2 " " $3 " " $4 " pull_seconds " $7 }' \
    "${csv}")"
  if [[ -n "${violations}" ]]; then
    echo "bandwidth: cold rows below the ${floor}s floor for ${image}, so this cell is not cold:" >&2
    echo "${violations}" >&2
    mv "${csv}" "${csv%.csv}.failed-cold"
    echo "bandwidth: kept the evidence at ${csv%.csv}.failed-cold" >&2
    return 1
  fi
}

preflight

for config in ${CONFIGS}; do
  echo "### bandwidth: switching to config ${config}"
  "${REPO_ROOT}/scripts/cluster/config-switch.sh" "${config}"

  case "${config}" in
    1) tag="oci" ;;
    *) tag="nydus-none"; require_snapshotter ;;
  esac

  for rate in ${RATES}; do
    "${REPO_ROOT}/scripts/cluster/registry-netem.sh" on 0ms "${rate}"

    measured="$("${REPO_ROOT}/scripts/experiments/registry-throughput.sh")"
    configured_bps="$(awk -v r="${rate%mbit}" 'BEGIN { printf "%.0f", r * 1000000 / 8 }')"
    ratio="$(awk -v m="${measured}" -v c="${configured_bps}" 'BEGIN { printf "%.3f", m / c }')"
    if ! awk -v r="${ratio}" 'BEGIN { exit !(r >= 0.8 && r <= 1.2) }'; then
      echo "### bandwidth: ABORT config ${config} rate ${rate}:" \
        "measured ${measured} B/s is ${ratio}x the configured ${configured_bps} B/s" >&2
      exit 1
    fi

    echo "$(date -u +%FT%TZ),${config},${rate},${measured},${ratio}" >>"${NETWORK_CHECK}"
    echo "### bandwidth: config ${config} rate ${rate} (measured ${measured} B/s, ${ratio}x)"

    for entry in ${IMAGES}; do
      image="${entry%%:*}"
      port="${entry##*:}"

      if ls "${OUTDIR}/${PREFIX}-config${config}-${image}-rate${rate}-"*.csv >/dev/null 2>&1; then
        echo "### bandwidth: skip config ${config} ${image} rate ${rate} (CSV exists)"
        continue
      fi

      workload="${SCRATCH}/workload-${config}-${image}.yaml"
      ACR="${REGISTRY}" SERVICE="${image}" TAG="${tag}" PORT="${port}" \
        "${REPO_ROOT}/scripts/cluster/render-workload.sh" >"${workload}"

      csv="${OUTDIR}/${PREFIX}-config${config}-${image}-rate${rate}-$(date +%Y%m%dT%H%M%S).csv"
      if SERVICE="${image}" WORKLOAD="${workload}" \
          "${REPO_ROOT}/scripts/experiments/benchmark-scale-out.sh" "${config}" "${REPS}" "${csv}" "${SCALE}"; then
        if [[ "${config}" == "1" && "${REPS}" != "0" ]]; then
          assert_cell_cold "${csv}" "${image}" "${configured_bps}"
        fi
      else
        echo "### bandwidth: FAILED config ${config} ${image} rate ${rate}"
        if [[ -f "${csv}" ]]; then
          mv "${csv}" "${csv%.csv}.failed-partial"
        fi
        kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      fi
    done
  done
done

echo "### bandwidth experiment complete"
