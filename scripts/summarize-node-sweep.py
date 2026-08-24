#!/usr/bin/env python3
"""Collapses the node-count sweep's per-pod rows into one row per cell.

The sweep's question is what changes as the cluster grows, so the interesting
figures per cell are the slowest cold pod, which is when the scale-out is
actually finished, and the registry fetches the cluster paid.
"""

import argparse
import csv
import re
import statistics
from pathlib import Path

CELL_PATTERN = re.compile(
    r"node-sweep-config(?P<config>\d+)-(?P<image>.+?)-n(?P<nodes>\d+)-"
    r"rtt(?P<rtt>\d+)ms-rate(?P<rate>[^-]+)-\d{8}T\d{6}\.csv"
)


def read_rows(path):
    """Reads every row of one cell."""
    with path.open() as handle:
        return list(csv.DictReader(handle))


def registry_fetches_per_repetition(rows):
    """Totals the registry fetches one repetition of the whole scenario paid.

    Both phases count. Without the cache the seed fetches the touched set and
    every cold scale-out node fetches it again, so the total grows with the
    cluster; with the cache the seed pays once and the scale-out pays nothing.
    Comparing only the scale-out phase would credit the cache with removing
    every fetch rather than the (n-1)/n it actually removes. The harness writes
    the same delta onto every row of a phase, so each phase contributes its
    value once.
    """
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
    """Reads the (n-1)/n saving off the configuration 2 and 4 pair, when it can.

    Configuration 2 runs with the cache switched off, so its chunk fetches go
    straight from the node to the registry and no cache counter ever moves.
    The column below is therefore blank for those cells, and the ratio cannot
    be formed from these counters alone. The registry's own request log is what
    supplies the missing denominator; summarize-registry-traffic.py reads it.
    What this column does carry, for configuration 4, is the count that matters
    on its own: the fetches the cluster paid, which should stay flat as the
    node count grows.
    """
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
        default="analysis/results/benchmarks",
        help="Directory holding the sweep's per-cell CSVs",
    )
    parser.add_argument(
        "--out",
        default="analysis/results/summaries/node-sweep-summary.csv",
        help="Where to write the collapsed summary",
    )
    args = parser.parse_args()

    rows = []
    for path in sorted(Path(args.results).glob("node-sweep-*.csv")):
        match = CELL_PATTERN.fullmatch(path.name)
        if match:
            rows.append(summarize(path, match))

    if not rows:
        raise SystemExit(f"no node-sweep cells found under {args.results}")

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
