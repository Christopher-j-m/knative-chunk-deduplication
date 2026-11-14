#!/usr/bin/env python3
"""Summarizes the cluster-size experiment into one row per cell"""
import argparse
import csv
import re
import statistics
from pathlib import Path
from result_paths import summary_path, benchmark_folder

CELL_PATTERN = re.compile(
    r"cluster-size-config(?P<config>\d+)-(?P<image>.+?)-n(?P<nodes>\d+)-"
    r"rtt(?P<rtt>\d+)ms-rate(?P<rate>[^-]+)-\d{8}T\d{6}\.csv"
)


def read_rows(path):
    with path.open() as handle:
        return list(csv.DictReader(handle))


def registry_fetches_per_repetition(rows):
    per_repetition = {}
    for row in rows:
        phase_totals = per_repetition.setdefault(row["rep"], {})
        phase_totals[row["phase"]] = float(row["cache_misses_delta"])

    return [sum(phases.values()) for phases in per_repetition.values()]


def summarize(path, match):
    rows = read_rows(path)
    scaleout = [row for row in rows if row["phase"] == "scaleout"]
    cold = [float(row["ready_seconds"]) for row in scaleout if row["warm"] == "no"]
    warm = [float(row["ready_seconds"]) for row in scaleout if row["warm"] == "yes"]

    totals = registry_fetches_per_repetition(rows)
    fetches = statistics.median(totals) if totals else 0.0

    return {
        "config": match["config"],
        "image": match["image"],
        "nodes": int(match["nodes"]),
        "rtt_ms": int(match["rtt"]),
        "rate": match["rate"],
        "cold_pods": len(cold),
        "cold_ready_median_s": round(statistics.median(cold), 3) if cold else "",
        "cold_ready_max_s": round(max(cold), 3) if cold else "",
        "warm_ready_median_s": round(statistics.median(warm), 3) if warm else "",
        "registry_fetches": round(fetches, 1),
    }


def add_traffic_saving(rows):
    baseline = {
        (row["image"], row["nodes"]): row["registry_fetches"]
        for row in rows
        if row["config"] == "2"
    }

    for row in rows:
        reference = baseline.get((row["image"], row["nodes"]), 0)
        if row["config"] == "4" and reference > 0:
            row["traffic_saving"] = round(1 - row["registry_fetches"] / reference, 4)
        else:
            row["traffic_saving"] = ""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--results",
        default=str(benchmark_folder("cluster-size")),
        help="Directory holding the experiment's per-cell CSVs",
    )
    parser.add_argument(
        "--out",
        default=str(summary_path("cluster-size-summary.csv")),
        help="Where to write the collapsed summary",
    )
    args = parser.parse_args()

    rows = []
    for path in sorted(Path(args.results).glob("cluster-size-*.csv")):
        match = CELL_PATTERN.fullmatch(path.name)
        if match:
            rows.append(summarize(path, match))

    if not rows:
        raise SystemExit(f"no cluster-size cells found under {args.results}")

    add_traffic_saving(rows)
    rows.sort(key=lambda row: (row["image"], row["config"], row["nodes"]))

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    print(f"wrote {len(rows)} cells to {out}")
    for row in rows:
        if row["config"] == "4":
            print(
                f"  {row['image']} at {row['nodes']} nodes: "
                f"cache paid {row['registry_fetches']:.0f} registry fetches, "
                f"cold start {row['cold_ready_median_s']} s median"
            )

    if all(row["traffic_saving"] == "" for row in rows):
        print(
            "  note: configuration 2 runs with the cache off, so its fetches do not\n"
            "  reach these counters; read the saving from the registry log instead"
        )


if __name__ == "__main__":
    main()
