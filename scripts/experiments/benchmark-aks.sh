#!/usr/bin/env bash
# Runs the scale-out experiment over every configuration and corpus image on AKS, one CSV per cell, and resumes where an interrupted run stopped
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ACR="${ACR:?set ACR to the ACR login server}"
NYDUS_TAG="${NYDUS_TAG:-nydus-none}"
PREFETCH="${PREFETCH:-off}"
REPS="${REPS:-5}"
CONFIGS="${CONFIGS:-1 2 4}"
IMAGES="${IMAGES:-fn-go-hello:8080 fn-python-hello:8080 fn-java-springboot:8080 fn-node-api:8080 fn-python-ml:8080 fn-pytorch-cpu:8080}"
PREFIX="${PREFIX:-aks}"
ATTEMPTS="${ATTEMPTS:-3}"
RATE_IMAGE="${RATE_IMAGE:-fn-pytorch-cpu}"
SERVICE_ACR="${SERVICE_ACR:-${ACR}}"
INFRASTRUCTURE="${INFRASTRUCTURE:-${SERVICE_ACR}/chunk-proxy:0.2.1,${SERVICE_ACR}/nydus-cluster-cache:0.2.0,${SERVICE_ACR}/nydus-node-installer:0.1.1}"
OUTDIR="$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" "${PREFIX}")"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "${SCRATCH}"' EXIT

mkdir -p "${OUTDIR}"
export ACR REPO_ROOT

if [[ "${NYDUS_TAG}" != "nydus-none" || "${PREFETCH}" != "off" ]]; then
  for config in ${CONFIGS}; do
    if [[ "${config}" != "2s" ]]; then
      echo "NYDUS_TAG=${NYDUS_TAG} PREFETCH=${PREFETCH} only apply to config 2s, not ${config}" >&2
      exit 1
    fi
  done
fi

FACT_TAGS="oci,nydus-none"
[[ "${NYDUS_TAG}" != "nydus-none" ]] && FACT_TAGS+=",${NYDUS_TAG}"
export FACT_TAGS

QUIET_WAIT="${QUIET_WAIT:-0}"
[[ "${PREFETCH}" == "all" ]] && QUIET_WAIT=1
MIN_PREFETCH_FRACTION="${MIN_PREFETCH_FRACTION:-0.8}"

manifest_path() {
  echo "${OUTDIR}/${PREFIX}-manifest-$1.json"
}

save_manifest() {
  local image="$1"
  python3 - "${REPO_ROOT}/scripts/experiments/acr-image-facts.py" "${ACR}" "${image}" "${NYDUS_TAG}" \
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

check_prefetch() {
  local csv="$1" image="$2" verdicts require=()
  verdicts="${csv%.csv}.prefetch-check.csv"
  [[ "${PREFETCH}" == "all" ]] && require=(--require)
  if ! python3 "${REPO_ROOT}/scripts/experiments/check-prefetch-observed.py" "${require[@]}" \
      --cell "${csv}" --drain "${csv%.csv}.drain.csv" --manifest "$(manifest_path "${image}")" \
      --min-fraction "${MIN_PREFETCH_FRACTION}" >"${verdicts}"; then
    cat "${verdicts}"
    mv "${csv}" "${csv%.csv}.void-no-prefetch"
    echo "### experiment: VOID ${image} with prefetch all did not download the whole image; stopping"
    exit 1
  fi
  cat "${verdicts}"
  if [[ "${PREFETCH}" == "off" ]] && grep -q ',yes$' "${verdicts}"; then
    echo "### experiment: WARNING ${image} with prefetch off downloaded most of the image anyway"
  fi
}

image_names=""
for entry in ${IMAGES}; do
  image_names+=" ${entry%%:*}"
done
export IMAGE_FACTS="${OUTDIR}/${PREFIX}-image-facts.json"
if [[ ! -s "${IMAGE_FACTS}" ]]; then
  # shellcheck disable=SC2086
  python3 "${REPO_ROOT}/scripts/experiments/acr-image-facts.py" "--infrastructure=${INFRASTRUCTURE}" \
    "${ACR}" ${image_names} >"${IMAGE_FACTS}"
fi
# shellcheck source=cold-guard-aks.sh
source "${REPO_ROOT}/scripts/experiments/cold-guard-aks.sh"
export PRE_SEED_CHECK="source '${REPO_ROOT}/scripts/experiments/cold-guard-aks.sh' && assert_nodes_hold_no_corpus"

export CAPTURE_LOGS=1

run_cell() {
  local config="$1" image="$2" workload="$3" attempt csv
  for attempt in $(seq 1 "${ATTEMPTS}"); do
    csv="${OUTDIR}/${PREFIX}-scaleout-config${config}-${image}-$(date +%Y%m%dT%H%M%S).csv"
    echo "### experiment: config ${config} image ${image} attempt ${attempt}/${ATTEMPTS}"

    local quiet_env=()
    if [[ "${QUIET_WAIT}" == "1" ]]; then
      quiet_env=(DRAIN_RECORD="${csv%.csv}.drain.csv"
        PRE_SCALEOUT_HOOK="NODE_SOURCE=kubernetes ${REPO_ROOT}/scripts/experiments/wait-for-quiet-nodes.sh")
    fi
    if ! env "${quiet_env[@]}" CACHE_IP=10.0.0.50:8080 \
        RESET_SCRIPT="${REPO_ROOT}/scripts/cluster/cluster-reset-aks.sh" \
        ACR="${ACR}" SERVICE="${image}" WORKLOAD="${workload}" \
        "${REPO_ROOT}/scripts/experiments/benchmark-scale-out.sh" "${config}" "${REPS}" "${csv}"; then
      echo "### experiment: FAILED config ${config} ${image}"
      [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
      kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      continue
    fi

    if [[ "${config}" == "1" ]]; then
      if ! assert_cell_cold "${csv}" "${image}" "${PULL_RATE_BPS}" \
          || ! assert_cell_received_bytes "${csv}" "${image}"; then
        echo "### experiment: COLD GUARD FAILED config ${config} ${image}"
        continue
      fi
    fi
    [[ "${QUIET_WAIT}" == "1" ]] && check_prefetch "${csv}" "${image}"
    echo "### experiment: accepted ${csv##*/}"
    return 0
  done
  echo "### experiment: GAVE UP config ${config} ${image} after ${ATTEMPTS} attempts"
  return 1
}

if [[ "${QUIET_WAIT}" == "1" ]]; then
  for entry in ${IMAGES}; do
    image="${entry%%:*}"
    save_manifest "${image}"
    echo "### experiment: ${image}:${NYDUS_TAG} holds $(python3 "${REPO_ROOT}/scripts/experiments/check-prefetch-observed.py" \
      --print-blob-bytes --manifest "$(manifest_path "${image}")") bytes of data blobs"
  done
fi

for config in ${CONFIGS}; do
  echo "### experiment: switching to config ${config}"
  case "${config}" in
    1) tag="oci" ;;
    2s) tag="${NYDUS_TAG}" ;;
    *) tag="nydus-none" ;;
  esac
  if [[ "${config}" == "2s" ]]; then
    ACR="${ACR}" NYDUS_TAG="${NYDUS_TAG}" PREFETCH="${PREFETCH}" \
      "${REPO_ROOT}/scripts/cluster/config-switch-aks.sh" "${config}"
  else
    ACR="${ACR}" NYDUS_TAG=nydus-none PREFETCH=off "${REPO_ROOT}/scripts/cluster/config-switch-aks.sh" "${config}"
  fi

  if [[ "${config}" == "1" && -z "${PULL_RATE_BPS:-}" ]]; then
    ACR="${ACR}" "${REPO_ROOT}/scripts/cluster/cluster-reset-aks.sh" >/dev/null
    assert_nodes_hold_no_corpus
    rate_line="$(measure_pull_rate "${RATE_IMAGE}")"
    assert_nodes_hold_no_corpus
    read -r PULL_RATE_BPS rate_bytes rate_seconds <<<"${rate_line}"
    if [[ -z "${PULL_RATE_BPS}" || "${PULL_RATE_BPS}" == "0" ]]; then
      echo "### experiment: FAILED reference fetch, no pull floor can be set" >&2
      exit 1
    fi
    export PULL_RATE_BPS
    rate_log="${OUTDIR}/${PREFIX}-pull-rate.csv"
    [[ -f "${rate_log}" ]] || echo "measured_at,image,compressed_bytes,fetch_seconds,bytes_per_second" >"${rate_log}"
    echo "$(date -u +%FT%TZ),${RATE_IMAGE},${rate_bytes},${rate_seconds},${PULL_RATE_BPS}" \
      >>"${rate_log}"
    echo "### experiment: reference fetch ${RATE_IMAGE} ${rate_bytes} B in ${rate_seconds}s = ${PULL_RATE_BPS} B/s"
  fi

  for entry in ${IMAGES}; do
    image="${entry%%:*}"
    port="${entry##*:}"

    if ls "${OUTDIR}/${PREFIX}-scaleout-config${config}-${image}-"*.csv >/dev/null 2>&1; then
      echo "### experiment: skip config ${config} ${image} (CSV exists)"
      continue
    fi

    workload="${SCRATCH}/workload-${config}-${image}.yaml"
    ACR="${ACR}" SERVICE="${image}" TAG="${tag}" PORT="${port}" \
      "${REPO_ROOT}/scripts/cluster/render-workload.sh" >"${workload}"
    run_cell "${config}" "${image}" "${workload}" || true
  done
done

echo "### experiment complete"
