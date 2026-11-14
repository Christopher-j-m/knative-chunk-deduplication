#!/usr/bin/env python3
"""Compares every plain nydusd cell of a prefetch run with the cluster cache, for the cold start and for the first start"""
import argparse
import csv
import importlib.util
import sys
from pathlib import Path
from result_paths import summary_path

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))
from cell_bootstrap import difference_interval, pooled_mean, ratio_interval  # noqa: E402

_summary_spec = importlib.util.spec_from_file_location("summarize_plain_prefetch", SCRIPTS / "summarize-plain-prefetch.py")
summary = importlib.util.module_from_spec(_summary_spec)
_summary_spec.loader.exec_module(summary)

PREFETCH_MODES = ["off", "all"]


def read_cell(prefix, config, image, conversion, mode, rtt):
    path = summary.cell_path(prefix, config, image, conversion, mode, rtt)
    if path is None:
        raise SystemExit(f"missing cell: {prefix}, config {config}, {image}, {conversion}, prefetch {mode}, {rtt} ms")

    return {
        "cold": summary.grouped_times(summary.cell_rows(path, "scaleout")),
        "seed": summary.grouped_times(summary.cell_rows(path, "seed")),
        "file": path.name,
    }


def with_interval(row, name, estimate, unit=""):
    value, low, high = estimate
    row[f"{name}{unit}"] = round(value, 3)
    row[f"{name}_ci_low{unit}"] = round(low, 3)
    row[f"{name}_ci_high{unit}"] = round(high, 3)


def compare_mode(plain, plain_without_prefetch, cache, mode):
    row = {
        "plain_cold_mean_s": round(pooled_mean(plain["cold"]), 3),
        "cache_cold_mean_s": round(pooled_mean(cache["cold"]), 3),
    }

    with_interval(row, "plain_over_cache", ratio_interval(plain["cold"], cache["cold"]))

    row["plain_seed_mean_s"] = round(pooled_mean(plain["seed"]), 3)
    row["cache_seed_mean_s"] = round(pooled_mean(cache["seed"]), 3)
    with_interval(row, "cache_seed_over_plain", ratio_interval(cache["seed"], plain["seed"]))
    with_interval(row, "cache_seed_minus_plain", difference_interval(cache["seed"], plain["seed"]), unit="_s")

    if mode == "off":
        for column in ("plain_seed_over_prefetch_off", "plain_seed_over_prefetch_off_ci_low",
                       "plain_seed_over_prefetch_off_ci_high"):
            row[column] = ""
    else:
        with_interval(row, "plain_seed_over_prefetch_off",
                      ratio_interval(plain["seed"], plain_without_prefetch["seed"]))

    row["plain_file"] = plain["file"]
    row["cache_file"] = cache["file"]
    return row


def compare_image(args, image, conversion):
    cache = read_cell(args.cache_prefix or args.prefix, "4", image, "nydus-none", "off", args.rtt)
    plain_by_mode = {mode: read_cell(args.prefix, "2s", image, conversion, mode, args.rtt) for mode in PREFETCH_MODES}

    rows = []
    for mode in PREFETCH_MODES:
        row = {"image": image, "conversion": conversion, "prefetch": mode}
        row.update(compare_mode(plain_by_mode[mode], plain_by_mode["off"], cache, mode))
        rows.append(row)
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", required=True, help="run of the plain cells")
    parser.add_argument("--cells", type=summary.parse_cells, required=True, help="comma separated image:conversion pairs")
    parser.add_argument("--rtt", type=int, default=summary.RTT, help="added round trip of the run in ms")
    parser.add_argument("--cache-prefix", help="run of the cache cells, when it is not the same one")
    parser.add_argument("--output")
    args = parser.parse_args()

    output = args.output or str(summary_path(f"{args.prefix}-cache-comparison.csv"))
    rows = []
    for image, conversion in args.cells:
        rows.extend(compare_image(args, image, conversion))

    with open(output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    for row in rows:
        print(row)


if __name__ == "__main__":
    main()
