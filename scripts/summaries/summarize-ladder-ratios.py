#!/usr/bin/env python3
"""Compares the configurations of the ladder with bootstrap intervals"""
import argparse
import glob
import os
import re

import pandas as pd

from cell_bootstrap import difference_interval, ratio_interval, removed_share_interval, repetition_groups
from result_paths import summary_path, find_benchmarks


COLD_RATIOS = [("2s", "4"), ("2", "4"), ("1", "4"), ("2", "2s"), ("2s", "1"),
               ("3", "4"), ("2s", "3"), ("1", "3")]

REMOVED_SHARES = [("2", "4"), ("2s", "4")]

SEED_DIFFERENCES = [("4", "2"), ("4", "2s"), ("3", "2s"), ("3", "4")]
COLD_DIFFERENCES = [("4", "1"), ("2s", "1")]


def cell_pattern(prefix):
    return re.compile(
        rf"{re.escape(prefix)}-config(?P<config>\d+s?)-(?P<image>fn-[a-z-]+)"
        r"-rtt(?P<rtt>\d+)ms-rate(?P<rate>[a-z0-9]+)-\d{8}T\d{6}\.csv$"
    )


def pod_groups(frame):
    scaleout = frame[frame["phase"] == "scaleout"]
    return {
        "cold": repetition_groups(scaleout[scaleout["warm"] == "no"]),
        "warm": repetition_groups(scaleout[scaleout["warm"] == "yes"]),
        "seed": repetition_groups(frame[frame["phase"] == "seed"]),
    }


def load_cells(benchmark_dir, prefix):
    pattern = cell_pattern(prefix)
    cells = {}
    if benchmark_dir is None:
        paths = [str(path) for path in find_benchmarks(f"{prefix}-config*.csv")]
    else:
        paths = sorted(glob.glob(os.path.join(benchmark_dir, f"{prefix}-config*.csv")))

    for path in paths:
        match = pattern.search(os.path.basename(path))
        if not match:
            continue
        key = (match["image"], int(match["rtt"]), match["rate"], match["config"])
        cells[key] = pod_groups(pd.read_csv(path))
    return cells


def row(image, rtt, rate, quantity, first, second, result):
    estimate, low, high = result
    return {
        "image": image,
        "rtt_ms": rtt,
        "rate": rate,
        "quantity": quantity,
        "first_config": first,
        "second_config": second,
        "estimate": round(estimate, 4),
        "ci_low": round(low, 4),
        "ci_high": round(high, 4),
    }


def compare(cells, image, rtt, rate):
    def cell(config):
        return cells.get((image, rtt, rate, config))

    rows = []
    for first, second in COLD_RATIOS:
        if cell(first) is not None and cell(second) is not None:
            result = ratio_interval(cell(first)["cold"], cell(second)["cold"])
            rows.append(row(image, rtt, rate, "cold_ratio", first, second, result))

    for baseline, cached in REMOVED_SHARES:
        if cell(baseline) is not None and cell(cached) is not None:
            result = removed_share_interval(
                cell(baseline)["cold"], cell(baseline)["warm"],
                cell(cached)["cold"], cell(cached)["warm"])
            rows.append(row(image, rtt, rate, "above_floor_removed", baseline, cached, result))

    for first, second in SEED_DIFFERENCES:
        if cell(first) is not None and cell(second) is not None:
            result = difference_interval(cell(first)["seed"], cell(second)["seed"])
            rows.append(row(image, rtt, rate, "seed_difference_s", first, second, result))

    for first, second in COLD_DIFFERENCES:
        if cell(first) is not None and cell(second) is not None:
            result = difference_interval(cell(first)["cold"], cell(second)["cold"])
            rows.append(row(image, rtt, rate, "cold_difference_s", first, second, result))
    return rows


def parse_arguments():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("benchmark_dir", nargs="?")
    parser.add_argument("--prefix", default="ladder", help="file prefix of the run to read")
    return parser.parse_args()


def main():
    arguments = parse_arguments()
    cells = load_cells(arguments.benchmark_dir, arguments.prefix)
    points = sorted({(image, rtt, rate) for image, rtt, rate, _ in cells})

    rows = []
    for image, rtt, rate in points:
        rows.extend(compare(cells, image, rtt, rate))
    table = pd.DataFrame(rows)

    out = summary_path(f"{arguments.prefix}-ratios.csv")
    table.to_csv(out, index=False)
    print(table.to_string(index=False))


if __name__ == "__main__":
    main()
