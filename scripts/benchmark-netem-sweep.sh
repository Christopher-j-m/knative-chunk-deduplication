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
# Configuration 1 is deliberately absent: the k3d nodes share one containerd
# content store, so a true OCI cold start cannot be isolated locally; the
# OCI distance behavior comes from the real cross-region anchors instead.
# Usage: [RTTS=..] [RATE=..] [CONFIGS=..] [IMAGES=..] [REPS=..] benchmark-netem-sweep.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RTTS="${RTTS:-0 10 25 50 80 100}"
RATE="${RATE:-600mbit}"
CONFIGS="${CONFIGS:-4 2}"
IMAGES="${IMAGES:-fn-python-hello:8080 fn-python-ml:8080}"
REPS="${REPS:-5}"
REGISTRY="registry.localhost:5000"
OUTDIR="${REPO_ROOT}/analysis/results/benchmarks"
PROVENANCE="${OUTDIR}/netem-sweep-provenance.csv"
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

for config in ${CONFIGS}; do
  echo "### sweep: switching to config ${config}"
  "${REPO_ROOT}/scripts/config-switch.sh" "${config}"

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

      if ls "${OUTDIR}/netem-sweep-config${config}-${image}-rtt${rtt}ms-rate${RATE}-"*.csv >/dev/null 2>&1; then
        echo "### sweep: skip config ${config} ${image} rtt ${rtt}ms (CSV exists)"
        continue
      fi

      workload="${SCRATCH}/workload-${config}-${image}.yaml"
      ACR="${REGISTRY}" SERVICE="${image}" TAG="nydus-none" PORT="${port}" \
        "${REPO_ROOT}/scripts/render-workload.sh" >"${workload}"

      # A failed cell surrenders its CSV and the sweep moves on; the partial
      # file is set aside so a rerun repeats the whole cell
      csv="${OUTDIR}/netem-sweep-config${config}-${image}-rtt${rtt}ms-rate${RATE}-$(date +%Y%m%dT%H%M%S).csv"
      if ! SERVICE="${image}" WORKLOAD="${workload}" \
          "${REPO_ROOT}/scripts/benchmark-scaleout.sh" "${config}" "${REPS}" "${csv}"; then
        echo "### sweep: FAILED config ${config} ${image} rtt ${rtt}ms"
        [[ -f "${csv}" ]] && mv "${csv}" "${csv%.csv}.failed-partial"
        kubectl delete ksvc "${image}" --ignore-not-found --timeout=120s || true
      fi
    done
  done
done

echo "### sweep complete"
