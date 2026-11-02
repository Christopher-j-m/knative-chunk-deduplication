#!/usr/bin/env bash
# Replays the full production week across cluster sizes, catalogs and
# placements, then sweeps capacity and policy at two cluster sizes. Three
# CSVs come out: one row per run, one row per run and frequency class, and
# one row per run of response-time quantiles. Runs without the cluster tier
# are charged stock nydusd's per-chunk cost by default; BASELINE=ablation
# charges them the proxy's own miss cost instead, which is the proxy with its
# cache switched off
set -euo pipefail
cd "$(dirname "$0")/.."

PYTHON="${PYTHON:-${HOME}/projects/thesis/.venv-trace/bin/python}"
OUT_DIR="${OUT_DIR:-analysis/results/simulation}"
TRACE="${TRACE:-analysis/traces/ibm-7d-all.csv}"
TOUCHED="analysis/results/chunks-live-touched.json"
COSTS="analysis/results/simulation/chunk-costs.csv"
NODES="${NODES:-3 10 100 1000 10000}"
POLICIES="lru,lru2,size,refcount,gdsf"
# which blocks of the campaign to run, so a long block can be resumed or
# repeated on its own without redoing the ones that already landed
BLOCKS="${BLOCKS:-A B C D}"
BASELINE="${BASELINE:-stock}"
case "${BASELINE}" in
  stock|ablation) ;;
  *) echo "BASELINE must be stock or ablation, not ${BASELINE}" >&2; exit 1 ;;
esac

# the trace loader alone peaks near 8 GB on the full week at this GC target,
# so only one campaign process runs at a time
export GOGC=50

PART="$(mktemp)"; PART_CLASSES="$(mktemp)"; PART_LATENCY="$(mktemp)"
# each campaign builds its own binary, so two campaigns with different
# baselines can run side by side without overwriting each other's build
BUILD_DIR="$(mktemp -d)"
SIM="${BUILD_DIR}/cache-sim"
trap 'rm -rf "${PART}" "${PART_CLASSES}" "${PART_LATENCY}" "${BUILD_DIR}"' EXIT

go build -o "${SIM}" ./cmd/cache-sim
mkdir -p "${OUT_DIR}"

wanted_block() {
  # a block runs only when its letter was asked for
  [[ " ${BLOCKS} " == *" $1 "* ]]
}

catalog_path() {
  case "$1" in
    6) echo "${TOUCHED}" ;;
    *) echo "analysis/traces/catalogs/n$1/working-sets.json" ;;
  esac
}

# per-chunk costs at one round trip, read from the fitted table. A cached
# run pays the proxy's miss cost for every chunk the cache lacks, while a run
# without the cluster tier pays what the chosen baseline pays per chunk
cost_args() {
  "${PYTHON}" - "$1" "$2" "${COSTS}" <<'EOF'
import sys, pandas as pd
rtt, side, path = int(sys.argv[1]), sys.argv[2], sys.argv[3]
matches = pd.read_csv(path).query("image == 'mean' and rtt_ms == @rtt")
if matches.empty:
    sys.exit(f"no mean cost row for rtt_ms={rtt} in {path}")
row = matches.iloc[0]
registry = row.stock_chunk_ms if side == "stock" else row.registry_chunk_ms
print(f"-warm-floor-ms {int(row.warm_floor_ms)} -registry-chunk-ms {registry} -cache-chunk-ms {row.cache_chunk_ms}")
EOF
}

# block A is what writes the headers, so every other selection has to add to
# results that are already on disk
first=1
if ! wanted_block A; then
  first=0
  for name in scale-campaign.csv scale-classes.csv scale-latency.csv; do
    if [[ ! -f "${OUT_DIR}/${name}" ]]; then
      echo "${OUT_DIR}/${name} is missing; run block A before the later blocks" >&2
      exit 1
    fi
  done
fi

append() {
  # folds one run's partial CSVs into the campaign totals, keeping a single
  # header at the top of each output file
  if [[ "${first}" = 1 ]]; then
    cat "${PART}" >"${OUT_DIR}/scale-campaign.csv"
    cat "${PART_CLASSES}" >"${OUT_DIR}/scale-classes.csv"
    cat "${PART_LATENCY}" >"${OUT_DIR}/scale-latency.csv"
    first=0
  else
    tail -n +2 "${PART}" >>"${OUT_DIR}/scale-campaign.csv"
    tail -n +2 "${PART_CLASSES}" >>"${OUT_DIR}/scale-classes.csv"
    tail -n +2 "${PART_LATENCY}" >>"${OUT_DIR}/scale-latency.csv"
  fi
}

run() {
  # run <nodes> <catalog> <placement> <rtt> <capacities> <policies> <seeds> <map> [extra flags]
  local nodes="$1" catalog="$2" placement="$3" rtt="$4" caps="$5" policies="$6" seeds="$7" map="$8"; shift 8
  echo "nodes=${nodes} catalog=${catalog} placement=${placement} rtt=${rtt} caps=${caps} policies=${policies} $*"

  # captured as its own statement so a failed lookup (bad rtt, missing row)
  # aborts under set -e instead of silently expanding to nothing and letting
  # cache-sim fall back to its built-in default coefficients
  # only a run without the cluster tier stands for the baseline
  local side="cached"
  if [[ " $* " == *" -no-cluster-cache "* && "${BASELINE}" == "stock" ]]; then
    side="stock"
  fi

  local costs
  costs="$(cost_args "${rtt}" "${side}")" || { echo "cost lookup failed for rtt=${rtt}" >&2; exit 1; }

  "${SIM}" -trace "${TRACE}" -working-sets "$(catalog_path "${catalog}")" \
    -nodes "${nodes}" -placement "${placement}" -capacities "${caps}" \
    -policies "${policies}" -seeds "${seeds}" -map "${map}" \
    ${costs} -latency-out "${PART_LATENCY}" -classes-out "${PART_CLASSES}" \
    -out "${PART}" "$@"
  append
}

# Block A: scale x catalog x placement, unbounded LRU, with and without the
# cluster tier, at the 80 ms coefficients
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

# block B sizes its capacity grid off the unbounded 10-node baseline on the
# 792 catalog; a shrunk NODES override (a smoke run) may have skipped it, so
# it is filled in here on demand instead of assumed present
if wanted_block B && [[ " ${NODES} " != *" 10 "* ]]; then
  run 10 792 packed 80 0 lru 1 volume
fi

# Block B: capacity x policy at 10 and 1000 nodes on the 792-image catalog;
# the grid is relative to that catalog's own unbounded peak
if wanted_block B; then
  peak="$("${PYTHON}" - "${OUT_DIR}/scale-campaign.csv" <<'EOF'
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

# Block C: the 0 ms coefficients at two cluster sizes
if wanted_block C; then
  for nodes in 3 1000; do
    for placement in packed random; do
      run "${nodes}" 792 "${placement}" 0 0 lru 1 volume
      run "${nodes}" 792 "${placement}" 0 0 lru 1 volume -no-cluster-cache
    done
  done
fi

# Block D: mapping sensitivity
if wanted_block D; then
  run 100 200 packed 80 0 lru 1,2,3 hash
  run 100 200 packed 80 0 lru 1,2,3 hash -no-cluster-cache
fi

echo "campaign written to ${OUT_DIR}/scale-campaign.csv ($(( $(wc -l < "${OUT_DIR}/scale-campaign.csv") - 1 )) rows)"
