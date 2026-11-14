#!/usr/bin/env python3
"""Compares the prefetch cells that two runs of the prefetch experiment both measured"""
import argparse
import csv
import importlib.util
import sys
from pathlib import Path
from result_paths import summary_path

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))
from cell_bootstrap import pooled_mean, ratio_interval  # noqa: E402

_summary_spec = importlib.util.spec_from_file_location("summarize_prefetch_plain", SCRIPTS / "summarize-plain-prefetch.py")
summary = importlib.util.module_from_spec(_summary_spec)
_summary_spec.loader.exec_module(summary)


def phase_groups(prefix, config, image, conversion, mode, phase, rtt):
    path = summary.cell_path(prefix, config, image, conversion, mode, rtt)
    if path is None:
        raise SystemExit(f"missing cell: {prefix}, config {config}, {image}, {conversion}, prefetch {mode}, {rtt} ms")
    return summary.grouped_times(summary.cell_rows(path, phase)), path.name


def compare_phase(reference, later):
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
                        help="comma separated cells to compare: plain_off, plain_all, cache_off")
    parser.add_argument("--rtt", type=int, default=summary.RTT, help="added round trip of both runs in ms")
    parser.add_argument("--output")
    args = parser.parse_args()

    output = args.output or str(summary_path(f"{args.prefix}-repeats.csv"))
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
