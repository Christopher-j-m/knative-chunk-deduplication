#!/usr/bin/env python3
"""Holds every stock nydusd cell of a prefetch session against the cluster cache, cold start and seed.

The session summary reports the cache's lead only over the faster stock mode.
The text also quotes the lead over each mode on its own, how much slower the
cached seed is than a stock seed, and what prefetch does to the stock seed.
Per image, conversion, and prefetch mode this reports those figures with
bootstrap intervals that resample whole repetitions. The cache cell can come
from another session, which is how a repeated cache cell is read against the
stock cells of the session it repeats.

Usage: compare-stock-with-cache.py --prefix prefetch-zstd100k --cells fn-go-hello:nydus-zstd,... [--rtt 80]
       [--cache-prefix <prefix>] [--output analysis/results/summaries/<prefix>-cache-comparison.csv]
"""

import argparse
import csv
import importlib.util
import sys
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))
from cell_bootstrap import difference_interval, pooled_mean, ratio_interval  # noqa: E402

# The summary script already knows how to find a cell and read its pods, and
# its file name has dashes, so it is loaded by path instead of imported by name
_summary_spec = importlib.util.spec_from_file_location("summarize_prefetch_stock", SCRIPTS / "summarize-prefetch-stock.py")
summary = importlib.util.module_from_spec(_summary_spec)
_summary_spec.loader.exec_module(summary)

PREFETCH_MODES = ["off", "all"]


def read_cell(prefix, config, image, conversion, mode, rtt):
    """Reads the cold starts and the seeds of one cell by repetition, with the file they came from."""
    path = summary.cell_path(prefix, config, image, conversion, mode, rtt)
    if path is None:
        raise SystemExit(f"missing cell: {prefix}, config {config}, {image}, {conversion}, prefetch {mode}, {rtt} ms")

    return {
        "cold": summary.grouped_times(summary.cell_rows(path, "scaleout")),
        "seed": summary.grouped_times(summary.cell_rows(path, "seed")),
        "file": path.name,
    }


def with_interval(row, name, estimate, unit=""):
    """Writes an estimate and the two ends of its interval into a row, rounded to three decimals."""
    value, low, high = estimate
    row[f"{name}{unit}"] = round(value, 3)
    row[f"{name}_ci_low{unit}"] = round(low, 3)
    row[f"{name}_ci_high{unit}"] = round(high, 3)


def compare_mode(stock, stock_without_prefetch, cache, mode):
    """Puts one stock cell's comparison with the cache cell into the columns of a row."""
    row = {
        "stock_cold_mean_s": round(pooled_mean(stock["cold"]), 3),
        "cache_cold_mean_s": round(pooled_mean(cache["cold"]), 3),
    }

    # Above one, the cache starts a cross-node cold pod faster than stock nydusd
    with_interval(row, "stock_over_cache", ratio_interval(stock["cold"], cache["cold"]))

    # The cached seed pays for the misses of the whole touched set, so it is the slower one
    row["stock_seed_mean_s"] = round(pooled_mean(stock["seed"]), 3)
    row["cache_seed_mean_s"] = round(pooled_mean(cache["seed"]), 3)
    with_interval(row, "cache_seed_over_stock", ratio_interval(cache["seed"], stock["seed"]))
    with_interval(row, "cache_seed_minus_stock", difference_interval(cache["seed"], stock["seed"]), unit="_s")

    # Below one, prefetch made the stock seed faster; the prefetch-off row is its own reference
    if mode == "off":
        for column in ("stock_seed_over_prefetch_off", "stock_seed_over_prefetch_off_ci_low",
                       "stock_seed_over_prefetch_off_ci_high"):
            row[column] = ""
    else:
        with_interval(row, "stock_seed_over_prefetch_off",
                      ratio_interval(stock["seed"], stock_without_prefetch["seed"]))

    row["stock_file"] = stock["file"]
    row["cache_file"] = cache["file"]
    return row


def compare_image(args, image, conversion):
    """Returns one row per prefetch mode of an image's stock cells on one conversion."""
    # The cache serves only the uncompressed conversion and runs without prefetch
    cache = read_cell(args.cache_prefix or args.prefix, "4", image, "nydus-none", "off", args.rtt)
    stock_by_mode = {mode: read_cell(args.prefix, "2s", image, conversion, mode, args.rtt) for mode in PREFETCH_MODES}

    rows = []
    for mode in PREFETCH_MODES:
        row = {"image": image, "conversion": conversion, "prefetch": mode}
        row.update(compare_mode(stock_by_mode[mode], stock_by_mode["off"], cache, mode))
        rows.append(row)
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", required=True, help="session of the stock cells")
    parser.add_argument("--cells", type=summary.parse_cells, required=True, help="comma separated image:conversion pairs")
    parser.add_argument("--rtt", type=int, default=summary.RTT, help="added round trip of the session in ms")
    parser.add_argument("--cache-prefix", help="session of the cache cells, when it is not the same one")
    parser.add_argument("--output")
    args = parser.parse_args()

    output = args.output or str(summary.SUMMARIES / f"{args.prefix}-cache-comparison.csv")
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
