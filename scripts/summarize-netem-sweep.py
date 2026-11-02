#!/usr/bin/env python3
"""Summarize round-trip-time sweep benchmark CSVs into one table.

Reads every cell CSV of one campaign prefix from a benchmark directory and
writes per-cell cold, warm, and seed statistics next to the other summaries.
The six-image ladder shares this file naming, so --prefix picks which campaign
to read. Run with:
uv run --with pandas scripts/summarize-netem-sweep.py [benchmark-dir] [--prefix ladder]
"""

import argparse
import glob
import os
import re

import pandas as pd

from cell_bootstrap import mean_interval, repetition_groups

BENCH = os.path.join(os.path.dirname(__file__), "../analysis/results/benchmarks")
SUMMARIES = os.path.join(os.path.dirname(__file__), "../analysis/results/summaries")


def cell_pattern(prefix):
    return re.compile(
        rf"{re.escape(prefix)}-config(?P<config>\d+s?)-(?P<image>fn-[a-z-]+)"
        r"-rtt(?P<rtt>\d+)ms-rate(?P<rate>[a-z0-9]+)-\d{8}T\d{6}\.csv$"
    )


def cell_stats(frame):
    cold = frame[(frame["phase"] == "scaleout") & (frame["warm"] == "no")]["ready_seconds"]
    warm = frame[(frame["phase"] == "scaleout") & (frame["warm"] == "yes")]["ready_seconds"]
    seed = frame[frame["phase"] == "seed"]["ready_seconds"]
    cold_frame = frame[(frame["phase"] == "scaleout") & (frame["warm"] == "no")]
    cold_low, cold_high = mean_interval(repetition_groups(cold_frame))
    seed_low, seed_high = mean_interval(seed)
    return {
        "cold_median_s": round(cold.median(), 3),
        # the published headline table reports means, so both live here and a
        # cell whose mean and median disagree is worth a second look
        "cold_mean_s": round(cold.mean(), 3),
        "cold_min_s": round(cold.min(), 3),
        "cold_max_s": round(cold.max(), 3),
        "cold_n": len(cold),
        # 95 percent bootstrap interval of the cold mean over the cell's repetitions
        "cold_ci_low_s": round(cold_low, 3),
        "cold_ci_high_s": round(cold_high, 3),
        "warm_median_s": round(warm.median(), 3),
        "warm_mean_s": round(warm.mean(), 3),
        "warm_n": len(warm),
        "seed_median_s": round(seed.median(), 3),
        "seed_mean_s": round(seed.mean(), 3),
        "seed_ci_low_s": round(seed_low, 3),
        "seed_ci_high_s": round(seed_high, 3),
        "above_floor_mean_s": round(cold.mean() - warm.mean(), 3),
        **dropped_packets(frame),
    }


# Packets the emulated link dropped, summed over the repetitions of a cell.
# Every scale-out row of a repetition carries that phase's count, so it is
# taken once per repetition. Cells recorded before the counter existed have
# no such column and report nothing
def dropped_packets(frame):
    if "netem_dropped" not in frame.columns:
        return {}
    seed = frame[frame["phase"] == "seed"]["netem_dropped"]
    scaleout = frame[frame["phase"] == "scaleout"].drop_duplicates("rep")["netem_dropped"]
    return {
        "netem_dropped_seed": int(seed.sum()),
        "netem_dropped_scaleout": int(scaleout.sum()),
        "netem_dropped_scaleout_max": int(scaleout.max()),
    }


# Reads the cell CSVs of one benchmark directory, defaulting to the campaign
# results. Naming an archived directory summarizes that campaign instead.
def parse_arguments():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("benchmark_dir", nargs="?", default=BENCH)
    parser.add_argument("--prefix", default="netem-sweep", help="campaign file prefix to read")
    return parser.parse_args()


def main():
    arguments = parse_arguments()
    benchmark_dir = arguments.benchmark_dir
    pattern = cell_pattern(arguments.prefix)
    out = os.path.join(SUMMARIES, f"{arguments.prefix}-summary.csv")

    rows = []
    for path in sorted(glob.glob(os.path.join(benchmark_dir, f"{arguments.prefix}-config*.csv"))):
        match = pattern.search(os.path.basename(path))
        if not match:
            continue
        rows.append({
            "config": match["config"],
            "image": match["image"],
            "rtt_ms": int(match["rtt"]),
            "rate": match["rate"],
            **cell_stats(pd.read_csv(path)),
        })

    table = pd.DataFrame(rows).sort_values(["image", "config", "rtt_ms", "rate"])

    # Only the campaign directory owns the summary file; summarizing an
    # archived run is a read-only look that must not overwrite it
    if os.path.realpath(benchmark_dir) == os.path.realpath(BENCH):
        table.to_csv(out, index=False)
    else:
        print(f"# {benchmark_dir} is not the campaign directory, {out} left alone")

    print(table.to_string(index=False))


if __name__ == "__main__":
    main()
