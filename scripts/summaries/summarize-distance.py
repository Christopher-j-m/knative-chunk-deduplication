#!/usr/bin/env python3
"""Summarizes the registry distance cells of one run into one table"""
import argparse
import glob
import os
import re

import pandas as pd

from cell_bootstrap import mean_interval, repetition_groups
from result_paths import summary_path, find_benchmarks



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
        "cold_mean_s": round(cold.mean(), 3),
        "cold_min_s": round(cold.min(), 3),
        "cold_max_s": round(cold.max(), 3),
        "cold_n": len(cold),
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


def parse_arguments():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("benchmark_dir", nargs="?")
    parser.add_argument("--prefix", default="distance", help="file prefix of the run to read")
    return parser.parse_args()


def main():
    arguments = parse_arguments()
    benchmark_dir = arguments.benchmark_dir
    pattern = cell_pattern(arguments.prefix)
    out = summary_path(f"{arguments.prefix}-summary.csv")

    rows = []
    if benchmark_dir is None:
        paths = [str(path) for path in find_benchmarks(f"{arguments.prefix}-config*.csv")]
    else:
        paths = sorted(glob.glob(os.path.join(benchmark_dir, f"{arguments.prefix}-config*.csv")))

    for path in paths:
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

    if benchmark_dir is None:
        table.to_csv(out, index=False)
    else:
        print(f"# {benchmark_dir} is not the benchmark directory, {out} left alone")

    print(table.to_string(index=False))


if __name__ == "__main__":
    main()
