#!/usr/bin/env python3
"""Measures what a second simultaneous replica costs each configuration.

The single-pod cells (prefix replica-solo) and the two-pod cells (prefix
replica-pair) ran back to back on one rig build, so their difference is the
cost of the second cold replica without a between-session offset. The
difference of the mean cold ready times gets a bootstrap interval over the
cold pods; the medians are printed next to it for comparison with earlier
campaigns, which reported medians.

Usage: summarize-replica.py [--output analysis/results/summaries/replica-summary.csv]
"""

import argparse
import csv
import re
import statistics
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BENCHMARKS = ROOT / "analysis" / "results" / "benchmarks"
SUMMARIES = ROOT / "analysis" / "results" / "summaries"

sys.path.insert(0, str(Path(__file__).resolve().parent))
from cell_bootstrap import difference_interval  # noqa: E402

CELL_PATTERN = re.compile(
    r"replica-(?P<protocol>solo|pair)-config(?P<config>\d+)-(?P<image>.+?)-"
    r"rate(?P<rate>[^-]+)-\d{8}T\d{6}\.csv$"
)


COLD_PODS_PER_REPETITION = {"solo": 1, "pair": 2}


def cold_ready_times(path, protocol):
    """Returns the cold scale-out ready times of the repetitions that ran the protocol.

    The scheduler sometimes places both pods of a single-pod repetition on
    cold nodes, which makes it a two-pod repetition, so only repetitions with
    exactly the protocol's number of cold pods are kept.
    """
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
    """Groups the cold ready times by configuration, image and protocol."""
    cells = {}
    for path in sorted(BENCHMARKS.glob("replica-*.csv")):
        match = CELL_PATTERN.match(path.name)
        if not match:
            continue
        key = (match["config"], match["image"], match["rate"])
        cells.setdefault(key, {})[match["protocol"]] = cold_ready_times(path, match["protocol"])
    return cells


def summarize(key, protocols):
    config, image, rate = key
    solo, solo_repetitions = protocols["solo"]
    pair, pair_repetitions = protocols["pair"]
    difference, low, high = difference_interval(pair, solo)

    # the bootstrap draws whole repetitions, the plain statistics use every pod
    solo = [time for times in solo for time in times]
    pair = [time for times in pair for time in times]

    return {
        "config": config,
        "image": image,
        "rate": rate,
        "solo_repetitions": solo_repetitions,
        "pair_repetitions": pair_repetitions,
        "solo_pods": len(solo),
        "pair_pods": len(pair),
        "solo_mean_s": round(statistics.mean(solo), 3),
        "pair_mean_s": round(statistics.mean(pair), 3),
        "second_replica_cost_s": round(difference, 3),
        "interval_low_s": round(low, 3),
        "interval_high_s": round(high, 3),
        "solo_median_s": round(statistics.median(solo), 3),
        "pair_median_s": round(statistics.median(pair), 3),
        "median_difference_s": round(statistics.median(pair) - statistics.median(solo), 3),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--output", default=str(SUMMARIES / "replica-summary.csv"))
    args = parser.parse_args()

    summary = []
    for key, protocols in sorted(collect_cells().items()):
        # A cell whose partner protocol is still missing has nothing to compare against
        if "solo" in protocols and "pair" in protocols:
            summary.append(summarize(key, protocols))

    with open(args.output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(summary[0].keys()))
        writer.writeheader()
        writer.writerows(summary)

    for row in summary:
        print(row)


if __name__ == "__main__":
    main()
