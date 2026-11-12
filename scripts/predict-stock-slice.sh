#!/usr/bin/env bash
# Predicts what the one-hour trace slice does on the seven-node rig under
# stock nydusd, before the hour is replayed live. The simulator replays the
# slice once as a whole and once per application, without the cluster tier and
# at stock nydusd's fitted cost per chunk, and the per-image cold-start times
# follow from the same coefficients. The output is a markdown section
#
# Usage: [RTT=80] [NODES=7] [PYTHON=..] predict-stock-slice.sh > section.md
set -euo pipefail
cd "$(dirname "$0")/.."

PYTHON="${PYTHON:-${PWD}/.venv-analysis/bin/python}"
RTT="${RTT:-80}"
NODES="${NODES:-7}"
TRACE="analysis/traces/slice-1h.csv"
APPS="analysis/traces/slice-1h-apps.json"
TOUCHED="analysis/results/chunks-live-touched.json"
COSTS="analysis/results/simulation/chunk-costs.csv"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

go build -o "${WORK}/cache-sim" ./cmd/cache-sim

# The simulator takes the mapping of applications to images as a flat object
"${PYTHON}" -c "import json, sys; apps = json.load(open(sys.argv[1]))['apps']; json.dump({name: app['image'] for name, app in apps.items()}, open(sys.argv[2], 'w'))" \
  "${APPS}" "${WORK}/assign.json"

# Stock nydusd's fitted floor and cost per chunk at this round trip
read -r floor_ms stock_chunk_ms < <("${PYTHON}" - "${RTT}" "${COSTS}" <<'EOF'
import sys
import pandas as pd
rtt, path = int(sys.argv[1]), sys.argv[2]
row = pd.read_csv(path).query("image == 'mean' and rtt_ms == @rtt").iloc[0]
print(int(row.warm_floor_ms), row.stock_chunk_ms)
EOF
)

simulate() {
  local trace="$1" out="$2"
  "${WORK}/cache-sim" -trace "${trace}" -working-sets "${TOUCHED}" -assign "${WORK}/assign.json" \
    -nodes "${NODES}" -placement packed -no-cluster-cache \
    -warm-floor-ms "${floor_ms}" -registry-chunk-ms "${stock_chunk_ms}" \
    -out "${out}" -latency-out "${out%.csv}.latency.csv" >/dev/null
}

# The whole hour gives the totals, and each application replayed alone gives
# its own cold-start count, as the earlier predictions did
simulate "${TRACE}" "${WORK}/whole.csv"
mkdir -p "${WORK}/apps"
"${PYTHON}" - "${TRACE}" "${WORK}/apps" <<'EOF'
import sys
import pandas as pd
trace = pd.read_csv(sys.argv[1])
for app, rows in trace.groupby("app"):
    rows.to_csv(f"{sys.argv[2]}/{app}.csv", index=False)
EOF
for app_trace in "${WORK}"/apps/*.csv; do
  simulate "${app_trace}" "${WORK}/apps/$(basename "${app_trace}" .csv).result"
done

"${PYTHON}" - "${WORK}" "${APPS}" "${TOUCHED}" "${floor_ms}" "${stock_chunk_ms}" "${RTT}" "${NODES}" <<'EOF'
import json
import sys
from pathlib import Path

import pandas as pd

work, apps_path, touched_path = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
floor_ms, stock_ms, rtt, nodes = float(sys.argv[4]), float(sys.argv[5]), sys.argv[6], sys.argv[7]
apps = json.load(open(apps_path))["apps"]
chunks = {entry["image"]: len(entry["chunks"]) for entry in json.load(open(touched_path))}

whole = pd.read_csv(work / "whole.csv").iloc[0]
latency = pd.read_csv(work / "whole.latency.csv").iloc[0]

def megabytes(value):
    return f"{value / 1e6:,.1f} MB"

print(f"Replayed by the simulator at {nodes} nodes, `packed` placement, without the")
print(f"cluster tier, at the {rtt} ms coefficients of `chunk-costs.csv` for stock")
print(f"nydusd (floor {floor_ms:,.0f} ms, {stock_ms:.3f} ms per registry chunk).")
print()
print("| Quantity | Stock nydusd |")
print("|---|---|")
print(f"| Cold starts | {int(whole.cold_starts)} ({int(whole.seed_cold_starts)} of them image seeds) |")
print(f"| Warm starts | {int(whole.warm_starts)} |")
print(f"| Registry bytes | {megabytes(whole.registry_bytes)} |")
print(f"| Registry requests | {int(whole.registry_requests):,} |")
print(f"| Cold-start latency p50 / p95 | {latency.cold_p50_ms:,.0f} / {latency.cold_p95_ms:,.0f} ms |")
print(f"| Response time p50 / p99 / p99.9 | {latency.p50_ms:,.0f} / {latency.p99_ms:,.0f} / {latency.p999_ms:,.0f} ms |")
print()
print("| app | class (weekly) | image | calls in hour | predicted cold starts |")
print("|---|---|---|---|---|")
for name, app in apps.items():
    result = pd.read_csv(work / "apps" / f"{name}.result").iloc[0]
    print(f"| {name} | F{app['class'] + 1} | {app['image']} | {app['calls_in_hour']} | {int(result.cold_starts)} |")
print()
print("| Image | Touched chunks | First start, cached (ms) | First start, uncached (ms) | Repeat on the node (ms) |")
print("|---|---|---|---|---|")
for image in ["fn-go-hello", "fn-python-hello", "fn-java-springboot", "fn-node-api", "fn-python-ml", "fn-pytorch-cpu"]:
    first = floor_ms + chunks[image] * stock_ms
    print(f"| {image} | {chunks[image]} | --- | {first:,.0f} | {floor_ms:,.0f} |")
EOF
