#!/usr/bin/env python3
"""Compares a prefetch session's uncompressed cells with the same cells of the ladder session.

The ladder measured stock nydusd with prefetch off and the cluster cache on
every image at both distances, so a later session that repeats those cells
can be held against it. Per image and cell this reports both cold means and
the later one's ratio to the ladder's, with a bootstrap interval that
resamples whole repetitions.

Usage: compare-prefetch-with-ladder.py --prefix prefetch-adjacent100k --rtt 0 --images fn-go-hello,...
       [--cells stock_off,cache_off] [--output analysis/results/summaries/<prefix>-ladder-anchor.csv]
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

LADDER_PREFIX = "ladder2"

# The two cells both sessions hold: stock nydusd with prefetch off, and the cluster cache
SHARED_CELLS = [("stock_off", "2s"), ("cache_off", "4")]


def ladder_groups(config, image, rtt):
    """Reads the cold ready times of one ladder cell by repetition, with the file they came from."""
    stem = f"{LADDER_PREFIX}-config{config}-{image}-rtt{rtt}ms-rate{summary.RATE}-"
    matches = sorted(
        path for path in summary.BENCHMARKS.glob(f"{stem}*.csv") if summary.MEASUREMENT_NAME.search(path.name)
    )
    if not matches:
        raise SystemExit(f"missing ladder cell: config {config}, {image}, {rtt} ms")
    return summary.grouped_times(summary.cell_rows(matches[-1], "scaleout")), matches[-1].name


def session_groups(args, config, image):
    """Reads the cold ready times of the session's uncompressed cell with prefetch off."""
    path = summary.cell_path(args.prefix, config, image, "nydus-none", "off", rtt=args.rtt)
    if path is None:
        raise SystemExit(f"missing cell: {args.prefix}, config {config}, {image}, {args.rtt} ms")
    return summary.grouped_times(summary.cell_rows(path, "scaleout")), path.name


def compare_cell(args, image, label, config):
    """Puts one cell's comparison with the ladder into a row."""
    reference, reference_file = ladder_groups(config, image, args.rtt)
    later, later_file = session_groups(args, config, image)
    ratio, low, high = ratio_interval(later, reference)

    return {
        "image": image,
        "cell": label,
        "config": config,
        "rtt_ms": args.rtt,
        "ladder_cold_mean_s": round(pooled_mean(reference), 3),
        "cold_mean_s": round(pooled_mean(later), 3),
        "cold_ratio": round(ratio, 4),
        "cold_ratio_ci_low": round(low, 4),
        "cold_ratio_ci_high": round(high, 4),
        "cold_change_percent": round((ratio - 1) * 100, 2),
        "ladder_file": reference_file,
        "file": later_file,
    }


def parse_cells(text):
    """Picks the shared cells to compare by their label, in the order the ladder lists them."""
    wanted = [label.strip() for label in text.split(",")]
    known = [label for label, _ in SHARED_CELLS]

    for label in wanted:
        if label not in known:
            raise argparse.ArgumentTypeError(f"unknown cell {label!r}; choose from {', '.join(known)}")
    return [cell for cell in SHARED_CELLS if cell[0] in wanted]


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", required=True)
    parser.add_argument("--rtt", type=int, required=True, help="added round trip of both sessions in ms")
    parser.add_argument("--images", required=True, help="comma separated image names")
    parser.add_argument("--cells", type=parse_cells, default=SHARED_CELLS,
                        help="comma separated cells to compare: stock_off, cache_off")
    parser.add_argument("--output")
    args = parser.parse_args()

    output = args.output or str(summary.SUMMARIES / f"{args.prefix}-ladder-anchor.csv")
    rows = []
    for image in args.images.split(","):
        for label, config in args.cells:
            rows.append(compare_cell(args, image.strip(), label, config))

    with open(output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    for row in rows:
        print(row)


if __name__ == "__main__":
    main()
