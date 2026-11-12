#!/usr/bin/env bash
set -euo pipefail

# Measures what nydusd's whole-image prefetch changes for a cold start. The
# stock snapshotter ships with prefetch switched on, while the published nydus
# cells ran with it off, so this runs stock nydusd (config 2s) both ways on the
# same rig and conversion, next to the cluster cache (config 4) with prefetch
# off. Configs 2 and 4 read through the chunk proxy, and the configuration the
# snapshotter writes for them never starts nydusd's prefetch workers, so
# prefetch on is refused there instead of quietly measuring prefetch off again
#
# A prefetching cell is kept only if every seed node really downloaded the
# whole image, counted from the node's network counters. Between the seed and
# the scale-out the campaign waits for the link to go quiet, so the cold starts
# it measures never share the link with the seed's background download
#
# Usage: [MODES=..] [CONFIGS=..] [IMAGES=..] [NYDUS_TAG=..] [RTT=..] [RATE=..]
#        [REPS=..] [PREFIX=..] [DRY_RUN=1] [FORCE=1] benchmark-prefetch-sensitivity.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODES="${MODES:-off all}"
CONFIGS="${CONFIGS:-2s}"
IMAGES="${IMAGES:-fn-python-hello:8080 fn-python-ml:8080 fn-pytorch-cpu:8080}"
NYDUS_TAG="${NYDUS_TAG:-nydus-none}"
RTT="${RTT:-80}"
RATE="${RATE:-600mbit}"
REPS="${REPS:-5}"
PREFIX="${PREFIX:-prefetch-stock}"
MIN_PREFETCH_FRACTION="${MIN_PREFETCH_FRACTION:-0.8}"
REGISTRY="registry.localhost:5000"
OUTDIR="${OUTDIR:-${REPO_ROOT}/analysis/results/benchmarks}"
PROVENANCE="${OUTDIR}/${PREFIX}-provenance.csv"

CELLS_RUN=0
CELLS_SKIPPED=0

# Only stock nydusd, alone or behind Dragonfly, reads its own prefetch
# settings and compressed chunks; the chunk proxy configurations do neither
reads_registry_directly() {
  case "$1" in
    2s|3) return 0 ;;
    *) return 1 ;;
  esac
}

# Refuses every cell the rig cannot measure before any of them starts, so a
# wrong setting costs seconds instead of an afternoon of rig time
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
        "only stock nydusd reads compressed chunks" >&2
      return 1
    fi
  done
}

# The conversion tag is part of every file name, so a cell measured on one
# conversion is never mistaken for the same cell on another, nor for a file
# from the earlier campaign whose names carried no tag
cell_stem() {
  local config="$1" image="$2" mode="$3"
  echo "${OUTDIR}/${PREFIX}-config${config}-${image}-${NYDUS_TAG}-prefetch${mode}-rtt${RTT}ms-rate${RATE}"
}

# Only the measurement itself ends in its bare timestamp. A failed cell leaves
# its drain record and verdict behind, and they must not count as measured
cell_measured() {
  [[ -z "${FORCE:-}" ]] || return 1
  compgen -G "$(cell_stem "$@")-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9].csv" >/dev/null
}

manifest_path() {
  echo "${OUTDIR}/${PREFIX}-manifest-$1-${NYDUS_TAG}.json"
}

validate_cells

# The plan lists every cell in the order the campaign runs them, and a dry run
# stops right after it without touching the rig
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

# Leave the rig in its published state whatever happens: the cluster cache in
# the path, prefetch off and the registry link unshaped
restore() {
  rm -rf "${SCRATCH}"
  echo "### prefetch: restoring config 4 with prefetch off"
  PREFETCH=off "${REPO_ROOT}/scripts/config-switch.sh" 4 >/dev/null 2>&1 \
    || echo "### prefetch: could not restore config 4; run config-switch.sh 4 by hand" >&2
  "${REPO_ROOT}/scripts/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap restore EXIT

mapfile -t NODES < <("${REPO_ROOT}/scripts/cluster-nodes.sh")
if [[ "${#NODES[@]}" -eq 0 ]]; then
  echo "prefetch: no cluster nodes found; is the k3d cluster running?" >&2
  exit 1
fi

# The registry is reached on its cluster-network address, the way the nodes
# reach it, because the host port answers through docker's proxy
REGISTRY_IP="$(docker inspect \
  -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' \
  registry.localhost)"

# The prefetch mode lives in a file on the nodes, and any reinstall between
# two cells would quietly put the published mode back without failing
# anything. Every cell is therefore measured only after the nodes are seen to
# carry the mode under test
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

# Fetching every manifest first proves the conversion under test is in the
# rig's registry, which a rebuilt rig loses for conversions that only ever
# lived there, and it gives the size a prefetching node has to download
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
    blob_bytes="$(python3 "${REPO_ROOT}/scripts/check-prefetch-observed.py" --print-blob-bytes --manifest "${path}")"
    echo "### prefetch: ${image}:${NYDUS_TAG} holds ${blob_bytes} bytes of data blobs"
  done
}

# The TCP handshake pays the shaped registry link exactly once, so its time
# proves the emulated round trip is in force. A first request warms the
# address lookup so the measured one pays for the handshake alone
arm_shaping() {
  local connect_seconds
  "${REPO_ROOT}/scripts/registry-netem.sh" on "${RTT}ms" "${RATE}"
  curl -o /dev/null -s -m 15 "http://${REGISTRY_IP}:5000/v2/" || true
  connect_seconds="$(curl -o /dev/null -s -m 15 -w '%{time_connect}' "http://${REGISTRY_IP}:5000/v2/")"
  if ! awk -v connect="${connect_seconds}" -v rtt="${RTT}" \
      'BEGIN { ms = connect * 1000; exit !(ms >= rtt - 5 && ms <= rtt + 30) }'; then
    echo "prefetch: connect took ${connect_seconds}s, which does not reflect ${RTT}ms of shaping" >&2
    return 1
  fi
  [[ -f "${PROVENANCE}" ]] || echo "timestamp,rtt_ms,rate,measured_connect_seconds" >"${PROVENANCE}"
  echo "$(date -u +%FT%TZ),${RTT},${RATE},${connect_seconds}" >>"${PROVENANCE}"
  echo "### prefetch: link shaped to ${RTT}ms and ${RATE} (connect ${connect_seconds}s)"
}

# Every cell is judged on whether its seed nodes downloaded the whole image.
# A prefetching cell that falls short is set aside and stops the campaign,
# because every later prefetching cell would fall short for the same reason;
# a cell without prefetch that does not fall short is only reported
check_prefetch() {
  local csv="$1" drain="$2" image="$3" mode="$4" verdicts require=()
  verdicts="${csv%.csv}.prefetch-check.csv"
  [[ "${mode}" == "all" ]] && require=(--require)

  if ! python3 "${REPO_ROOT}/scripts/check-prefetch-observed.py" "${require[@]}" \
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
  # The configuration switch reinstalls nydusd on every node and takes the
  # prefetch mode from the environment, so the mode has to travel with it
  export PREFETCH="${mode}"

  for config in ${CONFIGS}; do
    echo "### prefetch: switching to config ${config} with prefetch ${mode}"
    "${REPO_ROOT}/scripts/config-switch.sh" "${config}"
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
        "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"

      # A failed cell surrenders its CSV and the campaign moves on; the
      # partial file is set aside so a rerun repeats the whole cell
      csv="$(cell_stem "${config}" "${image}" "${mode}")-$(date +%Y%m%dT%H%M%S).csv"
      drain="${csv%.csv}.drain.csv"
      if ! SERVICE="${image}" WORKLOAD="${workload}" DRAIN_RECORD="${drain}" \
          PRE_SCALEOUT_HOOK="${REPO_ROOT}/scripts/wait-for-quiet-nodes.sh" \
          "${REPO_ROOT}/scripts/benchmark-scaleout.sh" "${config}" "${REPS}" "${csv}"; then
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
  echo "### prefetch campaign measured nothing: all ${CELLS_SKIPPED} cells already had a CSV" >&2
  echo "### move them aside, or rerun with FORCE=1 to overwrite" >&2
  exit 1
fi

echo "### prefetch campaign complete: ${CELLS_RUN} cells measured, ${CELLS_SKIPPED} skipped"
