#!/usr/bin/env python3
"""Holds stock Nydus at its defaults against the cluster cache over registry distance.

Stock nydusd runs on the zstd conversion with whole-image prefetch off and on
(prefix distance-defaults100k), and the cache cell at the same image and round
trip comes from the distance sweep (prefix distance-clean, configuration 4).
Per image and round trip this reports both stock modes, the ratio between
them, the faster of the two, and the cache's lead over it with bootstrap
intervals. A ratio of prefetch on over off below one means prefetch paid.

Usage: summarize-distance-defaults.py [--prefix distance-defaults100k] [--cache-prefix distance-clean]
       [--output analysis/results/summaries/distance-defaults100k-summary.csv]
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
IMAGES = ["fn-python-hello", "fn-python-ml"]
RTTS = [0, 10, 25, 50, 80, 100]
MEASUREMENT_NAME = re.compile(r"-\d{8}T\d{6}\.csv$")


def newest(pattern):
    """Returns the newest measurement file that matches a glob, or None."""
    matches = sorted(path for path in BENCHMARKS.glob(pattern) if MEASUREMENT_NAME.search(path.name))
    return matches[-1] if matches else None


def cold_groups(path, phase="scaleout"):
    """Ready times of the pods that started on a node without the image, grouped by repetition."""
    with path.open() as handle:
        rows = [row for row in csv.DictReader(handle) if row["phase"] == phase and row["warm"] == "no"]
    groups = {}
    for row in rows:
        groups.setdefault(row["rep"], []).append(float(row["ready_seconds"]))
    dropped = max(int(row["netem_dropped"] or 0) for row in rows)
    return list(groups.values()), dropped


def verdicts(path):
    """Counts the repetitions whose seed downloaded the whole image."""
    verdict_path = path.with_name(path.name[: -len(".csv")] + ".prefetch-check.csv")
    if not verdict_path.exists():
        return "missing"
    with verdict_path.open() as handle:
        observed = [row["observed"] for row in csv.DictReader(handle)]
    return f"{observed.count('yes')}/{len(observed)}"


def describe(label, path, row):
    """Adds one cell's cold mean, interval and seed mean to a summary row."""
    cold, dropped = cold_groups(path)
    seed, _ = cold_groups(path, "seed")
    low, high = mean_interval(cold)
    row[f"{label}_cold_mean_s"] = round(pooled_mean(cold), 3)
    row[f"{label}_cold_ci_low_s"] = round(low, 3)
    row[f"{label}_cold_ci_high_s"] = round(high, 3)
    row[f"{label}_seed_mean_s"] = round(pooled_mean(seed), 3)
    row[f"{label}_netem_dropped_max"] = dropped
    return cold


def summarize(prefix, cache_prefix, image, rtt):
    stem = f"rtt{rtt}ms-rate{RATE}-"
    paths = {
        "stock_off": newest(f"{prefix}-config2s-{image}-nydus-zstd-prefetchoff-{stem}*.csv"),
        "stock_all": newest(f"{prefix}-config2s-{image}-nydus-zstd-prefetchall-{stem}*.csv"),
        "cache": newest(f"{cache_prefix}-config4-{image}-{stem}*.csv"),
    }
    if None in paths.values():
        return None

    row = {"image": image, "rtt_ms": rtt}
    cold = {label: describe(label, path, row) for label, path in paths.items()}
    row["stock_off_prefetch_observed"] = verdicts(paths["stock_off"])
    row["stock_all_prefetch_observed"] = verdicts(paths["stock_all"])

    ratio, low, high = ratio_interval(cold["stock_all"], cold["stock_off"])
    row.update({"all_over_off": round(ratio, 3), "all_over_off_ci_low": round(low, 3),
                "all_over_off_ci_high": round(high, 3)})

    # The cache is held against whichever stock mode started faster here
    faster = "stock_all" if pooled_mean(cold["stock_all"]) < pooled_mean(cold["stock_off"]) else "stock_off"
    lead, lead_low, lead_high = ratio_interval(cold[faster], cold["cache"])
    row.update({"faster_stock_mode": faster, "faster_stock_over_cache": round(lead, 3),
                "faster_stock_over_cache_ci_low": round(lead_low, 3),
                "faster_stock_over_cache_ci_high": round(lead_high, 3)})
    return row


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", default="distance-defaults100k")
    parser.add_argument("--cache-prefix", default="distance-clean")
    parser.add_argument("--output")
    args = parser.parse_args()
    output = args.output or str(SUMMARIES / f"{args.prefix}-summary.csv")

    rows = []
    for image in IMAGES:
        for rtt in RTTS:
            row = summarize(args.prefix, args.cache_prefix, image, rtt)
            if row is None:
                print(f"skipping {image} at {rtt} ms: a cell is missing", file=sys.stderr)
                continue
            rows.append(row)
    if not rows:
        sys.exit("no complete image and round trip to summarize")

    with open(output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    for row in rows:
        print(f"{row['image']:16} {row['rtt_ms']:>3} ms  off {row['stock_off_cold_mean_s']:6.2f}  "
              f"all {row['stock_all_cold_mean_s']:6.2f}  cache {row['cache_cold_mean_s']:6.2f}  "
              f"all/off {row['all_over_off']:.2f}  lead {row['faster_stock_over_cache']:.2f} "
              f"({row['faster_stock_over_cache_ci_low']:.2f}-{row['faster_stock_over_cache_ci_high']:.2f}) "
              f"observed {row['stock_all_prefetch_observed']}/{row['stock_off_prefetch_observed']}")


if __name__ == "__main__":
    main()
