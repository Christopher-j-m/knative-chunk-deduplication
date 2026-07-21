#!/usr/bin/env python3
"""Summarize the netem RTT sweep benchmark CSVs into one table.

Reads every netem-sweep cell CSV from analysis/results/benchmarks and writes
per-cell cold, warm, and seed statistics next to the other summaries. Run with:
uv run --with pandas scripts/summarize-netem-sweep.py
"""

import glob
import os
import re

import pandas as pd

BENCH = os.path.join(os.path.dirname(__file__), "../analysis/results/benchmarks")
OUT = os.path.join(
    os.path.dirname(__file__), "../analysis/results/summaries/netem-sweep-summary.csv"
)

CELL_PATTERN = re.compile(
    r"netem-sweep-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
    r"-rtt(?P<rtt>\d+)ms-rate(?P<rate>[a-z0-9]+)-\d{8}T\d{6}\.csv$"
)


def cell_stats(frame):
    cold = frame[(frame["phase"] == "scaleout") & (frame["warm"] == "no")]["ready_seconds"]
    warm = frame[(frame["phase"] == "scaleout") & (frame["warm"] == "yes")]["ready_seconds"]
    seed = frame[frame["phase"] == "seed"]["ready_seconds"]
    return {
        "cold_median_s": round(cold.median(), 3),
        "cold_min_s": round(cold.min(), 3),
        "cold_max_s": round(cold.max(), 3),
        "cold_n": len(cold),
        "warm_median_s": round(warm.median(), 3),
        "warm_n": len(warm),
        "seed_median_s": round(seed.median(), 3),
    }


def main():
    rows = []
    for path in sorted(glob.glob(os.path.join(BENCH, "netem-sweep-config*.csv"))):
        match = CELL_PATTERN.search(os.path.basename(path))
        if not match:
            continue
        rows.append({
            "config": int(match["config"]),
            "image": match["image"],
            "rtt_ms": int(match["rtt"]),
            "rate": match["rate"],
            **cell_stats(pd.read_csv(path)),
        })

    table = pd.DataFrame(rows).sort_values(["image", "config", "rtt_ms", "rate"])
    table.to_csv(OUT, index=False)
    print(table.to_string(index=False))


if __name__ == "__main__":
    main()
