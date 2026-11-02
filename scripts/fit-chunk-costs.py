#!/usr/bin/env python3
"""Fit the per-chunk startup costs the replay charges from the ladder cells.

For each image and round trip, the warm floor is the mean ready time of the
warm config-4 cells. The registry cost per chunk is the config-2 cross-node
cold mean minus that floor, over the image's live touched chunk count; the
stock cost is the same for config 2s, stock nydusd without the proxy, and the
cache cost the same for config 4. The mean over python-hello and python-ml is
what the simulator uses, and the spread between the two images is reported
alongside it.
"""

import glob
import json
import sys
from pathlib import Path

import pandas as pd

ROOT = Path(__file__).resolve().parent.parent
BENCH = ROOT / "analysis/results/benchmarks"
IMAGES = ["fn-python-hello", "fn-python-ml"]


def touched_counts():
    inventory = json.loads((ROOT / "analysis/results/chunks-live-touched.json").read_text())
    return {img["image"]: len({c["hash"] for c in img["chunks"]}) for img in inventory}


def cell(config, image, rtt):
    files = sorted(glob.glob(str(BENCH / f"ladder2-config{config}-{image}-rtt{rtt}ms-rate600mbit-*.csv")))
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
                "stock_chunk_ms": round((cross2s - floor) / chunks[image], 3),
                "cache_chunk_ms": round((cross4 - floor) / chunks[image], 3),
            })
    fit = pd.DataFrame(rows)
    means = fit.groupby("rtt_ms")[["warm_floor_ms", "registry_chunk_ms", "stock_chunk_ms", "cache_chunk_ms"]].mean().round(3).reset_index()
    means["image"] = "mean"
    means["chunks"] = ""
    out = pd.concat([fit, means], ignore_index=True)
    out.to_csv(ROOT / "analysis/results/simulation/chunk-costs.csv", index=False)
    print(out.to_string(index=False))


if __name__ == "__main__":
    main()
