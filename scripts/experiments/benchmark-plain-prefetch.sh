#!/usr/bin/env bash
# Measures plain nydusd with whole-image prefetch off and on next to the cluster cache on the same rig
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODES="${MODES:-off all}"
CONFIGS="${CONFIGS:-2s}"
IMAGES="${IMAGES:-fn-python-hello:8080 fn-python-ml:8080 fn-pytorch-cpu:8080}"
NYDUS_TAG="${NYDUS_TAG:-nydus-none}"
RTT="${RTT:-80}"
RATE="${RATE:-600mbit}"
REPS="${REPS:-5}"
PREFIX="${PREFIX:-plain-prefetch}"
MIN_PREFETCH_FRACTION="${MIN_PREFETCH_FRACTION:-0.8}"
REGISTRY="registry.localhost:5000"
OUTDIR="${OUTDIR:-$(python3 "${REPO_ROOT}/scripts/summaries/result_paths.py" "${PREFIX}")}"
NETWORK_CHECK="${OUTDIR}/${PREFIX}-network-check.csv"

CELLS_RUN=0
CELLS_SKIPPED=0

reads_registry_directly() {
  case "$1" in
    2s|3) return 0 ;;
    *) return 1 ;;
  esac
}

validate_cells() {
  local mode config
  for mode in ${MODES}; do
    case "${mode}" in
      off|all) ;;
      *) echo "prefetch: unknown mode ${mode}; choose off or all" >&2; return 1 ;;
    esac
  done

  for config in ${CONFIGS}; do
    case "${config}" in
      2|2s|3|4) ;;
      *) echo "prefetch: config ${config} runs no nydusd; choose 2, 2s, 3 or 4" >&2; return 1 ;;
    esac
    if [[ " ${MODES} " == *" all "* ]] && ! reads_registry_directly "${config}"; then
      echo "prefetch: config ${config} reads through the chunk proxy, whose nydusd never starts" \
        "its prefetch workers, so prefetch all would only repeat prefetch off" >&2
      return 1
    fi
    if [[ "${NYDUS_TAG}" != "nydus-none" ]] && ! reads_registry_directly "${config}"; then
      echo "prefetch: config ${config} cannot read the ${NYDUS_TAG} conversion;" \
        "only plain nydusd reads compressed chunks" >&2
      return 1
    fi
  done
}

cell_stem() {
  local config="$1" image="$2" mode="$3"
  echo "${OUTDIR}/${PREFIX}-config${config}-${image}-${NYDUS_TAG}-prefetch${mode}-rtt${RTT}ms-rate${RATE}"
}

cell_measured() {
  [[ -z "${FORCE:-}" ]] || return 1
  compgen -G "$(cell_stem "$@")-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9].csv" >/dev/null
}

manifest_path() {
  echo "${OUTDIR}/${PREFIX}-manifest-$1-${NYDUS_TAG}.json"
}

validate_cells

for mode in ${MODES}; do
  for config in ${CONFIGS}; do
    for entry in ${IMAGES}; do
      image="${entry%%:*}"
      verdict="measure"
      cell_measured "${config}" "${image}" "${mode}" && verdict="skip"
      echo "plan: ${verdict} config=${config} image=${image} tag=${NYDUS_TAG} prefetch=${mode}"
    done
  done
done
if [[ -n "${DRY_RUN:-}" ]]; then
  exit 0
fi

SCRATCH="$(mktemp -d)"

restore() {
  rm -rf "${SCRATCH}"
  echo "### prefetch: restoring config 4 with prefetch off"
  PREFETCH=off "${REPO_ROOT}/scripts/cluster/config-switch.sh" 4 >/dev/null 2>&1 \
    || echo "### prefetch: could not restore config 4; run config-switch.sh 4 by hand" >&2
  "${REPO_ROOT}/scripts/cluster/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap restore EXIT

mapfile -t NODES < <("${REPO_ROOT}/scripts/cluster/cluster-nodes.sh")
if [[ "${#NODES[@]}" -eq 0 ]]; then
  echo "prefetch: no cluster nodes found; is the k3d cluster running?" >&2
  exit 1
fi

REGISTRY_IP="$(docker inspect \
  -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' \
  registry.localhost)"

assert_prefetch_mode() {
  local wanted="$1" expected node actual
  case "${wanted}" in
    off) expected=false ;;
    all) expected=true ;;
  esac

  for node in "${NODES[@]}"; do
    actual="$(docker exec "${node}" cat /etc/nydus/nydusd-config.json 2>/dev/null \
      | python3 -c 'import json,sys; print(str(json.load(sys.stdin)["fs_prefetch"]["prefetch_all"]).lower())' 2>/dev/null || echo unknown)"
    if [[ "${actual}" != "${expected}" ]]; then
      echo "prefetch: ${node} reports prefetch_all=${actual}, want ${expected} for mode ${wanted}" >&2
      return 1
    fi
  done
}

save_manifests() {
  local entry image path blob_bytes
  for entry in ${IMAGES}; do
    image="${entry%%:*}"
    path="$(manifest_path "${image}")"
    if ! curl -sf -m 15 \
        -H "Accept: application/vnd.oci.image.manifest.v1+json" \
        -H "Accept: application/vnd.docker.distribution.manifest.v2+json" \
        "http://${REGISTRY_IP}:5000/v2/${image}/manifests/${NYDUS_TAG}" -o "${path}"; then
      echo "prefetch: ${image}:${NYDUS_TAG} is not in the rig's registry; convert and push it first" >&2
      return 1
    fi
    blob_bytes="$(python3 "${REPO_ROOT}/scripts/experiments/check-prefetch-observed.py" --print-blob-bytes --manifest "${path}")"
    echo "### prefetch: ${image}:${NYDUS_TAG} holds ${blob_bytes} bytes of data blobs"
  done
}

arm_shaping() {
  local connect_seconds
  "${REPO_ROOT}/scripts/cluster/registry-netem.sh" on "${RTT}ms" "${RATE}"
  curl -o /dev/null -s -m 15 "http://${REGISTRY_IP}:5000/v2/" || true
  connect_seconds="$(curl -o /dev/null -s -m 15 -w '%{time_connect}' "http://${REGISTRY_IP}:5000/v2/")"
  if ! awk -v connect="${connect_seconds}" -v rtt="${RTT}" \
      'BEGIN { ms = connect * 1000; exit !(ms >= rtt - 5 && ms <= rtt + 30) }'; then
    echo "prefetch: connect took ${connect_seconds}s, which does not reflect ${RTT}ms of shaping" >&2
    return 1
  fi
  [[ -f "${NETWORK_CHECK}" ]] || echo "timestamp,rtt_ms,rate,measured_connect_seconds" >"${NETWORK_CHECK}"
  echo "$(date -u +%FT%TZ),${RTT},${RATE},${connect_seconds}" >>"${NETWORK_CHECK}"
  echo "### prefetch: link shaped to ${RTT}ms and ${RATE} (connect ${connect_seconds}s)"
}

check_prefetch() {
  local csv="$1" drain="$2" image="$3" mode="$4" verdicts require=()
  verdicts="${csv%.csv}.prefetch-check.csv"
  [[ "${mode}" == "all" ]] && require=(--require)

  if ! python3 "${REPO_ROOT}/scripts/experiments/check-prefetch-observed.py" "${require[@]}" \
      --cell "${csv}" --drain "${drain}" --manifest "$(manifest_path "${image}")" \
      --min-fraction "${MIN_PREFETCH_FRACTION}" >"${verdicts}"; then
    cat "${verdicts}" >&2
    mv "${csv}" "${csv%.csv}.void-no-prefetch"
    echo "### prefetch: ABORT ${image} with prefetch all did not download the whole image;" \
      "the cell is set aside as ${csv%.csv}.void-no-prefetch" >&2
    exit 1
  fi
  cat "${verdicts}"

  if [[ "${mode}" == "off" ]] && grep -q ',yes$' "${verdicts}"; then
    echo "### prefetch: WARNING ${image} with prefetch off downloaded most of the image anyway" >&2
  fi
}

mkdir -p "${OUTDIR}"
save_manifests

for mode in ${MODES}; do
  export PREFETCH="${mode}"

  for config in ${CONFIGS}; do
    echo "### prefetch: switching to config ${config} with prefetch ${mode}"
    "${REPO_ROOT}/scripts/cluster/config-switch.sh" "${config}"
    assert_prefetch_mode "${mode}"
    arm_shaping

    for entry in ${IMAGES}; do
      image="${entry%%:*}"
      port="${entry##*:}"

      if cell_measured "${config}" "${image}" "${mode}"; then
        echo "### prefetch: skip config ${config} ${image} ${NYDUS_TAG} prefetch ${mode} (CSV exists)"
        CELLS_SKIPPED=$((CELLS_SKIPPED + 1))
        continue
      fi
      CELLS_RUN=$((CELLS_RUN + 1))

      workload="${SCRATCH}/workload-${config}-${image}.yaml"
      ACR="${REGISTRY}" SERVICE="${image}" TAG="${NYDUS_TAG}" PORT="${port}" \
        "${REPO_ROOT}/scripts/cluster/render-workload.sh" >"${workload}"

      csv="$(cell_stem "${config}" "${image}" "${mode}")-$(date +%Y%m%dT%H%M%S).csv"
      drain="${csv%.csv}.drain.csv"
      if ! SERVICE="${image}" WORKLOAD="${workload}" DRAIN_RECORD="${drain}" \
          PRE_SCALEOUT_HOOK="${REPO_ROOT}/scripts/experiments/wait-for-quiet-nodes.sh" \
          "${REPO_ROOT}/scripts/experiments/benchmark-scale-out.sh" "${config}" "${REPS}" "${csv}"; then
        echo "### prefetch: FAILED config ${config} ${image} ${NYDUS_TAG} prefetch ${mode}"
        [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
        kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
        continue
      fi

      check_prefetch "${csv}" "${drain}" "${image}" "${mode}"
    done
  done
done

if (( CELLS_RUN == 0 )); then
  echo "### prefetch experiment measured nothing: all ${CELLS_SKIPPED} cells already had a CSV" >&2
  echo "### move them aside, or rerun with FORCE=1 to overwrite" >&2
  exit 1
fi

echo "### prefetch experiment complete: ${CELLS_RUN} cells measured, ${CELLS_SKIPPED} skipped"
