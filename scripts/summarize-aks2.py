#!/usr/bin/env python3
"""Summarize the guarded managed-cloud campaign with bootstrap intervals.

Reads every accepted cell of the same-region campaign and of the cross-region
anchors, and writes one row per cell with its cold, warm and seed statistics,
the mean eager pull time and the bytes each cold node received. A second file
holds the cold-start ratios between configurations of the same image and
region. Run with:
uv run --with pandas --with numpy scripts/summarize-aks2.py
"""

import argparse
import glob
import json
import os
import re

import pandas as pd

from cell_bootstrap import mean_interval, ratio_interval, repetition_groups

BENCH = os.path.join(os.path.dirname(__file__), "../analysis/results/benchmarks")
SUMMARIES = os.path.join(os.path.dirname(__file__), "../analysis/results/summaries")

# The second eastus session re-measured configuration 2 against a registry
# verified to be of the same Standard tier as the same-region one
REGIONS = {
    "westeurope": "aks2-scaleout",
    "eastus": "aks2-eastus-scaleout",
    "eastus-standard": "aks3-eastus-scaleout",
}
FACTS = {
    "westeurope": "aks2-image-facts.json",
    "eastus": "aks2-eastus-image-facts.json",
    "eastus-standard": "aks3-eastus-image-facts.json",
}

# Cold-start ratios as numerator over denominator, so a value above one means
# the denominator is faster
COLD_RATIOS = [("2", "4"), ("1", "4"), ("3", "4"), ("2", "3"), ("1", "2"), ("1", "3")]


def cell_pattern(prefix):
    return re.compile(
        rf"^{re.escape(prefix)}-config(?P<config>\d)-(?P<image>fn-[a-z-]+)-\d{{8}}T\d{{6}}\.csv$"
    )


# Only the newest accepted CSV of a cell counts; rejected attempts carry a
# different suffix and never match
def load_cells(prefix):
    pattern = cell_pattern(prefix)
    cells = {}
    for path in sorted(glob.glob(os.path.join(BENCH, f"{prefix}-config*.csv"))):
        match = pattern.match(os.path.basename(path))
        if match:
            cells[(match["config"], match["image"])] = pd.read_csv(path)
    return cells


def transfer_bytes(region):
    path = os.path.join(BENCH, FACTS[region])
    if not os.path.exists(path):
        return {}
    with open(path) as handle:
        facts = json.load(handle)
    return {image: tags["oci"]["transfer_bytes"] for image, tags in facts.items()}


def pod_groups(frame):
    scaleout = frame[frame["phase"] == "scaleout"]
    return {
        "cold": scaleout[scaleout["warm"] == "no"],
        "warm": scaleout[scaleout["warm"] == "yes"],
        "seed": frame[frame["phase"] == "seed"],
    }


def describe(pods):
    values = pods["ready_seconds"]
    low, high = mean_interval(repetition_groups(pods))
    return {
        "median_s": round(float(values.median()), 3),
        "mean_s": round(float(values.mean()), 3),
        "ci_low_s": round(low, 3),
        "ci_high_s": round(high, 3),
        "min_s": round(float(values.min()), 3),
        "max_s": round(float(values.max()), 3),
        "n": int(len(values)),
    }


def cell_row(region, config, image, frame, eager_bytes):
    groups = pod_groups(frame)
    row = {"region": region, "config": config, "image": image}

    for name in ("cold", "warm", "seed"):
        for key, value in describe(groups[name]).items():
            row[f"{name}_{key}"] = value

    # Only an eager pull has a pull time and a byte count worth reporting
    cold_and_seed = pd.concat([groups["cold"], groups["seed"]])
    if config == "1":
        row["cold_pull_mean_s"] = round(float(groups["cold"]["pull_seconds"].mean()), 3)
        row["seed_pull_mean_s"] = round(float(groups["seed"]["pull_seconds"].mean()), 3)
        row["transfer_bytes"] = eager_bytes.get(image, "")
        row["min_rx_bytes"] = int(cold_and_seed["node_rx_bytes"].min())
    row["seed_misses_mean"] = round(float(groups["seed"]["cache_misses_delta"].mean()), 1)
    # Cells recorded after the proxy learned to count failed registry fetches
    if "registry_fetch_errors" in frame.columns:
        row["failed_fetches_total"] = int(pd.to_numeric(frame["registry_fetch_errors"], errors="coerce").fillna(0).sum())
    row["scaleout_hits_mean"] = round(float(frame[frame["phase"] == "scaleout"]
                                            .drop_duplicates("rep")["cache_hits_delta"].mean()), 1)
    return row


def ratio_rows(region, cells):
    images = sorted({image for _, image in cells})
    rows = []
    for image in images:
        for first, second in COLD_RATIOS:
            if (first, image) not in cells or (second, image) not in cells:
                continue
            numerator = repetition_groups(pod_groups(cells[(first, image)])["cold"])
            denominator = repetition_groups(pod_groups(cells[(second, image)])["cold"])
            estimate, low, high = ratio_interval(numerator, denominator)
            rows.append({
                "region": region,
                "image": image,
                "numerator_config": first,
                "denominator_config": second,
                "cold_ratio": round(estimate, 3),
                "ci_low": round(low, 3),
                "ci_high": round(high, 3),
            })
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--out", default=os.path.join(SUMMARIES, "aks2-summary.csv"))
    parser.add_argument("--ratios", default=os.path.join(SUMMARIES, "aks2-ratios.csv"))
    arguments = parser.parse_args()

    cell_rows, ratios = [], []
    loaded = {}
    for region, prefix in REGIONS.items():
        cells = load_cells(prefix)
        loaded[region] = cells
        eager_bytes = transfer_bytes(region)
        for (config, image), frame in sorted(cells.items()):
            cell_rows.append(cell_row(region, config, image, frame, eager_bytes))
        ratios.extend(ratio_rows(region, cells))

    # The re-measured configuration-2 cells against the cache cells of the
    # first eastus session, a ratio across two sessions one day apart
    for image in sorted({image for _, image in loaded["eastus-standard"]}):
        if ("2", image) not in loaded["eastus-standard"] or ("4", image) not in loaded["eastus"]:
            continue
        numerator = repetition_groups(pod_groups(loaded["eastus-standard"][("2", image)])["cold"])
        denominator = repetition_groups(pod_groups(loaded["eastus"][("4", image)])["cold"])
        estimate, low, high = ratio_interval(numerator, denominator)
        ratios.append({"region": "eastus-standard-over-eastus", "image": image,
                       "numerator_config": "2", "denominator_config": "4",
                       "cold_ratio": round(estimate, 3), "ci_low": round(low, 3), "ci_high": round(high, 3)})

    pd.DataFrame(cell_rows).to_csv(arguments.out, index=False)
    pd.DataFrame(ratios).to_csv(arguments.ratios, index=False)
    print(f"wrote {len(cell_rows)} cells to {arguments.out} and {len(ratios)} ratios to {arguments.ratios}")


if __name__ == "__main__":
    main()
