#!/usr/bin/env bash
set -euo pipefail

# Runs the full configuration x corpus-image scale-out campaign on AKS.
# One CSV lands per cell; cells that already have a CSV are skipped, so an
# interrupted campaign resumes by rerunning this script. Configurations are
# the outer loop so containerd is switched once per configuration, not once
# per cell. Every repetition first proves that no node holds the corpus, and
# every eager-pull cell must pass a pull-time floor and a received-bytes check
# afterwards; a cell that fails is set aside as .failed-cold and measured again.
# Usage: ACR=<name>.azurecr.io [PREFIX=aks2] scripts/benchmark-campaign-aks.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACR="${ACR:?set ACR to the ACR login server}"
REPS="${REPS:-5}"
CONFIGS="${CONFIGS:-1 2 4}"
IMAGES="${IMAGES:-fn-go-hello:8080 fn-python-hello:8080 fn-java-springboot:8080 fn-node-api:8080 fn-python-ml:8080 fn-pytorch-cpu:8080}"
PREFIX="${PREFIX:-aks}"
ATTEMPTS="${ATTEMPTS:-3}"
# The largest image gives the reference pull the most bytes to average over
RATE_IMAGE="${RATE_IMAGE:-fn-pytorch-cpu}"
# Our own service images, whose base layers every node keeps for good. The
# corpus may live in a different registry from them, as it does for the
# cross-region anchors
SERVICE_ACR="${SERVICE_ACR:-${ACR}}"
INFRASTRUCTURE="${INFRASTRUCTURE:-${SERVICE_ACR}/chunk-proxy:0.2.1,${SERVICE_ACR}/nydus-cluster-cache:0.2.0,${SERVICE_ACR}/nydus-node-installer:0.1.1}"
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "${SCRATCH}"' EXIT

mkdir -p "${OUTDIR}"
export ACR REPO_ROOT

# What the guards know about the corpus is read from the registry once and
# kept beside the results, so a later reader can check the bytes they used
image_names=""
for entry in ${IMAGES}; do
  image_names+=" ${entry%%:*}"
done
export IMAGE_FACTS="${OUTDIR}/${PREFIX}-image-facts.json"
if [[ ! -s "${IMAGE_FACTS}" ]]; then
  # shellcheck disable=SC2086
  python3 "${REPO_ROOT}/scripts/acr-image-facts.py" "--infrastructure=${INFRASTRUCTURE}" \
    "${ACR}" ${image_names} >"${IMAGE_FACTS}"
fi
# shellcheck source=cold-guard-aks.sh
source "${REPO_ROOT}/scripts/cold-guard-aks.sh"
export PRE_SEED_CHECK="source '${REPO_ROOT}/scripts/cold-guard-aks.sh' && assert_nodes_hold_no_corpus"

# Keep the chunk proxies' and nydusd's logs of every repetition beside the
# cell, so a registry that refuses or throttles requests leaves a record
export CAPTURE_LOGS=1

# Measures one cell, retrying when a guard rejects it. Only the eager-pull
# configuration has a floor to hold, since lazy pulls never move the whole image
run_cell() {
  local config="$1" image="$2" workload="$3" attempt csv
  for attempt in $(seq 1 "${ATTEMPTS}"); do
    csv="${OUTDIR}/${PREFIX}-scaleout-config${config}-${image}-$(date +%Y%m%dT%H%M%S).csv"
    echo "### campaign: config ${config} image ${image} attempt ${attempt}/${ATTEMPTS}"

    # A failed cell surrenders its CSV; the partial file is set aside so a
    # rerun repeats the whole cell
    if ! CACHE_IP=10.0.0.50:8080 \
        RESET_SCRIPT="${REPO_ROOT}/scripts/cluster-reset-aks.sh" \
        ACR="${ACR}" SERVICE="${image}" WORKLOAD="${workload}" \
        "${REPO_ROOT}/scripts/benchmark-scaleout.sh" "${config}" "${REPS}" "${csv}"; then
      echo "### campaign: FAILED config ${config} ${image}"
      [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
      kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      continue
    fi

    if [[ "${config}" == "1" ]]; then
      if ! assert_cell_cold "${csv}" "${image}" "${PULL_RATE_BPS}" \
          || ! assert_cell_received_bytes "${csv}" "${image}"; then
        echo "### campaign: COLD GUARD FAILED config ${config} ${image}"
        continue
      fi
    fi
    echo "### campaign: accepted ${csv##*/}"
    return 0
  done
  echo "### campaign: GAVE UP config ${config} ${image} after ${ATTEMPTS} attempts"
  return 1
}

for config in ${CONFIGS}; do
  echo "### campaign: switching to config ${config}"
  ACR="${ACR}" "${REPO_ROOT}/scripts/config-switch-aks.sh" "${config}"

  case "${config}" in
    1) tag="oci" ;;
    *) tag="nydus-none" ;;
  esac

  # The pull floor needs the rate this registry path really delivers today,
  # measured once per session from compressed bytes rather than assumed. The
  # nodes must be clean first and again afterwards: containerd shares stored
  # layers between namespaces, so a leftover copy would make the fetch instant
  if [[ "${config}" == "1" && -z "${PULL_RATE_BPS:-}" ]]; then
    ACR="${ACR}" "${REPO_ROOT}/scripts/cluster-reset-aks.sh" >/dev/null
    assert_nodes_hold_no_corpus
    rate_line="$(measure_pull_rate "${RATE_IMAGE}")"
    assert_nodes_hold_no_corpus
    read -r PULL_RATE_BPS rate_bytes rate_seconds <<<"${rate_line}"
    if [[ -z "${PULL_RATE_BPS}" || "${PULL_RATE_BPS}" == "0" ]]; then
      echo "### campaign: FAILED reference fetch, no pull floor can be set" >&2
      exit 1
    fi
    export PULL_RATE_BPS
    rate_log="${OUTDIR}/${PREFIX}-pull-rate.csv"
    [[ -f "${rate_log}" ]] || echo "measured_at,image,compressed_bytes,fetch_seconds,bytes_per_second" >"${rate_log}"
    echo "$(date -u +%FT%TZ),${RATE_IMAGE},${rate_bytes},${rate_seconds},${PULL_RATE_BPS}" \
      >>"${rate_log}"
    echo "### campaign: reference fetch ${RATE_IMAGE} ${rate_bytes} B in ${rate_seconds}s = ${PULL_RATE_BPS} B/s"
  fi

  for entry in ${IMAGES}; do
    image="${entry%%:*}"
    port="${entry##*:}"

    if ls "${OUTDIR}/${PREFIX}-scaleout-config${config}-${image}-"*.csv >/dev/null 2>&1; then
      echo "### campaign: skip config ${config} ${image} (CSV exists)"
      continue
    fi

    workload="${SCRATCH}/workload-${config}-${image}.yaml"
    ACR="${ACR}" SERVICE="${image}" TAG="${tag}" PORT="${port}" \
      "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"
    run_cell "${config}" "${image}" "${workload}" || true
  done
done

echo "### campaign complete"
