#!/usr/bin/env bash
set -euo pipefail

# Sweeps emulated registry egress bandwidth across the local scale-out
# benchmark at zero added delay, so the crossover between eager OCI pulling
# and the cluster cache becomes a measured rate instead of an argument. The
# outer loop switches the benchmark configuration once, the middle loop
# re-arms the shaping per rate point, and the inner loop runs one cell per
# corpus image. Cells that already have a CSV are skipped, so an interrupted
# sweep resumes by rerunning the script with the same environment.
#
# Usage: [RATES=..] [CONFIGS=..] [IMAGES=..] [REPS=..] benchmark-rate-sweep.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RATES="${RATES:-100mbit 250mbit 400mbit 600mbit 1000mbit 2500mbit 10000mbit}"
CONFIGS="${CONFIGS:-4 1 2}"
IMAGES="${IMAGES:-fn-python-hello:8080 fn-java-springboot:8080 fn-pytorch-cpu:8080}"
REPS="${REPS:-5}"
SCALE="${SCALE:-3}"
PREFIX="${PREFIX:-rate-sweep}"
REGISTRY="registry.localhost:5000"
NODES="k3d-nydus-cache-server-0 k3d-nydus-cache-agent-0 k3d-nydus-cache-agent-1"
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
PROVENANCE="${OUTDIR}/rate-sweep-provenance.csv"
SCRATCH="$(mktemp -d)"

# Leave the registry unshaped whenever the sweep stops; the next invocation
# re-arms the shaping for the point it resumes at
cleanup() {
  rm -rf "${SCRATCH}"
  "${REPO_ROOT}/scripts/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "${OUTDIR}"
[[ -f "${PROVENANCE}" ]] || \
  echo "timestamp,config,rate,measured_bytes_per_second,ratio" >"${PROVENANCE}"

# The campaign spans days, and a host reboot leaves the cluster superficially
# up while the snapshotter is down and the chunk-proxy needs a restart.
# Measuring into that state produces poisoned cells that look like data, so
# the driver refuses to start until the cluster is genuinely ready
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

  # A node was once found whose containerd content store had lost the layer blobs
  # behind almost every image record, so every pod failed to start including the
  # sandbox itself. That state fails quietly: pods simply never start, and a
  # campaign left running against it produces timeouts instead of data, so each
  # node has to prove it can still run a pod before any measurement begins. The
  # sandbox image is what every pod needs anyway and is already on every node,
  # so this proves the store without reaching out to a public registry
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

  "${REPO_ROOT}/scripts/registry-netem.sh" off >/dev/null 2>&1 || true
  echo "preflight: unshaped registry throughput $("${REPO_ROOT}/scripts/registry-throughput.sh") B/s"
}

# Only the nydus configurations read through the snapshotter, and switching to
# configuration 1 deliberately stops it, so this cannot be part of the preflight
# that runs before the first switch. The status is read into a variable rather
# than piped into a quiet grep, which would close the pipe on the first match
# and leave the reporting script killed by a broken pipe
require_snapshotter() {
  local status running
  if ! status="$("${REPO_ROOT}/scripts/node-snapshotter.sh" status)"; then
    echo "sweep: could not read the snapshotter status from the nodes" >&2
    return 1
  fi

  running="$(grep -c "running" <<<"${status}")" || true
  if [[ "${running}" != "3" ]]; then
    echo "sweep: the nydus snapshotter runs on ${running} of 3 nodes" >&2
    echo "${status}" >&2
    return 1
  fi
}

# What actually crosses the wire for an eager pull, measured from the registry
# manifests. The thesis size ladder is uncompressed on-disk size and is roughly
# four times larger, so it cannot be used to reason about transfer time
compressed_layer_bytes() {
  case "$1" in
    fn-python-hello) echo 48738419 ;;
    fn-java-springboot) echo 94041621 ;;
    fn-pytorch-cpu) echo 267478092 ;;
    *) echo "" ;;
  esac
}

# Defence in depth for the configuration that has to pay the registry. The reset
# gates coldness before the measurement; this re-checks it afterwards from the
# measurement itself, because during development a run once produced warm pulls
# while the reset's own gate reported clean and that was never explained. A pull
# far below what the shaped link allows did not cross the wire, whatever the
# reset believed. Pods that land back on the seed node are marked warm and are
# legitimately near zero, so only the cold rows are held to the floor
assert_cell_cold() {
  local csv="$1" image="$2" configured_bps="$3" bytes floor violations
  bytes="$(compressed_layer_bytes "${image}")"
  if [[ -z "${bytes}" ]]; then
    echo "sweep: no compressed layer size known for ${image}, so its cold rows cannot be checked" >&2
    return 1
  fi

  floor="$(awk -v b="${bytes}" -v r="${configured_bps}" 'BEGIN { printf "%.3f", 0.6 * b / r }')"
  violations="$(awk -F, -v floor="${floor}" \
    'NR > 1 && $5 == "no" && $7 + 0 < floor { print "  rep " $2 " " $3 " " $4 " pull_seconds " $7 }' \
    "${csv}")"
  if [[ -n "${violations}" ]]; then
    echo "sweep: cold rows below the ${floor}s floor for ${image}, so this cell is not cold:" >&2
    echo "${violations}" >&2
    mv "${csv}" "${csv%.csv}.failed-cold"
    echo "sweep: kept the evidence at ${csv%.csv}.failed-cold" >&2
    return 1
  fi
}

preflight

for config in ${CONFIGS}; do
  echo "### sweep: switching to config ${config}"
  "${REPO_ROOT}/scripts/config-switch.sh" "${config}"

  case "${config}" in
    1) tag="oci" ;;
    *) tag="nydus-none"; require_snapshotter ;;
  esac

  for rate in ${RATES}; do
    "${REPO_ROOT}/scripts/registry-netem.sh" on 0ms "${rate}"

    # Refuse to measure against a registry that is not delivering the rate it
    # was told to: a cell whose shaping silently failed is indistinguishable
    # from a fast configuration
    measured="$("${REPO_ROOT}/scripts/registry-throughput.sh")"
    configured_bps="$(awk -v r="${rate%mbit}" 'BEGIN { printf "%.0f", r * 1000000 / 8 }')"
    ratio="$(awk -v m="${measured}" -v c="${configured_bps}" 'BEGIN { printf "%.3f", m / c }')"
    if ! awk -v r="${ratio}" 'BEGIN { exit !(r >= 0.8 && r <= 1.2) }'; then
      echo "### sweep: ABORT config ${config} rate ${rate}:" \
        "measured ${measured} B/s is ${ratio}x the configured ${configured_bps} B/s" >&2
      exit 1
    fi

    echo "$(date -u +%FT%TZ),${config},${rate},${measured},${ratio}" >>"${PROVENANCE}"
    echo "### sweep: config ${config} rate ${rate} (measured ${measured} B/s, ${ratio}x)"

    for entry in ${IMAGES}; do
      image="${entry%%:*}"
      port="${entry##*:}"

      if ls "${OUTDIR}/${PREFIX}-config${config}-${image}-rate${rate}-"*.csv >/dev/null 2>&1; then
        echo "### sweep: skip config ${config} ${image} rate ${rate} (CSV exists)"
        continue
      fi

      workload="${SCRATCH}/workload-${config}-${image}.yaml"
      ACR="${REGISTRY}" SERVICE="${image}" TAG="${tag}" PORT="${port}" \
        "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"

      # A failed cell surrenders its CSV and the sweep moves on; the partial
      # file is set aside so a rerun repeats the whole cell
      csv="${OUTDIR}/${PREFIX}-config${config}-${image}-rate${rate}-$(date +%Y%m%dT%H%M%S).csv"
      if SERVICE="${image}" WORKLOAD="${workload}" \
          "${REPO_ROOT}/scripts/benchmark-scaleout.sh" "${config}" "${REPS}" "${csv}" "${SCALE}"; then
        if [[ "${config}" == "1" && "${REPS}" != "0" ]]; then
          assert_cell_cold "${csv}" "${image}" "${configured_bps}"
        fi
      else
        echo "### sweep: FAILED config ${config} ${image} rate ${rate}"
        if [[ -f "${csv}" ]]; then
          mv "${csv}" "${csv%.csv}.failed-partial"
        fi
        kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      fi
    done
  done
done

echo "### sweep complete"
