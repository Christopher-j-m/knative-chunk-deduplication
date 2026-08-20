#!/usr/bin/env python3
"""Summarize the registry bandwidth sweep benchmark CSVs into one table.

Reads every rate-sweep cell CSV from analysis/results/benchmarks and writes
per-cell cold, warm, seed and pull statistics next to the other summaries.
Run with: uv run --with pandas scripts/summarize-rate-sweep.py
"""

import glob
import os
import re

import pandas as pd

BENCH = os.path.join(os.path.dirname(__file__), "../analysis/results/benchmarks")
OUT = os.path.join(
    os.path.dirname(__file__), "../analysis/results/summaries/rate-sweep-summary.csv"
)

CELL_PATTERN = re.compile(
    r"rate-sweep-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
    r"-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"
)


def cell_stats(frame):
    scaleout = frame[frame["phase"] == "scaleout"]
    cold = scaleout[scaleout["warm"] == "no"]["ready_seconds"]
    warm = scaleout[scaleout["warm"] == "yes"]["ready_seconds"]
    seed = frame[frame["phase"] == "seed"]["ready_seconds"]

    # The pull time of the cross-node pods is what the bandwidth story rests on:
    # it is the part of a cold start that the registry rate directly governs
    pull = scaleout[scaleout["warm"] == "no"]["pull_seconds"]
    return {
        "cold_median_s": round(cold.median(), 3),
        "cold_min_s": round(cold.min(), 3),
        "cold_max_s": round(cold.max(), 3),
        "cold_n": len(cold),
        "warm_median_s": round(warm.median(), 3),
        "warm_n": len(warm),
        "seed_median_s": round(seed.median(), 3),
        "pull_median_s": round(pull.median(), 3),
    }


def main():
    rows = []
    for path in sorted(glob.glob(os.path.join(BENCH, "rate-sweep-config*.csv"))):
        match = CELL_PATTERN.search(os.path.basename(path))
        if not match:
            continue

        frame = pd.read_csv(path)
        row = {
            "config": int(match.group("config")),
            "image": match.group("image"),
            "rate_mbit": int(match.group("rate")),
        }
        row.update(cell_stats(frame))
        rows.append(row)

    if not rows:
        raise SystemExit(f"no rate-sweep cell CSVs found in {os.path.normpath(BENCH)}")

    summary = pd.DataFrame(rows).sort_values(["config", "image", "rate_mbit"])
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    summary.to_csv(OUT, index=False)
    print(summary.to_string(index=False))
    print(f"\nWrote {OUT}")


if __name__ == "__main__":
    main()
