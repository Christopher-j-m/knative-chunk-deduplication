#!/usr/bin/env python3
"""Compares the prefetch cells of repeated images between two sessions of the prefetch campaign.

A later session repeats images that an earlier session already measured, which
shows whether the rows of both sessions can stand in one table. For each image
and each of its three cells (stock nydusd with prefetch off and on, and the
cluster cache) this reports the cold and seed means of both sessions and the
ratio of the later mean to the earlier one, with a bootstrap interval that
resamples whole repetitions.

Usage: compare-prefetch-sessions.py --reference-prefix prefetch-deepqueue100k --prefix prefetch-ladder100k
       [--images fn-python-hello,...] [--conversion nydus-none] [--modes stock_off,...] [--rtt 80]
       [--output <csv>]
"""

import argparse
import csv
import importlib.util
import sys
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))
from cell_bootstrap import pooled_mean, ratio_interval  # noqa: E402

# The summary script already knows how to find a cell and read its pods, and
# its file name has dashes, so it is loaded by path instead of imported by name
_summary_spec = importlib.util.spec_from_file_location("summarize_prefetch_stock", SCRIPTS / "summarize-prefetch-stock.py")
summary = importlib.util.module_from_spec(_summary_spec)
_summary_spec.loader.exec_module(summary)


def phase_groups(prefix, config, image, conversion, mode, phase, rtt):
    """Reads one phase of one cell as ready times grouped by repetition, with the file it came from."""
    path = summary.cell_path(prefix, config, image, conversion, mode, rtt)
    if path is None:
        raise SystemExit(f"missing cell: {prefix}, config {config}, {image}, {conversion}, prefetch {mode}, {rtt} ms")
    return summary.grouped_times(summary.cell_rows(path, phase)), path.name


def compare_phase(reference, later):
    """Returns both means and the later one's ratio to the reference, with its interval."""
    ratio, low, high = ratio_interval(later, reference)
    return {
        "reference_mean_s": round(pooled_mean(reference), 3),
        "mean_s": round(pooled_mean(later), 3),
        "ratio": round(ratio, 4),
        "ratio_ci_low": round(low, 4),
        "ratio_ci_high": round(high, 4),
        "change_percent": round((ratio - 1) * 100, 2),
    }


def compare_cell(args, image, label, config, mode):
    """Puts the cold and seed comparison of one image's cell into one row."""
    row = {"image": image, "cell": label, "config": config, "prefetch": mode}

    for phase, column in (("scaleout", "cold"), ("seed", "seed")):
        reference, reference_file = phase_groups(
            args.reference_prefix, config, image, args.conversion, mode, phase, args.rtt)
        later, later_file = phase_groups(args.prefix, config, image, args.conversion, mode, phase, args.rtt)
        for name, value in compare_phase(reference, later).items():
            row[f"{column}_{name}"] = value

    row["reference_file"] = reference_file
    row["file"] = later_file
    return row


def parse_modes(text):
    """Picks the cells to compare by their label, in the order the summary lists them."""
    wanted = [label.strip() for label in text.split(",")]
    known = [label for label, _, _ in summary.MODES]

    for label in wanted:
        if label not in known:
            raise argparse.ArgumentTypeError(f"unknown cell {label!r}; choose from {', '.join(known)}")
    return [mode for mode in summary.MODES if mode[0] in wanted]


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--reference-prefix", required=True)
    parser.add_argument("--prefix", required=True)
    parser.add_argument("--images", default="fn-python-hello", help="comma separated image names")
    parser.add_argument("--conversion", default="nydus-none")
    parser.add_argument("--modes", type=parse_modes, default=summary.MODES,
                        help="comma separated cells to compare: stock_off, stock_all, cache_off")
    parser.add_argument("--rtt", type=int, default=summary.RTT, help="added round trip of both sessions in ms")
    parser.add_argument("--output")
    args = parser.parse_args()

    output = args.output or str(summary.SUMMARIES / f"{args.prefix}-anchor.csv")
    rows = []
    for image in args.images.split(","):
        for label, config, mode in args.modes:
            rows.append(compare_cell(args, image.strip(), label, config, mode))

    with open(output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    for row in rows:
        print(row)


if __name__ == "__main__":
    main()
