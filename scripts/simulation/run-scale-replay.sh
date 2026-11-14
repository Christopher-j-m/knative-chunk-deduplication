#!/usr/bin/env bash
# Replays the production week in the simulator over cluster sizes, catalogs, placements, capacities and eviction policies
set -euo pipefail
cd "$(dirname "$0")/../.."

PYTHON="${PYTHON:-python3}"
OUT_DIR="${OUT_DIR:-analysis/results/simulation}"
TRACE="${TRACE:-analysis/traces/ibm-7d-all.csv}"
TOUCHED="analysis/results/chunks-live-touched.json"
COSTS="analysis/results/simulation/chunk-costs.csv"
NODES="${NODES:-3 10 100 1000 10000}"
POLICIES="lru,lru2,size,refcount,gdsf"
BLOCKS="${BLOCKS:-A B C D}"
BASELINE="${BASELINE:-plain}"
case "${BASELINE}" in
  plain|proxy) ;;
  *) echo "BASELINE must be plain or proxy, not ${BASELINE}" >&2; exit 1 ;;
esac

SUFFIX=""
if [[ "${BASELINE}" == "proxy" ]]; then
  SUFFIX="-proxy"
fi
REPLAY_FILE="${OUT_DIR}/scale-replay${SUFFIX}.csv"
CLASSES_FILE="${OUT_DIR}/scale-classes${SUFFIX}.csv"
LATENCY_FILE="${OUT_DIR}/scale-latency${SUFFIX}.csv"

export GOGC=50

PART="$(mktemp)"; PART_CLASSES="$(mktemp)"; PART_LATENCY="$(mktemp)"
BUILD_DIR="$(mktemp -d)"
SIMULATION_BINARY="${BUILD_DIR}/cache-simulation"
trap 'rm -rf "${PART}" "${PART_CLASSES}" "${PART_LATENCY}" "${BUILD_DIR}"' EXIT

go build -o "${SIMULATION_BINARY}" ./cmd/cache-simulation
mkdir -p "${OUT_DIR}"

wanted_block() {
  [[ " ${BLOCKS} " == *" $1 "* ]]
}

catalog_path() {
  case "$1" in
    6) echo "${TOUCHED}" ;;
    *) echo "analysis/traces/catalogs/n$1/working-sets.json" ;;
  esac
}

cost_args() {
  "${PYTHON}" - "$1" "$2" "${COSTS}" <<'EOF'
import sys, pandas as pd
rtt, side, path = int(sys.argv[1]), sys.argv[2], sys.argv[3]
matches = pd.read_csv(path).query("image == 'mean' and rtt_ms == @rtt")
if matches.empty:
    sys.exit(f"no mean cost row for rtt_ms={rtt} in {path}")
row = matches.iloc[0]
registry = row.plain_chunk_ms if side == "plain" else row.registry_chunk_ms
print(f"-warm-floor-ms {int(row.warm_floor_ms)} -registry-chunk-ms {registry} -cache-chunk-ms {row.cache_chunk_ms}")
EOF
}

first=1
if ! wanted_block A; then
  first=0
  for file in "${REPLAY_FILE}" "${CLASSES_FILE}" "${LATENCY_FILE}"; do
    if [[ ! -f "${file}" ]]; then
      echo "${file} is missing; run block A before the later blocks" >&2
      exit 1
    fi
  done
fi

append() {
  if [[ "${first}" = 1 ]]; then
    cat "${PART}" >"${REPLAY_FILE}"
    cat "${PART_CLASSES}" >"${CLASSES_FILE}"
    cat "${PART_LATENCY}" >"${LATENCY_FILE}"
    first=0
  else
    tail -n +2 "${PART}" >>"${REPLAY_FILE}"
    tail -n +2 "${PART_CLASSES}" >>"${CLASSES_FILE}"
    tail -n +2 "${PART_LATENCY}" >>"${LATENCY_FILE}"
  fi
}

run() {
  local nodes="$1" catalog="$2" placement="$3" rtt="$4" caps="$5" policies="$6" seeds="$7" map="$8"; shift 8
  echo "nodes=${nodes} catalog=${catalog} placement=${placement} rtt=${rtt} caps=${caps} policies=${policies} $*"

  local side="cached"
  if [[ " $* " == *" -no-cluster-cache "* && "${BASELINE}" == "plain" ]]; then
    side="plain"
  fi

  local costs
  costs="$(cost_args "${rtt}" "${side}")" || { echo "cost lookup failed for rtt=${rtt}" >&2; exit 1; }

  "${SIMULATION_BINARY}" -trace "${TRACE}" -working-sets "$(catalog_path "${catalog}")" \
    -nodes "${nodes}" -placement "${placement}" -capacities "${caps}" \
    -policies "${policies}" -seeds "${seeds}" -map "${map}" \
    ${costs} -latency-out "${PART_LATENCY}" -classes-out "${PART_CLASSES}" \
    -out "${PART}" "$@"
  append
}

if wanted_block A; then
  for nodes in ${NODES}; do
    for catalog in 6 200 792; do
      for placement in packed random; do
        run "${nodes}" "${catalog}" "${placement}" 80 0 lru 1 volume
        run "${nodes}" "${catalog}" "${placement}" 80 0 lru 1 volume -no-cluster-cache
      done
    done
  done
fi

if wanted_block B && [[ " ${NODES} " != *" 10 "* ]]; then
  run 10 792 packed 80 0 lru 1 volume
fi

if wanted_block B; then
  peak="$("${PYTHON}" - "${REPLAY_FILE}" <<'EOF'
import sys, pandas as pd
df = pd.read_csv(sys.argv[1])
row = df[
    (df.working_sets == "n792") & (df.nodes == 10) & (df.placement == "packed") &
    (df.policy == "lru") & (df.capacity_bytes == 0) & (df.cluster_cache == "yes")
]
if row.empty:
    sys.exit("no unbounded n792/nodes=10/packed/lru baseline row found for the capacity grid")
print(int(row.iloc[0].peak_stored_bytes))
EOF
)"
  caps="$("${PYTHON}" -c "print(','.join(str(int(${peak}*f)) for f in (0.10,0.25,0.50,0.75,1.00)))")"
  for nodes in 10 1000; do
    run "${nodes}" 792 packed 80 "${caps}" "${POLICIES}" 1 volume
  done
fi

if wanted_block C; then
  for nodes in 3 1000; do
    for placement in packed random; do
      run "${nodes}" 792 "${placement}" 0 0 lru 1 volume
      run "${nodes}" 792 "${placement}" 0 0 lru 1 volume -no-cluster-cache
    done
  done
fi

if wanted_block D; then
  run 100 200 packed 80 0 lru 1,2,3 hash
  run 100 200 packed 80 0 lru 1,2,3 hash -no-cluster-cache
fi

echo "replay written to ${REPLAY_FILE} ($(( $(wc -l < "${REPLAY_FILE}") - 1 )) rows)"
