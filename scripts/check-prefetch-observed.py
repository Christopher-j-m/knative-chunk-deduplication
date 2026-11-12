#!/usr/bin/env python3
"""Tells whether whole-image prefetch actually ran in every repetition of a cell.

A seed node that prefetches downloads all of the image's data blobs, part of
them before its pod is ready and the rest while the link drains afterwards.
What it received from the start of its phase until the link went quiet, as
the drain record holds it, is set against the size of the data blobs in the
image's manifest, and a repetition counts as prefetched once it reaches the
given fraction of that size. With --require, one repetition that falls short
fails the cell.

Usage: check-prefetch-observed.py --cell <cell.csv> --drain <drain.csv> --manifest <manifest.json> [--min-fraction 0.8] [--require]
       check-prefetch-observed.py --print-blob-bytes --manifest <manifest.json>
"""

import argparse
import csv
import json
import sys
from pathlib import Path

NYDUS_BLOB_ANNOTATION = "containerd.io/snapshot/nydus-blob"


def blob_bytes(manifest_path):
    """Adds up the image's data blobs, which leaves out the bootstrap layer.

    An image without data blobs is not a nydus conversion, and prefetch has
    nothing to fetch for it, so that is an error rather than a size of zero.
    """
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
    """Yields the repetition and the seed node of every seed row in the cell."""
    with open(cell_path, newline="") as handle:
        for row in csv.DictReader(handle):
            if row["phase"] == "seed":
                yield row["rep"], row["node"]


def seed_phase_bytes(drain_path):
    """Maps each repetition and seed node to everything it received from its phase start to quiet.

    A missing figure counts as nothing received, which can only make a
    repetition look less prefetched than it was, never more.
    """
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
    parser.add_argument("--cell", help="the cell's CSV from benchmark-scaleout.sh")
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

    # Each repetition is judged on its own seed node, so one good repetition
    # cannot hide a bad one
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
