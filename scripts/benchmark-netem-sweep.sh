#!/usr/bin/env bash
set -euo pipefail

# Sweeps emulated registry round-trip time across the local scale-out
# benchmark so the cache benefit over distance becomes a curve instead of
# two isolated points. The outer loop switches the benchmark configuration
# once, the middle loop re-arms the registry netem shaping per round-trip
# point, and the inner loop runs one cell per corpus image. Cells that
# already have a CSV are skipped, so an interrupted sweep resumes by
# rerunning the script with the same environment.
#
# Configuration 1, eager OCI pulling, is supported alongside the nydus
# configurations: the bandwidth campaign established that a true OCI cold
# start can be isolated locally, so this driver renders the OCI tag and
# checks the pull actually crossed the shaped link the same way the rate
# sweep does.
# Usage: [RTTS=..] [RATE=..] [CONFIGS=..] [IMAGES=..] [REPS=..] [PREFIX=..] benchmark-netem-sweep.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RTTS="${RTTS:-0 10 25 50 80 100}"
RATE="${RATE:-600mbit}"
CONFIGS="${CONFIGS:-4 2}"
IMAGES="${IMAGES:-fn-python-hello:8080 fn-python-ml:8080}"
REPS="${REPS:-5}"
PREFIX="${PREFIX:-netem-sweep}"
REGISTRY="registry.localhost:5000"
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
PROVENANCE="${OUTDIR}/netem-sweep-provenance.csv"
SCRATCH="$(mktemp -d)"

# A sweep that skips every cell has measured nothing, which is almost always
# a stale results directory rather than a genuine resume. Counting the two
# outcomes lets the run fail loudly at the end instead of printing "complete"
# after three seconds
CELLS_RUN=0
CELLS_SKIPPED=0

# Leave the registry unshaped whenever the sweep stops; the next invocation
# re-arms the shaping for the point it resumes at
cleanup() {
  rm -rf "${SCRATCH}"
  "${REPO_ROOT}/scripts/registry-netem.sh" off >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "${OUTDIR}"
[[ -f "${PROVENANCE}" ]] || \
  echo "timestamp,config,rtt_ms,rate,measured_connect_seconds" >"${PROVENANCE}"

# The TCP handshake pays the shaped registry egress exactly once, so the
# connect time doubles as proof that the emulated round trip is in force.
# The registry must be reached on its cluster-network address: the host
# port goes through the docker proxy, which accepts instantly and would
# hide the shaping. A throwaway request first warms the neighbor cache so
# the measured handshake does not also pay for address resolution
REGISTRY_IP="$(docker inspect \
  -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' \
  registry.localhost)"
measured_connect() {
  curl -o /dev/null -s -m 15 "http://${REGISTRY_IP}:5000/v2/" || true
  curl -o /dev/null -s -m 15 -w '%{time_connect}' "http://${REGISTRY_IP}:5000/v2/"
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

  local expected
  expected="$("${REPO_ROOT}/scripts/cluster-nodes.sh" | grep -c . || true)"
  if [[ "${expected}" == "0" ]]; then
    echo "sweep: no cluster nodes found; is the k3d cluster running?" >&2
    return 1
  fi
  running="$(grep -c "running" <<<"${status}")" || true
  if [[ "${running}" != "${expected}" ]]; then
    echo "sweep: the nydus snapshotter runs on ${running} of ${expected} nodes" >&2
    echo "${status}" >&2
    return 1
  fi
}

# The cold-pull guard for configuration 1 lives next to the other sweep drivers
# shellcheck source=cold-guard.sh
source "${REPO_ROOT}/scripts/cold-guard.sh"

# The netem sweep holds one rate for the whole run rather than sweeping it,
# so the floor for the cold-pull assertion is computed once rather than per point
CONFIGURED_BPS="$(rate_bytes_per_second "${RATE}")"

for config in ${CONFIGS}; do
  echo "### sweep: switching to config ${config}"
  "${REPO_ROOT}/scripts/config-switch.sh" "${config}"

  case "${config}" in
    1) tag="oci" ;;
    # NYDUS_TAG picks another conversion of the same image, such as the
    # compressed one that stock nydusd can read but the cache proxy cannot
    *) tag="${NYDUS_TAG:-nydus-none}"; require_snapshotter ;;
  esac

  for rtt in ${RTTS}; do
    "${REPO_ROOT}/scripts/registry-netem.sh" on "${rtt}ms" "${RATE}"
    connect_seconds="$(measured_connect)"

    # Refuse to measure against an unshaped registry: the handshake must
    # reflect the requested round trip or every cell at this point is invalid
    if ! awk -v connect="${connect_seconds}" -v rtt="${rtt}" \
        'BEGIN { ms = connect * 1000; exit !(ms >= rtt - 5 && ms <= rtt + 30) }'; then
      echo "### sweep: ABORT config ${config} rtt ${rtt}ms:" \
        "connect ${connect_seconds}s does not reflect the shaping" >&2
      exit 1
    fi

    echo "$(date -u +%FT%TZ),${config},${rtt},${RATE},${connect_seconds}" >>"${PROVENANCE}"
    echo "### sweep: config ${config} rtt ${rtt}ms rate ${RATE} (connect ${connect_seconds}s)"

    for entry in ${IMAGES}; do
      image="${entry%%:*}"
      port="${entry##*:}"

      if [[ -z "${FORCE:-}" ]] && ls "${OUTDIR}/${PREFIX}-config${config}-${image}-rtt${rtt}ms-rate${RATE}-"*.csv >/dev/null 2>&1; then
        echo "### sweep: skip config ${config} ${image} rtt ${rtt}ms (CSV exists)"
        CELLS_SKIPPED=$((CELLS_SKIPPED + 1))
        continue
      fi
      CELLS_RUN=$((CELLS_RUN + 1))

      workload="${SCRATCH}/workload-${config}-${image}.yaml"
      ACR="${REGISTRY}" SERVICE="${image}" TAG="${tag}" PORT="${port}" \
        "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"

      # A failed cell surrenders its CSV and the sweep moves on; the partial
      # file is set aside so a rerun repeats the whole cell
      csv="${OUTDIR}/${PREFIX}-config${config}-${image}-rtt${rtt}ms-rate${RATE}-$(date +%Y%m%dT%H%M%S).csv"
      if SERVICE="${image}" WORKLOAD="${workload}" \
          "${REPO_ROOT}/scripts/benchmark-scaleout.sh" "${config}" "${REPS}" "${csv}"; then
        if [[ "${config}" == "1" && "${REPS}" != "0" ]]; then
          assert_cell_cold "${csv}" "${image}" "${CONFIGURED_BPS}"
        fi
      else
        echo "### sweep: FAILED config ${config} ${image} rtt ${rtt}ms"
        [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
        kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      fi
    done
  done
done

if (( CELLS_RUN == 0 )); then
  echo "### sweep measured nothing: all ${CELLS_SKIPPED} cells already had a CSV in ${OUTDIR}" >&2
  echo "### results from a different machine living in that directory will do this" >&2
  echo "### move them aside, or rerun with FORCE=1 to overwrite" >&2
  exit 1
fi

echo "### sweep complete: ${CELLS_RUN} cells measured, ${CELLS_SKIPPED} skipped"
