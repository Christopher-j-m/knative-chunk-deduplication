#!/usr/bin/env python3
"""Merges the serving-capacity runs into one table and finds where the cache pod stops scaling"""
import argparse
import csv
from pathlib import Path
from result_paths import summary_path, benchmark_folder

MIN_GROWTH = 0.05


def mark_peak(rows):
    by_chunk = {}
    for row in rows:
        by_chunk.setdefault(row["chunk_size_bytes"], []).append(row)

    for series in by_chunk.values():
        series.sort(key=lambda row: int(row["concurrency"]))
        best = None
        peak_found = False

        for row in series:
            rate = float(row["requests_per_second"])
            row["at_peak"] = "no"

            if best is not None and not peak_found and rate <= best * (1 + MIN_GROWTH):
                row["at_peak"] = "yes"
                peak_found = True

            best = rate if best is None else max(best, rate)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--results",
        default=str(benchmark_folder("serving-capacity")),
        help="Directory holding the serving-capacity CSVs",
    )
    parser.add_argument(
        "--out",
        default=str(summary_path("serving-capacity-summary.csv")),
        help="Where to write the merged table",
    )
    args = parser.parse_args()

    rows = []
    for path in sorted(Path(args.results).glob("serving-capacity-*.csv")):
        with path.open() as handle:
            rows.extend(csv.DictReader(handle))

    if not rows:
        raise SystemExit(f"no serving-capacity runs found under {args.results}")

    mark_peak(rows)
    rows.sort(key=lambda row: (int(row["chunk_size_bytes"]), int(row["concurrency"])))

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    print(f"wrote {len(rows)} points to {out}")
    for row in rows:
        if row["at_peak"] == "yes":
            print(
                f"  peak at concurrency {row['concurrency']} "
                f"for {row['chunk_size_bytes']}-byte chunks: "
                f"{row['requests_per_second']} req/s, "
                f"{row['mib_per_second']} MiB/s, p99 {row['p99_ms']} ms"
            )


if __name__ == "__main__":
    main()
