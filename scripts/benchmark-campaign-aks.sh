#!/usr/bin/env bash
set -euo pipefail

# Runs the full configuration x corpus-image scale-out campaign on AKS.
# One CSV lands per cell; cells that already have a CSV are skipped, so an
# interrupted campaign resumes by rerunning this script. Configurations are
# the outer loop so containerd is switched once per configuration, not once
# per cell.
# Usage: ACR=<name>.azurecr.io scripts/benchmark-campaign-aks.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACR="${ACR:?set ACR to the ACR login server}"
REPS="${REPS:-5}"
CONFIGS="${CONFIGS:-1 2 4}"
IMAGES="${IMAGES:-fn-go-hello:8080 fn-python-hello:8080 fn-java-springboot:8080 fn-node-api:8080 fn-python-ml:8080 fn-pytorch-cpu:8080}"
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "${SCRATCH}"' EXIT

mkdir -p "${OUTDIR}"

for config in ${CONFIGS}; do
  echo "### campaign: switching to config ${config}"
  ACR="${ACR}" "${REPO_ROOT}/scripts/config-switch-aks.sh" "${config}"

  case "${config}" in
    1) tag="oci" ;;
    *) tag="nydus-none" ;;
  esac

  for entry in ${IMAGES}; do
    image="${entry%%:*}"
    port="${entry##*:}"

    if ls "${OUTDIR}/aks-scaleout-config${config}-${image}-"*.csv >/dev/null 2>&1; then
      echo "### campaign: skip config ${config} ${image} (CSV exists)"
      continue
    fi

    echo "### campaign: config ${config} image ${image} (${REPS} reps)"
    workload="${SCRATCH}/workload-${config}-${image}.yaml"
    ACR="${ACR}" SERVICE="${image}" TAG="${tag}" PORT="${port}" \
      "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"

    # A failed cell surrenders its CSV and the campaign moves on; the
    # partial file is set aside so a rerun repeats the whole cell
    csv="${OUTDIR}/aks-scaleout-config${config}-${image}-$(date +%Y%m%dT%H%M%S).csv"
    if ! CACHE_IP=10.0.0.50:8080 \
        RESET_SCRIPT="${REPO_ROOT}/scripts/cluster-reset-aks.sh" \
        ACR="${ACR}" SERVICE="${image}" WORKLOAD="${workload}" \
        "${REPO_ROOT}/scripts/benchmark-scaleout.sh" "${config}" "${REPS}" "${csv}"; then
      echo "### campaign: FAILED config ${config} ${image}"
      [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
      kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
    fi
  done
done

echo "### campaign complete"
