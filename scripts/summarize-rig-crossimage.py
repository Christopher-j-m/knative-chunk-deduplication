#!/usr/bin/env python3
"""Turns the rig's cross-image reuse cells into a latency for the second image's first start.

Each repetition seeds the cache with image A on one node and then starts image
B for the first time on another node, so every chunk B finds in the cache came
from A. Per pair this reports the seed and cross start means with intervals,
the cache hits and misses of the cross start, and, where the distance sweep
holds the same image at the same round trip, the cross start against B's seed
on an empty cache (configuration 4) and against its seed under stock nydusd
(configuration 2s).

Usage: summarize-rig-crossimage.py [--rtt 80] [--sweep-prefix distance-clean]
       [--output analysis/results/summaries/rig-crossimage-summary.csv]
"""

import argparse
import csv
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BENCHMARKS = ROOT / "analysis" / "results" / "benchmarks"
SUMMARIES = ROOT / "analysis" / "results" / "summaries"

sys.path.insert(0, str(Path(__file__).resolve().parent))
from cell_bootstrap import mean_interval, pooled_mean, ratio_interval  # noqa: E402

RATE = "600mbit"
CROSS_NAME = re.compile(rf"^rig-crossimage-(?P<a>fn-[a-z-]+?)-(?P<b>fn-[a-z-]+)-rtt(?P<rtt>\d+)ms-rate{RATE}-\d{{8}}T\d{{6}}\.csv$")
SWEEP_NAME = re.compile(r"-\d{8}T\d{6}\.csv$")


def read_rows(path):
    with path.open() as handle:
        return list(csv.DictReader(handle))


def sweep_seeds(prefix, config, image, rtt):
    """Seed ready times of one distance-sweep cell, one value per repetition."""
    matches = sorted(path for path in BENCHMARKS.glob(f"{prefix}-config{config}-{image}-rtt{rtt}ms-rate{RATE}-*.csv")
                     if SWEEP_NAME.search(path.name))
    if not matches:
        return None
    return [float(row["ready_seconds"]) for row in read_rows(matches[-1]) if row["phase"] == "seed"]


def interval_columns(name, values, row):
    low, high = mean_interval(values)
    row[f"{name}_mean_s"] = round(pooled_mean(values), 3)
    row[f"{name}_ci_low_s"] = round(low, 3)
    row[f"{name}_ci_high_s"] = round(high, 3)
    row[f"{name}_min_s"] = round(min(values), 3)
    row[f"{name}_max_s"] = round(max(values), 3)


def summarize(path, match, sweep_prefix):
    rows = read_rows(path)
    seed = [row for row in rows if row["phase"] == "seed"]
    cross = [row for row in rows if row["phase"] == "cross"]
    rtt = int(match["rtt"])
    summary = {"service_a": match["a"], "service_b": match["b"], "rtt_ms": rtt, "file": path.name,
               "reps": len(cross)}

    interval_columns("seed_a", [float(row["ready_seconds"]) for row in seed], summary)
    cross_times = [float(row["ready_seconds"]) for row in cross]
    interval_columns("cross_b", cross_times, summary)
    summary["cross_hits_mean"] = round(sum(int(row["cache_hits_delta"]) for row in cross) / len(cross), 1)
    summary["cross_misses_mean"] = round(sum(int(row["cache_misses_delta"]) for row in cross) / len(cross), 1)
    summary["cross_hit_share"] = round(summary["cross_hits_mean"]
                                       / (summary["cross_hits_mean"] + summary["cross_misses_mean"]), 4)
    summary["cross_landed_off_seed_node"] = sum(
        1 for s, c in zip(seed, cross) if s["node"] != c["node"]) == len(cross)

    # Image B's own first start, from the sweep: on an empty cache, and under stock nydusd
    for label, config in (("empty_cache_seed_b", "4"), ("stock_seed_b", "2s")):
        reference = sweep_seeds(sweep_prefix, config, match["b"], rtt)
        if not reference:
            continue
        interval_columns(label, reference, summary)
        ratio, low, high = ratio_interval(cross_times, reference)
        summary[f"cross_over_{label}"] = round(ratio, 3)
        summary[f"cross_over_{label}_ci_low"] = round(low, 3)
        summary[f"cross_over_{label}_ci_high"] = round(high, 3)
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--sweep-prefix", default="distance-clean")
    parser.add_argument("--output", default=str(SUMMARIES / "rig-crossimage-summary.csv"))
    args = parser.parse_args()

    summaries = []
    for path in sorted(BENCHMARKS.glob("rig-crossimage-*.csv")):
        match = CROSS_NAME.match(path.name)
        if match:
            summaries.append(summarize(path, match, args.sweep_prefix))
    if not summaries:
        sys.exit("no rig-crossimage cells found")

    fields = []
    for summary in summaries:
        fields += [key for key in summary if key not in fields]
    with open(args.output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=fields)
        writer.writeheader()
        writer.writerows(summaries)
    for summary in summaries:
        print(summary)


if __name__ == "__main__":
    main()
