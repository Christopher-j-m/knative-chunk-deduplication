#!/usr/bin/env python3
"""Merges the saturation runs into one table and reports where the pod tops out.

The knee is taken as the first concurrency level whose throughput gain over the
best level below it is under five percent, which is the point past which adding
requesters buys latency rather than work.
"""

import argparse
import csv
from pathlib import Path

KNEE_GROWTH = 0.05


def mark_knee(rows):
    """Flags the first level whose throughput gain over the previous one is small."""
    by_chunk = {}
    for row in rows:
        by_chunk.setdefault(row["chunk_size_bytes"], []).append(row)

    for series in by_chunk.values():
        series.sort(key=lambda row: int(row["concurrency"]))
        best = None
        knee_found = False

        for row in series:
            rate = float(row["requests_per_second"])
            row["at_knee"] = "no"

            if best is not None and not knee_found and rate <= best * (1 + KNEE_GROWTH):
                row["at_knee"] = "yes"
                knee_found = True

            best = rate if best is None else max(best, rate)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--results",
        default="analysis/results/benchmarks",
        help="Directory holding the saturation CSVs",
    )
    parser.add_argument(
        "--out",
        default="analysis/results/summaries/cache-saturation-summary.csv",
        help="Where to write the merged table",
    )
    args = parser.parse_args()

    rows = []
    for path in sorted(Path(args.results).glob("cache-saturation-*.csv")):
        with path.open() as handle:
            rows.extend(csv.DictReader(handle))

    if not rows:
        raise SystemExit(f"no saturation runs found under {args.results}")

    mark_knee(rows)
    rows.sort(key=lambda row: (int(row["chunk_size_bytes"]), int(row["concurrency"])))

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    print(f"wrote {len(rows)} points to {out}")
    for row in rows:
        if row["at_knee"] == "yes":
            print(
                f"  knee at concurrency {row['concurrency']} "
                f"for {row['chunk_size_bytes']}-byte chunks: "
                f"{row['requests_per_second']} req/s, "
                f"{row['mib_per_second']} MiB/s, p99 {row['p99_ms']} ms"
            )


if __name__ == "__main__":
    main()
