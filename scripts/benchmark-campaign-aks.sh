#!/usr/bin/env bash
set -euo pipefail

# Runs the full configuration x corpus-image scale-out campaign on AKS.
# One CSV lands per cell; cells that already have a CSV are skipped, so an
# interrupted campaign resumes by rerunning this script. Configurations are
# the outer loop so containerd is switched once per configuration, not once
# per cell. Every repetition first proves that no node holds the corpus, and
# every eager-pull cell must pass a pull-time floor and a received-bytes check
# afterwards; a cell that fails is set aside as .failed-cold and measured again.
# Stock nydusd (configuration 2s) can read another conversion than the
# uncompressed one, chosen with NYDUS_TAG, and prefetch the whole image with
# PREFETCH=all; every other configuration refuses both.
# Usage: ACR=<name>.azurecr.io [PREFIX=aks2] [NYDUS_TAG=nydus-none] [PREFETCH=off] scripts/benchmark-campaign-aks.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACR="${ACR:?set ACR to the ACR login server}"
NYDUS_TAG="${NYDUS_TAG:-nydus-none}"
PREFETCH="${PREFETCH:-off}"
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

# The chunk proxy path reads only the uncompressed conversion and never
# prefetches, so a compressed tag or prefetch would make its cells meaningless
if [[ "${NYDUS_TAG}" != "nydus-none" || "${PREFETCH}" != "off" ]]; then
  for config in ${CONFIGS}; do
    if [[ "${config}" != "2s" ]]; then
      echo "NYDUS_TAG=${NYDUS_TAG} PREFETCH=${PREFETCH} only apply to config 2s, not ${config}" >&2
      exit 1
    fi
  done
fi

# The cold guards must know the layers of the conversion the cells read
FACT_TAGS="oci,nydus-none"
[[ "${NYDUS_TAG}" != "nydus-none" ]] && FACT_TAGS+=",${NYDUS_TAG}"
export FACT_TAGS

# A node that prefetches keeps downloading after its pod is ready, so with
# QUIET_WAIT=1 (always under PREFETCH=all) the scale-out waits until no node
# receives data, and each node's bytes are recorded beside the cell. A
# prefetching cell counts only if every seed node received at least
# MIN_PREFETCH_FRACTION of the image's data blobs
QUIET_WAIT="${QUIET_WAIT:-0}"
[[ "${PREFETCH}" == "all" ]] && QUIET_WAIT=1
MIN_PREFETCH_FRACTION="${MIN_PREFETCH_FRACTION:-0.8}"

manifest_path() {
  echo "${OUTDIR}/${PREFIX}-manifest-$1.json"
}

# The manifest names the data blobs whose size a prefetching seed must reach,
# and fetching it proves the conversion is in the registry before any cell
save_manifest() {
  local image="$1"
  python3 - "${REPO_ROOT}/scripts/acr-image-facts.py" "${ACR}" "${image}" "${NYDUS_TAG}" \
      >"$(manifest_path "${image}")" <<'PYEOF'
import importlib.util
import json
import sys

helper_path, registry, image, tag = sys.argv[1:5]
spec = importlib.util.spec_from_file_location("acr_image_facts", helper_path)
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)
manifest = helper.platform_manifest(registry, image, tag, helper.anonymous_token(registry, image))
json.dump(manifest, sys.stdout, indent=1)
PYEOF
}

# Judges every seed of a cell on whether it downloaded the whole image. A
# prefetching cell that falls short is set aside and stops the campaign,
# because every later cell would fall short for the same reason
check_prefetch() {
  local csv="$1" image="$2" verdicts require=()
  verdicts="${csv%.csv}.prefetch-check.csv"
  [[ "${PREFETCH}" == "all" ]] && require=(--require)
  if ! python3 "${REPO_ROOT}/scripts/check-prefetch-observed.py" "${require[@]}" \
      --cell "${csv}" --drain "${csv%.csv}.drain.csv" --manifest "$(manifest_path "${image}")" \
      --min-fraction "${MIN_PREFETCH_FRACTION}" >"${verdicts}"; then
    cat "${verdicts}"
    mv "${csv}" "${csv%.csv}.void-no-prefetch"
    echo "### campaign: VOID ${image} with prefetch all did not download the whole image; stopping"
    exit 1
  fi
  cat "${verdicts}"
  if [[ "${PREFETCH}" == "off" ]] && grep -q ',yes$' "${verdicts}"; then
    echo "### campaign: WARNING ${image} with prefetch off downloaded most of the image anyway"
  fi
}

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
    local quiet_env=()
    if [[ "${QUIET_WAIT}" == "1" ]]; then
      quiet_env=(DRAIN_RECORD="${csv%.csv}.drain.csv"
        PRE_SCALEOUT_HOOK="NODE_SOURCE=kubernetes ${REPO_ROOT}/scripts/wait-for-quiet-nodes.sh")
    fi
    if ! env "${quiet_env[@]}" CACHE_IP=10.0.0.50:8080 \
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
    [[ "${QUIET_WAIT}" == "1" ]] && check_prefetch "${csv}" "${image}"
    echo "### campaign: accepted ${csv##*/}"
    return 0
  done
  echo "### campaign: GAVE UP config ${config} ${image} after ${ATTEMPTS} attempts"
  return 1
}

if [[ "${QUIET_WAIT}" == "1" ]]; then
  for entry in ${IMAGES}; do
    image="${entry%%:*}"
    save_manifest "${image}"
    echo "### campaign: ${image}:${NYDUS_TAG} holds $(python3 "${REPO_ROOT}/scripts/check-prefetch-observed.py" \
      --print-blob-bytes --manifest "$(manifest_path "${image}")") bytes of data blobs"
  done
fi

for config in ${CONFIGS}; do
  echo "### campaign: switching to config ${config}"
  case "${config}" in
    1) tag="oci" ;;
    2s) tag="${NYDUS_TAG}" ;;
    *) tag="nydus-none" ;;
  esac
  if [[ "${config}" == "2s" ]]; then
    ACR="${ACR}" NYDUS_TAG="${NYDUS_TAG}" PREFETCH="${PREFETCH}" \
      "${REPO_ROOT}/scripts/config-switch-aks.sh" "${config}"
  else
    ACR="${ACR}" NYDUS_TAG=nydus-none PREFETCH=off "${REPO_ROOT}/scripts/config-switch-aks.sh" "${config}"
  fi

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
