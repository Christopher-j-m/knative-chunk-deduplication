#!/usr/bin/env python3
"""Summarizes the registry bandwidth cells with two cold pods into one table"""
import glob
import os
import re

import pandas as pd
from result_paths import summary_path, find_benchmarks, BENCHMARKS

OUT = summary_path("bandwidth-two-pods-summary.csv")

CELL_PATTERN = re.compile(
    r"bandwidth-two-pods-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
    r"-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"
)


def cell_stats(frame):
    scaleout = frame[frame["phase"] == "scaleout"]
    cold = scaleout[scaleout["warm"] == "no"]["ready_seconds"]
    warm = scaleout[scaleout["warm"] == "yes"]["ready_seconds"]
    seed = frame[frame["phase"] == "seed"]["ready_seconds"]

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
    for path in [str(path) for path in find_benchmarks("bandwidth-two-pods-config*.csv")]:
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
        raise SystemExit(f"no bandwidth-two-pods cell CSVs found in {BENCHMARKS}")

    summary = pd.DataFrame(rows).sort_values(["config", "image", "rate_mbit"])
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    summary.to_csv(OUT, index=False)
    print(summary.to_string(index=False))
    print(f"\nWrote {OUT}")


if __name__ == "__main__":
    main()
