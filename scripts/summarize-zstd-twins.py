#!/usr/bin/env python3
"""Sets stock nydusd's cells on the zstd conversion beside its cells on the uncompressed one.

Both conversions of an image were measured in one session under one prefix,
with prefetch off and on. Per image and prefetch mode this reports how many
times longer the uncompressed conversion takes, the compressed cell's ratio
to the cluster cache, and its gap to the eager pull of the ladder session.

Usage: summarize-zstd-twins.py --prefix prefetch-zstd100k --images fn-go-hello,... [--rtt 80]
       [--output analysis/results/summaries/<prefix>-compression.csv]
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

LADDER_PREFIX = "ladder2"
PREFETCH_MODES = ["off", "all"]


def eager_groups(image, rtt):
    """Reads the cold ready times of the ladder's eager pull of one image, grouped by repetition."""
    stem = f"{LADDER_PREFIX}-config1-{image}-rtt{rtt}ms-rate{summary.RATE}-"
    matches = sorted(
        path for path in summary.BENCHMARKS.glob(f"{stem}*.csv") if summary.MEASUREMENT_NAME.search(path.name)
    )
    if not matches:
        raise SystemExit(f"missing ladder cell: eager pull, {image}, {rtt} ms")
    return summary.grouped_times(summary.cell_rows(matches[-1], "scaleout"))


def phase_groups(args, config, image, conversion, mode, phase):
    """Reads one phase of one cell as ready times grouped by repetition."""
    path = summary.cell_path(args.prefix, config, image, conversion, mode, rtt=args.rtt)
    if path is None:
        raise SystemExit(f"missing cell: {args.prefix}, config {config}, {image}, {conversion}, prefetch {mode}")
    return summary.grouped_times(summary.cell_rows(path, phase))


def compare_conversions(args, image, mode):
    """Puts one image's comparison of the two conversions under one prefetch mode into a row."""
    row = {"image": image, "prefetch": mode}

    for phase, column in (("scaleout", "cold"), ("seed", "seed")):
        plain = phase_groups(args, "2s", image, "nydus-none", mode, phase)
        compressed = phase_groups(args, "2s", image, "nydus-zstd", mode, phase)
        ratio, low, high = ratio_interval(plain, compressed)
        row[f"{column}_plain_mean_s"] = round(pooled_mean(plain), 3)
        row[f"{column}_zstd_mean_s"] = round(pooled_mean(compressed), 3)
        row[f"{column}_plain_over_zstd"] = round(ratio, 3)
        row[f"{column}_plain_over_zstd_ci_low"] = round(low, 3)
        row[f"{column}_plain_over_zstd_ci_high"] = round(high, 3)

    # The cache serves only the uncompressed conversion and runs without prefetch
    compressed_cold = phase_groups(args, "2s", image, "nydus-zstd", mode, "scaleout")
    cache_cold = phase_groups(args, "4", image, "nydus-none", "off", "scaleout")
    lead, lead_low, lead_high = ratio_interval(compressed_cold, cache_cold)
    row["zstd_over_cache"] = round(lead, 3)
    row["zstd_over_cache_ci_low"] = round(lead_low, 3)
    row["zstd_over_cache_ci_high"] = round(lead_high, 3)

    # Above zero, compressed stock lazy pulling is slower than the eager pull,
    # and an interval that spans zero leaves the two unseparated
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
    parser.add_argument("--rtt", type=int, default=summary.RTT, help="added round trip of the session in ms")
    parser.add_argument("--output")
    args = parser.parse_args()

    output = args.output or str(summary.SUMMARIES / f"{args.prefix}-compression.csv")
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
