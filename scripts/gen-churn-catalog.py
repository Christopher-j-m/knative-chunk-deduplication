#!/usr/bin/env python3
"""Build a synthetic image catalog with version churn for the cache simulator.

Clones the calibrated live touched working sets into a catalog of the
requested size, keeping chunks shared across the calibrated corpus as a common
base and salting unique chunks per clone. Splits the trace into version epochs
and emits a rewritten trace, an explicit app-to-image assignment, and the
catalog working sets.
"""

import argparse
import hashlib
import json
import sys
from collections import Counter
from pathlib import Path

import pandas as pd


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--touched", required=True, help="chunks-live-touched.json")
    parser.add_argument("--trace", required=False, help="canonical trace CSV to rewrite")
    parser.add_argument("--images", type=int, required=True, help="catalog size")
    parser.add_argument("--epoch-hours", type=float, default=0, help="version epoch length; 0 disables churn")
    parser.add_argument("--delta", type=float, default=0.2, help="fraction of unique chunks rewritten per version")
    parser.add_argument("--rebase", type=float, default=0.2, help="fraction of shared base chunks a version rebases; 0 keeps the base frozen")
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--catalog-only", action="store_true", help="write working-sets.json only; no trace rewrite or assignment")
    return parser.parse_args()


def salted(hash_value, salt):
    return hashlib.sha256(f"{hash_value}/{salt}".encode()).hexdigest()


# Chunks that appear in two or more measured images are the shared base every
# clone keeps verbatim, so the catalog preserves real cross-image overlap
# instead of inventing it
def load_calibrated(path):
    images = json.loads(Path(path).read_text())
    seen_in = Counter()
    for image in images:
        for chunk_hash in {chunk["hash"] for chunk in image["chunks"]}:
            seen_in[chunk_hash] += 1

    shared = {chunk_hash for chunk_hash, count in seen_in.items() if count >= 2}
    return images, shared


def rebased_base(shared, epoch_count, rebase):
    """Map each version to the base chunks a base-image update rewrote.

    A base update reaches every image built on that base, so the replacement
    hashes are salted by version alone. Version 0 is the original base.
    """
    base = sorted(shared)
    span = int(len(base) * rebase)
    by_version = {0: {}}

    for version in range(1, max(epoch_count, 1)):
        rewritten = {}
        if base and span:
            for position in range((version - 1) * span, version * span):
                original = base[position % len(base)]
                rewritten[original] = salted(original, f"base-v{version}")
        by_version[version] = rewritten

    return by_version


def build_catalog(calibrated, shared, image_count, epoch_count, delta, rebase):
    base_updates = rebased_base(shared, epoch_count, rebase)
    catalog = []

    for index in range(image_count):
        source = calibrated[index % len(calibrated)]

        # deduplicate the source chunk list once, keeping first-seen sizes
        sizes = {}
        for chunk in source["chunks"]:
            sizes.setdefault(chunk["hash"], chunk["size"])
        unique = sorted(chunk_hash for chunk_hash in sizes if chunk_hash not in shared)

        for version in range(max(epoch_count, 1)):
            # each version rewrites a sliding window of the unique chunks, so
            # successive versions overlap by one minus delta the way a real
            # image update replaces part of its layers
            rewritten = set()
            span = int(len(unique) * delta)
            if unique:
                for position in range(version * span, (version + 1) * span):
                    rewritten.add(unique[position % len(unique)])

            chunks = []
            rebased = base_updates[version]
            for chunk_hash, size in sizes.items():
                if chunk_hash in shared:
                    # a rebased chunk keeps the same replacement hash in every
                    # image, because one base update moves them all together
                    chunks.append({"hash": rebased.get(chunk_hash, chunk_hash), "size": size})
                elif chunk_hash in rewritten:
                    chunks.append({"hash": salted(chunk_hash, f"img{index}v{version}"), "size": size})
                else:
                    chunks.append({"hash": salted(chunk_hash, f"img{index}"), "size": size})

            catalog.append({"image": f"img{index:03d}.v{version}", "chunks": chunks})

    return catalog


def main():
    args = parse_args()
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    calibrated, shared = load_calibrated(args.touched)

    # a catalog-only run skips the trace entirely, so a bigger catalog can be
    # replayed against the original trace instead of a churned rewrite of it
    if args.catalog_only:
        catalog = build_catalog(calibrated, shared, args.images, 1, args.delta, args.rebase)
        (out_dir / "working-sets.json").write_text(json.dumps(catalog))
        print(f"catalog images={args.images} chunks={sum(len(image['chunks']) for image in catalog)}")
        return

    if not args.trace:
        sys.exit("--trace is required unless --catalog-only is set")

    trace = pd.read_csv(args.trace)
    span_ms = trace["at_ms"].max() - trace["at_ms"].min()
    if args.epoch_hours > 0:
        # rounded, not truncated: a replay window is never exactly a whole
        # number of epochs long, and a window a few milliseconds short of two
        # epochs must still be split into two rather than silently collapsing
        # into the no-churn case
        epoch_count = max(round(span_ms / (args.epoch_hours * 3.6e6)), 1)
    else:
        epoch_count = 1

    catalog = build_catalog(calibrated, shared, args.images, epoch_count, args.delta, args.rebase)
    (out_dir / "working-sets.json").write_text(json.dumps(catalog))

    # apps rotate over the catalog by rank, and the epoch index comes from
    # when the invocation happened, so the replay engine needs no changes
    apps = sorted(trace["app"].unique())
    app_image = {app: f"img{index % args.images:03d}" for index, app in enumerate(apps)}
    epoch = ((trace["at_ms"] - trace["at_ms"].min()) // (span_ms // epoch_count + 1)).astype(int)
    trace["app"] = trace["app"].astype(str) + ".e" + epoch.astype(str)

    assignment = {}
    for app, image in app_image.items():
        for index in range(epoch_count):
            assignment[f"{app}.e{index}"] = f"{image}.v{index}"
    (out_dir / "assign.json").write_text(json.dumps(assignment))

    trace.to_csv(out_dir / "trace.csv", index=False)
    print(f"catalog images={args.images} epochs={epoch_count} chunks={sum(len(image['chunks']) for image in catalog)}")


if __name__ == "__main__":
    main()
