#!/usr/bin/env python3
"""Compares stock nydusd with whole-image prefetch off and on next to the cluster cache.

Every image and conversion has three cells at 80 ms and 600 Mbit/s: stock
nydusd (configuration 2s) with prefetch off and with prefetch_all on, and the
cluster cache (configuration 4) with prefetch off on the uncompressed
conversion. Per image and conversion this reports every mode's cold ready
times, its seed and the bytes each cold node received, and two ratios with
bootstrap intervals. Under the cache a node's counter also sees the traffic
between the chunk proxy and nydusd inside the node, so there only the seed's
bytes measure the registry link.

A prefix names one session, and the cells of one prefix were measured
together. The first campaign ran twice on three images, once with the
emulator's default queue of 1,000 packets (prefix prefetch-stock) and once
with 100,000 (prefix prefetch-deepqueue100k). A later session with the deep
queue (prefix prefetch-ladder100k) measured all six images uncompressed,
three of them for the second time. The prefix prefetch-adjacent100k holds the
same cells for all six images at zero delay. The --cells option picks the
image and conversion pairs a prefix holds.

Usage: summarize-prefetch-stock.py [--prefix prefetch-stock] [--cells image:conversion,...] [--rtt 80]
       [--output analysis/results/summaries/<prefix>-summary.csv]
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

RTT = 80
RATE = "600mbit"

# The cluster cache reads only the uncompressed conversion, so its one cell per
# image is the reference for both conversions of stock nydusd. These are the
# pairs the first campaign measured, and the default when no --cells is given
DEFAULT_CELLS = [
    ("fn-python-hello", "nydus-none"),
    ("fn-python-ml", "nydus-none"),
    ("fn-pytorch-cpu", "nydus-none"),
    ("fn-python-hello", "nydus-zstd"),
    ("fn-pytorch-cpu", "nydus-zstd"),
]
MODES = [("stock_off", "2s", "off"), ("stock_all", "2s", "all"), ("cache_off", "4", "off")]

# Only the measurement file itself ends in its timestamp; the drain record,
# the prefetch verdict and any set-aside cell carry a further suffix
MEASUREMENT_NAME = re.compile(r"-\d{8}T\d{6}\.csv$")


def cell_path(prefix, config, image, conversion, mode, rtt=RTT):
    """Finds the newest measurement file of one cell, or None when it was never measured."""
    stem = f"{prefix}-config{config}-{image}-{conversion}-prefetch{mode}-rtt{rtt}ms-rate{RATE}-"
    matches = sorted(path for path in BENCHMARKS.glob(f"{stem}*.csv") if MEASUREMENT_NAME.search(path.name))
    if not matches:
        return None
    return matches[-1]


def cell_rows(path, phase):
    """Returns the rows of one phase of a cell that started on a node without the image."""
    with path.open() as handle:
        rows = list(csv.DictReader(handle))
    return [row for row in rows if row["phase"] == phase and row["warm"] == "no"]


def grouped_times(rows):
    """Groups ready times by repetition, the unit the bootstrap resamples."""
    by_repetition = {}
    for row in rows:
        by_repetition.setdefault(row["rep"], []).append(float(row["ready_seconds"]))
    return list(by_repetition.values())


def seed_verdicts(path):
    """Reads how much each repetition's seed received and whether that was the whole image."""
    verdict_path = path.with_name(path.name[: -len(".csv")] + ".prefetch-check.csv")
    with verdict_path.open() as handle:
        verdicts = list(csv.DictReader(handle))
    observed = [row["observed"] for row in verdicts]
    received = [int(row["received_bytes"]) for row in verdicts]
    return f"{observed.count('yes')}/{len(observed)}", round(sum(received) / len(received))


def rounded(value, digits=3):
    return round(value, digits)


def describe_mode(path):
    """Collects the cold start, seed and traffic figures of one cell."""
    cold_rows = cell_rows(path, "scaleout")
    cold = grouped_times(cold_rows)
    flat = [time for group in cold for time in group]
    low, high = mean_interval(cold)
    received = [int(row["node_rx_bytes"]) for row in cold_rows]
    dropped = [int(row["netem_dropped"] or 0) for row in cold_rows]
    observed, seed_received = seed_verdicts(path)

    figures = {
        "cold_mean_s": rounded(pooled_mean(cold)),
        "cold_ci_low_s": rounded(low),
        "cold_ci_high_s": rounded(high),
        "cold_min_s": rounded(min(flat)),
        "cold_max_s": rounded(max(flat)),
        "cold_n": len(flat),
        "seed_mean_s": rounded(pooled_mean(grouped_times(cell_rows(path, "seed")))),
        "seed_rx_mean_bytes": seed_received,
        "cold_node_rx_mean_bytes": round(sum(received) / len(received)),
        "netem_dropped_max": max(dropped),
        "prefetch_observed": observed,
    }
    return figures, cold


def summarize_cell(prefix, image, conversion, rtt):
    row = {"image": image, "conversion": conversion}
    cold_by_mode = {}

    for label, config, mode in MODES:
        # The cache cell is shared by both conversions and measured on the uncompressed one
        path = cell_path(prefix, config, image, "nydus-none" if config == "4" else conversion, mode, rtt)
        if path is None:
            raise SystemExit(f"missing cell: config {config}, {image}, {conversion}, prefetch {mode}")
        figures, cold = describe_mode(path)
        cold_by_mode[label] = cold
        for name, value in figures.items():
            row[f"{label}_{name}"] = value

    # Above one, turning prefetch on made stock nydusd slower
    ratio, low, high = ratio_interval(cold_by_mode["stock_all"], cold_by_mode["stock_off"])
    row["all_over_off"] = rounded(ratio)
    row["all_over_off_ci_low"] = rounded(low)
    row["all_over_off_ci_high"] = rounded(high)

    # The cache is held against whichever stock mode started faster
    faster = "stock_off"
    if pooled_mean(cold_by_mode["stock_all"]) < pooled_mean(cold_by_mode["stock_off"]):
        faster = "stock_all"
    lead, lead_low, lead_high = ratio_interval(cold_by_mode[faster], cold_by_mode["cache_off"])
    row["faster_stock_mode"] = faster
    row["faster_stock_over_cache"] = rounded(lead)
    row["faster_stock_over_cache_ci_low"] = rounded(lead_low)
    row["faster_stock_over_cache_ci_high"] = rounded(lead_high)
    return row


def parse_cells(text):
    """Turns a comma separated list of image:conversion pairs into tuples, keeping their order."""
    cells = []
    for entry in text.split(","):
        image, separator, conversion = entry.strip().partition(":")
        if not separator or not image or not conversion:
            raise argparse.ArgumentTypeError(f"expected image:conversion, got {entry!r}")
        cells.append((image, conversion))
    return cells


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", default="prefetch-stock")
    parser.add_argument("--cells", type=parse_cells, default=DEFAULT_CELLS)
    parser.add_argument("--rtt", type=int, default=RTT, help="added round trip of the session in ms")
    parser.add_argument("--output")
    args = parser.parse_args()

    output = args.output or str(SUMMARIES / f"{args.prefix}-summary.csv")
    summary = [summarize_cell(args.prefix, image, conversion, args.rtt) for image, conversion in args.cells]

    with open(output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(summary[0].keys()))
        writer.writeheader()
        writer.writerows(summary)

    for row in summary:
        print(row)


if __name__ == "__main__":
    main()
