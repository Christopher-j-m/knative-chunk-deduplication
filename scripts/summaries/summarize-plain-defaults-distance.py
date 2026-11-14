#!/usr/bin/env python3
"""Compares plain nydusd at its defaults with the cluster cache over registry distance"""
import argparse
import csv
import re
import sys
from pathlib import Path
from result_paths import summary_path, find_benchmarks

ROOT = Path(__file__).resolve().parent.parent.parent

sys.path.insert(0, str(Path(__file__).resolve().parent))
from cell_bootstrap import mean_interval, pooled_mean, ratio_interval  # noqa: E402

RATE = "600mbit"
IMAGES = ["fn-python-hello", "fn-python-ml"]
RTTS = [0, 10, 25, 50, 80, 100]
MEASUREMENT_NAME = re.compile(r"-\d{8}T\d{6}\.csv$")


def newest(pattern):
    matches = sorted(path for path in find_benchmarks(pattern) if MEASUREMENT_NAME.search(path.name))
    return matches[-1] if matches else None


def cold_groups(path, phase="scaleout"):
    with path.open() as handle:
        rows = [row for row in csv.DictReader(handle) if row["phase"] == phase and row["warm"] == "no"]
    groups = {}
    for row in rows:
        groups.setdefault(row["rep"], []).append(float(row["ready_seconds"]))
    dropped = max(int(row["netem_dropped"] or 0) for row in rows)
    return list(groups.values()), dropped


def verdicts(path):
    verdict_path = path.with_name(path.name[: -len(".csv")] + ".prefetch-check.csv")
    if not verdict_path.exists():
        return "missing"
    with verdict_path.open() as handle:
        observed = [row["observed"] for row in csv.DictReader(handle)]
    return f"{observed.count('yes')}/{len(observed)}"


def describe(label, path, row):
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
        "plain_off": newest(f"{prefix}-config2s-{image}-nydus-zstd-prefetchoff-{stem}*.csv"),
        "plain_all": newest(f"{prefix}-config2s-{image}-nydus-zstd-prefetchall-{stem}*.csv"),
        "cache": newest(f"{cache_prefix}-config4-{image}-{stem}*.csv"),
    }
    if None in paths.values():
        return None

    row = {"image": image, "rtt_ms": rtt}
    cold = {label: describe(label, path, row) for label, path in paths.items()}
    row["plain_off_prefetch_observed"] = verdicts(paths["plain_off"])
    row["plain_all_prefetch_observed"] = verdicts(paths["plain_all"])

    ratio, low, high = ratio_interval(cold["plain_all"], cold["plain_off"])
    row.update({"all_over_off": round(ratio, 3), "all_over_off_ci_low": round(low, 3),
                "all_over_off_ci_high": round(high, 3)})

    faster = "plain_all" if pooled_mean(cold["plain_all"]) < pooled_mean(cold["plain_off"]) else "plain_off"
    lead, lead_low, lead_high = ratio_interval(cold[faster], cold["cache"])
    row.update({"faster_plain_mode": faster, "faster_plain_over_cache": round(lead, 3),
                "faster_plain_over_cache_ci_low": round(lead_low, 3),
                "faster_plain_over_cache_ci_high": round(lead_high, 3)})
    return row


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", default="plain-defaults-distance100k")
    parser.add_argument("--cache-prefix", default="distance100k")
    parser.add_argument("--output")
    args = parser.parse_args()
    output = args.output or str(summary_path(f"{args.prefix}-summary.csv"))

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
        print(f"{row['image']:16} {row['rtt_ms']:>3} ms  off {row['plain_off_cold_mean_s']:6.2f}  "
              f"all {row['plain_all_cold_mean_s']:6.2f}  cache {row['cache_cold_mean_s']:6.2f}  "
              f"all/off {row['all_over_off']:.2f}  lead {row['faster_plain_over_cache']:.2f} "
              f"({row['faster_plain_over_cache_ci_low']:.2f}-{row['faster_plain_over_cache_ci_high']:.2f}) "
              f"observed {row['plain_all_prefetch_observed']}/{row['plain_off_prefetch_observed']}")


if __name__ == "__main__":
    main()
