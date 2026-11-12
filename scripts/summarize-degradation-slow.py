#!/usr/bin/env python3
"""Summarizes the slow-cache degradation cells next to a healthy cache and the uncached system.

Each mode delays every answer of the cache pod between the seed and the
scale-out. Per mode this reports how many scale-out pods became ready, the
cold mean with its interval and range, the cache hits and misses of the
scale-out, the registry fetch errors, and the delay the probe pod measured
before each repetition. The references are the deep-queue node sweep's cells
of the same image at three nodes: configuration 4 (healthy cache) and
configuration 2 (no cache), and the cold mean's ratio to each comes with an
interval.

Usage: summarize-degradation-slow.py [--prefix degradation-slow] [--reference-prefix node-sweep100k]
       [--output analysis/results/summaries/degradation-slow-summary.csv]
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

MEASUREMENT_NAME = re.compile(r"-\d{8}T\d{6}\.csv$")


def newest(pattern):
    matches = sorted(path for path in BENCHMARKS.glob(pattern) if MEASUREMENT_NAME.search(path.name))
    return matches[-1] if matches else None


def read_rows(path):
    with path.open() as handle:
        return list(csv.DictReader(handle))


def cold_groups(rows):
    groups = {}
    for row in rows:
        if row["phase"] == "scaleout" and row["warm"] == "no":
            groups.setdefault(row["rep"], []).append(float(row["ready_seconds"]))
    return [groups[rep] for rep in sorted(groups, key=int)]


def delay_record(path):
    """Reads the delay the probe pod measured before each repetition."""
    if not path.exists():
        return {}
    rows = read_rows(path)
    added = [float(row["added_ms"]) for row in rows]
    return {
        "delay_checks": len(rows),
        "delay_verified": sum(1 for row in rows if row["verified"] == "yes"),
        "measured_added_ms_min": min(added),
        "measured_added_ms_max": max(added),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", default="degradation-slow")
    parser.add_argument("--reference-prefix", default="node-sweep100k")
    parser.add_argument("--image", default="fn-python-hello")
    parser.add_argument("--rtt", type=int, default=80)
    parser.add_argument("--nodes", type=int, default=3)
    parser.add_argument("--output", default=None)
    args = parser.parse_args()
    output = args.output or str(SUMMARIES / f"{args.prefix}-summary.csv")
    stem = f"{args.image}-rtt{args.rtt}ms-rate600mbit"

    references = {}
    for label, config in (("healthy_cache", "4"), ("no_cache", "2")):
        path = newest(f"{args.reference_prefix}-config{config}-{args.image}-n{args.nodes}-rtt{args.rtt}ms-rate600mbit-*.csv")
        if path is not None:
            references[label] = (path.name, cold_groups(read_rows(path)))

    summary = []
    for path in sorted(BENCHMARKS.glob(f"{args.prefix}-slow*-{stem}-*.csv")):
        if not MEASUREMENT_NAME.search(path.name):
            continue
        mode = re.match(rf"{re.escape(args.prefix)}-(slow\d+)-", path.name).group(1)
        rows = read_rows(path)
        scaleout = [row for row in rows if row["phase"] == "scaleout"]
        cold = cold_groups(rows)
        flat = [value for group in cold for value in group]
        low, high = mean_interval(cold)
        per_phase = {row["rep"]: row for row in scaleout}.values()
        row = {
            "mode": mode, "file": path.name, "reps": len({r["rep"] for r in rows}),
            "scaleout_pods_ready": len(scaleout), "cold_pods": len(flat),
            "cold_mean_s": round(pooled_mean(cold), 3), "cold_ci_low_s": round(low, 3), "cold_ci_high_s": round(high, 3),
            "cold_min_s": round(min(flat), 3), "cold_max_s": round(max(flat), 3),
            "seed_mean_s": round(sum(float(r["ready_seconds"]) for r in rows if r["phase"] == "seed")
                                 / len({r["rep"] for r in rows}), 3),
            "scaleout_cache_hits": sum(int(r["cache_hits_delta"]) for r in per_phase),
            "scaleout_cache_misses": sum(int(r["cache_misses_delta"]) for r in per_phase),
            "registry_fetch_errors": sum(int(r["registry_fetch_errors"] or 0) for r in rows),
            "restarts": sum(int(r["restart_count"] or 0) for r in rows),
            "netem_dropped": sum(int(r["netem_dropped"] or 0) for r in rows if r["phase"] == "seed")
            + sum(int(r["netem_dropped"] or 0) for r in per_phase),
        }
        row.update(delay_record(path.with_name(f"{args.prefix}-{mode}-{stem}.delay.csv")))
        for label, (name, groups) in references.items():
            ratio, ratio_low, ratio_high = ratio_interval(cold, groups)
            row[f"{label}_file"] = name
            row[f"{label}_cold_mean_s"] = round(pooled_mean(groups), 3)
            row[f"over_{label}"] = round(ratio, 3)
            row[f"over_{label}_ci_low"] = round(ratio_low, 3)
            row[f"over_{label}_ci_high"] = round(ratio_high, 3)
        summary.append(row)

    if not summary:
        sys.exit(f"no {args.prefix} cells for {stem}")
    fields = []
    for row in summary:
        fields += [key for key in row if key not in fields]
    with open(output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=fields)
        writer.writeheader()
        writer.writerows(summary)
    for row in summary:
        print(row)


if __name__ == "__main__":
    main()
