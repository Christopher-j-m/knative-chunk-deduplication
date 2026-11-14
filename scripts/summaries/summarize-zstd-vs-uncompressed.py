#!/usr/bin/env python3
"""Compares plain nydusd on the zstd conversion with the uncompressed conversion and the cluster cache"""
import argparse
import csv
import importlib.util
import sys
from pathlib import Path
from result_paths import summary_path, find_benchmarks

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))
from cell_bootstrap import difference_interval, pooled_mean, ratio_interval  # noqa: E402

_summary_spec = importlib.util.spec_from_file_location("summarize_prefetch_plain", SCRIPTS / "summarize-plain-prefetch.py")
summary = importlib.util.module_from_spec(_summary_spec)
_summary_spec.loader.exec_module(summary)

LADDER_PREFIX = "ladder"
PREFETCH_MODES = ["off", "all"]


def eager_groups(image, rtt):
    stem = f"{LADDER_PREFIX}-config1-{image}-rtt{rtt}ms-rate{summary.RATE}-"
    matches = sorted(
        path for path in find_benchmarks(f"{stem}*.csv") if summary.MEASUREMENT_NAME.search(path.name)
    )
    if not matches:
        raise SystemExit(f"missing ladder cell: eager pull, {image}, {rtt} ms")
    return summary.grouped_times(summary.cell_rows(matches[-1], "scaleout"))


def phase_groups(args, config, image, conversion, mode, phase):
    path = summary.cell_path(args.prefix, config, image, conversion, mode, rtt=args.rtt)
    if path is None:
        raise SystemExit(f"missing cell: {args.prefix}, config {config}, {image}, {conversion}, prefetch {mode}")
    return summary.grouped_times(summary.cell_rows(path, phase))


def compare_conversions(args, image, mode):
    row = {"image": image, "prefetch": mode}

    for phase, column in (("scaleout", "cold"), ("seed", "seed")):
        uncompressed = phase_groups(args, "2s", image, "nydus-none", mode, phase)
        compressed = phase_groups(args, "2s", image, "nydus-zstd", mode, phase)
        ratio, low, high = ratio_interval(uncompressed, compressed)
        row[f"{column}_uncompressed_mean_s"] = round(pooled_mean(uncompressed), 3)
        row[f"{column}_zstd_mean_s"] = round(pooled_mean(compressed), 3)
        row[f"{column}_uncompressed_over_zstd"] = round(ratio, 3)
        row[f"{column}_uncompressed_over_zstd_ci_low"] = round(low, 3)
        row[f"{column}_uncompressed_over_zstd_ci_high"] = round(high, 3)

    compressed_cold = phase_groups(args, "2s", image, "nydus-zstd", mode, "scaleout")
    cache_cold = phase_groups(args, "4", image, "nydus-none", "off", "scaleout")
    lead, lead_low, lead_high = ratio_interval(compressed_cold, cache_cold)
    row["zstd_over_cache"] = round(lead, 3)
    row["zstd_over_cache_ci_low"] = round(lead_low, 3)
    row["zstd_over_cache_ci_high"] = round(lead_high, 3)

    eager_cold = eager_groups(image, args.rtt)
    gap, gap_low, gap_high = difference_interval(compressed_cold, eager_cold)
    row["eager_cold_mean_s"] = round(pooled_mean(eager_cold), 3)
    row["zstd_minus_eager_s"] = round(gap, 3)
    row["zstd_minus_eager_ci_low_s"] = round(gap_low, 3)
    row["zstd_minus_eager_ci_high_s"] = round(gap_high, 3)
    return row


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", required=True)
    parser.add_argument("--images", required=True, help="comma separated image names")
    parser.add_argument("--rtt", type=int, default=summary.RTT, help="added round trip of the run in ms")
    parser.add_argument("--output")
    args = parser.parse_args()

    output = args.output or str(summary_path(f"{args.prefix}-zstd-vs-uncompressed.csv"))
    rows = []
    for image in args.images.split(","):
        for mode in PREFETCH_MODES:
            rows.append(compare_conversions(args, image.strip(), mode))

    with open(output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    for row in rows:
        print(row)


if __name__ == "__main__":
    main()
