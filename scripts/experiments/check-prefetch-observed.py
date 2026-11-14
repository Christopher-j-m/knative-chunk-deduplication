#!/usr/bin/env python3
"""Checks from the received bytes whether whole-image prefetch really ran in every repetition of a cell"""
import argparse
import csv
import json
import sys
from pathlib import Path

NYDUS_BLOB_ANNOTATION = "containerd.io/snapshot/nydus-blob"


def blob_bytes(manifest_path):
    manifest = json.loads(Path(manifest_path).read_text())
    total = sum(
        layer["size"]
        for layer in manifest.get("layers", [])
        if layer.get("annotations", {}).get(NYDUS_BLOB_ANNOTATION) == "true"
    )
    if total == 0:
        sys.exit(f"{manifest_path} lists no nydus data blobs; is this a nydus conversion?")
    return total


def seed_rows(cell_path):
    with open(cell_path, newline="") as handle:
        for row in csv.DictReader(handle):
            if row["phase"] == "seed":
                yield row["rep"], row["node"]


def seed_phase_bytes(drain_path):
    totals = {}
    if not Path(drain_path).exists():
        return totals
    with open(drain_path, newline="") as handle:
        for row in csv.DictReader(handle):
            if row.get("seed_phase_bytes"):
                totals[(row["repetition"], row["node"])] = int(row["seed_phase_bytes"])
    return totals


def main():
    parser = argparse.ArgumentParser(description="Tells whether whole-image prefetch ran in a cell.")
    parser.add_argument("--manifest", required=True, help="the registry manifest of the measured image")
    parser.add_argument("--cell", help="the cell's CSV from benchmark-scale-out.sh")
    parser.add_argument("--drain", help="the drain record that wait-for-quiet-nodes.sh appended to")
    parser.add_argument("--min-fraction", type=float, default=0.8)
    parser.add_argument("--require", action="store_true", help="exit with status 1 when a repetition falls short")
    parser.add_argument("--print-blob-bytes", action="store_true", help="print the image's data blob size and stop")
    args = parser.parse_args()

    expected = blob_bytes(args.manifest)
    if args.print_blob_bytes:
        print(expected)
        return 0
    if not args.cell or not args.drain:
        parser.error("--cell and --drain are needed unless --print-blob-bytes is given")

    totals = seed_phase_bytes(args.drain)
    writer = csv.writer(sys.stdout, lineterminator="\n")
    writer.writerow(["repetition", "seed_node", "received_bytes", "blob_bytes", "fraction", "observed"])

    short = 0
    for repetition, node in seed_rows(args.cell):
        received = totals.get((repetition, node), 0)
        fraction = received / expected
        observed = fraction >= args.min_fraction
        if not observed:
            short += 1
        writer.writerow([repetition, node, received, expected, f"{fraction:.3f}", "yes" if observed else "no"])

    if args.require and short:
        print(f"prefetch was not observed in {short} repetition(s) of {args.cell}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
