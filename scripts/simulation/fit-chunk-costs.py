#!/usr/bin/env python3
"""Fits the cost per chunk that the simulator charges from the cold starts of the ladder"""
import glob
import json
import sys
from pathlib import Path

import pandas as pd

ROOT = Path(__file__).resolve().parent.parent.parent
sys.path.insert(0, str(ROOT / "scripts" / "summaries"))
from result_paths import find_benchmarks  # noqa: E402
IMAGES = ["fn-python-hello", "fn-python-ml"]


def touched_counts():
    inventory = json.loads((ROOT / "analysis/results/chunks-live-touched.json").read_text())
    return {img["image"]: len({c["hash"] for c in img["chunks"]}) for img in inventory}


def cell(config, image, rtt):
    files = [str(path) for path in find_benchmarks(f"ladder-config{config}-{image}-rtt{rtt}ms-rate600mbit-*.csv")]
    if not files:
        sys.exit(f"no ladder cell for config {config} {image} {rtt} ms")
    return pd.read_csv(files[-1])


def main():
    chunks = touched_counts()
    rows = []
    for rtt in (0, 80):
        for image in IMAGES:
            warm = cell(4, image, rtt)
            floor = warm[warm["warm"] == "yes"]["ready_seconds"].mean() * 1000
            cold2 = cell(2, image, rtt)
            cold2s = cell("2s", image, rtt)
            cold4 = cell(4, image, rtt)
            cross2 = cold2[(cold2["phase"] == "scaleout") & (cold2["warm"] == "no")]["ready_seconds"].mean() * 1000
            cross2s = cold2s[(cold2s["phase"] == "scaleout") & (cold2s["warm"] == "no")]["ready_seconds"].mean() * 1000
            cross4 = cold4[(cold4["phase"] == "scaleout") & (cold4["warm"] == "no")]["ready_seconds"].mean() * 1000
            rows.append({
                "rtt_ms": rtt, "image": image, "chunks": chunks[image],
                "warm_floor_ms": round(floor, 1),
                "registry_chunk_ms": round((cross2 - floor) / chunks[image], 3),
                "plain_chunk_ms": round((cross2s - floor) / chunks[image], 3),
                "cache_chunk_ms": round((cross4 - floor) / chunks[image], 3),
            })
    fit = pd.DataFrame(rows)
    means = fit.groupby("rtt_ms")[["warm_floor_ms", "registry_chunk_ms", "plain_chunk_ms", "cache_chunk_ms"]].mean().round(3).reset_index()
    means["image"] = "mean"
    means["chunks"] = ""
    out = pd.concat([fit, means], ignore_index=True)
    out.to_csv(ROOT / "analysis/results/simulation/chunk-costs.csv", index=False)
    print(out.to_string(index=False))


if __name__ == "__main__":
    main()
