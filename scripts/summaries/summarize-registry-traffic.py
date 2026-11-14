#!/usr/bin/env python3
"""Counts the registry requests and bytes of each benchmark phase from the registry's request log"""
import argparse
import csv
import lzma
import re
from datetime import datetime, timedelta
from pathlib import Path
from result_paths import summary_path

TIMESTAMP = re.compile(r'time="([^"]+)"')
WRITTEN = re.compile(r"http\.response\.written=(\d+)")
BLOB = re.compile(r"http\.request\.uri=\"?/v2/([^/]+)/blobs/")


def parse_time(value):
    cleaned = value.strip().replace("Z", "+00:00")
    if "." in cleaned:
        head, rest = cleaned.split(".", 1)
        fraction, _, offset = rest.partition("+")
        cleaned = f"{head}.{fraction[:6]}+{offset}"
    return datetime.fromisoformat(cleaned)


def open_log(path):
    if path.suffix == ".xz":
        return lzma.open(path, "rt", errors="replace")
    return path.open(errors="replace")


def read_requests(path):
    requests = []
    with open_log(path) as handle:
        for line in handle:
            stamp = TIMESTAMP.search(line)
            blob = BLOB.search(line)
            if not stamp or not blob:
                continue
            written = WRITTEN.search(line)
            requests.append(
                (parse_time(stamp.group(1)), blob.group(1), int(written.group(1)) if written else 0)
            )
    requests.sort(key=lambda item: item[0])
    return requests


def count_between(requests, image, start, end):
    fetches = 0
    written = 0
    for at, name, size in requests:
        if name == image and start <= at < end:
            fetches += 1
            written += size
    return fetches, written


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--windows", required=True, help="CSV of phase boundaries")
    parser.add_argument("--log", required=True, help="Captured registry request log")
    parser.add_argument(
        "--out",
        default=str(summary_path("registry-traffic-summary.csv")),
        help="Where to write the per-phase totals",
    )
    parser.add_argument(
        "--end-slack-seconds",
        type=float,
        default=0.0,
        help="Extends each scale-out window by this much; the driver stamps the end of a phase "
        "in whole seconds, so 1 covers the responses that complete in the cut-off fraction",
    )
    args = parser.parse_args()

    requests = read_requests(Path(args.log))
    if not requests:
        raise SystemExit(f"no blob responses parsed from {args.log}")

    rows = []
    with open(args.windows) as handle:
        for window in csv.DictReader(handle):
            image = window["image"]
            reset_end = parse_time(window["reset_end"])
            seed_end = parse_time(window["seed_end"])
            scaleout_end = parse_time(window["scaleout_end"])

            scaleout_end += timedelta(seconds=args.end_slack_seconds)

            seed_fetches, seed_bytes = count_between(requests, image, reset_end, seed_end)
            scale_fetches, scale_bytes = count_between(requests, image, seed_end, scaleout_end)

            rows.append({
                "config": window["config"],
                "nodes": int(window["nodes"]),
                "image": image,
                "rtt_ms": window["rtt_ms"],
                "seed_fetches": seed_fetches,
                "seed_bytes": seed_bytes,
                "scaleout_fetches": scale_fetches,
                "scaleout_bytes": scale_bytes,
                "total_fetches": seed_fetches + scale_fetches,
                "total_bytes": seed_bytes + scale_bytes,
            })

    rows.sort(key=lambda row: (row["image"], row["config"], row["nodes"]))

    baseline = {
        (row["image"], row["nodes"]): row["total_fetches"]
        for row in rows
        if row["config"] == "2"
    }
    for row in rows:
        reference = baseline.get((row["image"], row["nodes"]), 0)
        if row["config"] == "4" and reference > 0:
            row["fetch_saving"] = round(1 - row["total_fetches"] / reference, 4)
            row["predicted"] = round((row["nodes"] - 1) / row["nodes"], 4)
        else:
            row["fetch_saving"] = ""
            row["predicted"] = ""

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)

    print(f"wrote {len(rows)} phases to {out}")
    for row in rows:
        if row["fetch_saving"] != "":
            print(
                f"  {row['image']} at {row['nodes']} nodes: "
                f"{row['fetch_saving']:.1%} of registry fetches removed, "
                f"predicted {row['predicted']:.1%}"
            )


if __name__ == "__main__":
    main()
