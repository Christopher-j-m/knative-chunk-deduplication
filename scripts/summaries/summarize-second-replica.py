#!/usr/bin/env python3
"""Measures what a second simultaneous replica costs each configuration"""
import argparse
import csv
import re
import statistics
import sys
from pathlib import Path
from result_paths import summary_path, find_benchmarks

ROOT = Path(__file__).resolve().parent.parent.parent

sys.path.insert(0, str(Path(__file__).resolve().parent))
from cell_bootstrap import difference_interval  # noqa: E402

CELL_PATTERN = re.compile(
    r"second-replica-(?P<protocol>one-pod|two-pods)-config(?P<config>\d+)-(?P<image>.+?)-"
    r"rate(?P<rate>[^-]+)-\d{8}T\d{6}\.csv$"
)


COLD_PODS_PER_REPETITION = {"one-pod": 1, "two-pods": 2}


def cold_ready_times(path, protocol):
    with path.open() as handle:
        rows = list(csv.DictReader(handle))

    by_repetition = {}
    for row in rows:
        if row["phase"] == "scaleout" and row["warm"] == "no":
            by_repetition.setdefault(row["rep"], []).append(float(row["ready_seconds"]))

    wanted = COLD_PODS_PER_REPETITION[protocol]
    kept = [times for times in by_repetition.values() if len(times) == wanted]
    return kept, len(kept)


def collect_cells():
    cells = {}
    for path in find_benchmarks("second-replica-*.csv"):
        match = CELL_PATTERN.match(path.name)
        if not match:
            continue
        key = (match["config"], match["image"], match["rate"])
        cells.setdefault(key, {})[match["protocol"]] = cold_ready_times(path, match["protocol"])
    return cells


def summarize(key, protocols):
    config, image, rate = key
    one_pod, one_pod_repetitions = protocols["one-pod"]
    two_pods, two_pods_repetitions = protocols["two-pods"]
    difference, low, high = difference_interval(two_pods, one_pod)

    one_pod = [time for times in one_pod for time in times]
    two_pods = [time for times in two_pods for time in times]

    return {
        "config": config,
        "image": image,
        "rate": rate,
        "one_pod_repetitions": one_pod_repetitions,
        "two_pods_repetitions": two_pods_repetitions,
        "one_pod_cold_pods": len(one_pod),
        "two_pods_cold_pods": len(two_pods),
        "one_pod_mean_s": round(statistics.mean(one_pod), 3),
        "two_pods_mean_s": round(statistics.mean(two_pods), 3),
        "second_replica_cost_s": round(difference, 3),
        "interval_low_s": round(low, 3),
        "interval_high_s": round(high, 3),
        "one_pod_median_s": round(statistics.median(one_pod), 3),
        "two_pods_median_s": round(statistics.median(two_pods), 3),
        "median_difference_s": round(statistics.median(two_pods) - statistics.median(one_pod), 3),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--output", default=str(summary_path("second-replica-summary.csv")))
    args = parser.parse_args()

    summary = []
    for key, protocols in sorted(collect_cells().items()):
        if "one-pod" in protocols and "two-pods" in protocols:
            summary.append(summarize(key, protocols))

    with open(args.output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(summary[0].keys()))
        writer.writeheader()
        writer.writerows(summary)

    for row in summary:
        print(row)


if __name__ == "__main__":
    main()
