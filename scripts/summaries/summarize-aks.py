#!/usr/bin/env python3
"""Summarizes the AKS runs per cell with bootstrap intervals and writes the cold start ratios between configurations"""
import argparse
import glob
import json
import os
import re

import pandas as pd

from cell_bootstrap import mean_interval, ratio_interval, repetition_groups
from result_paths import summary_path, find_benchmarks, benchmark_path


REGIONS = {
    "westeurope": "aks-same-region-scaleout",
    "eastus": "aks-eastus-scaleout",
    "eastus-standard": "aks-eastus-standard-scaleout",
}
FACTS = {
    "westeurope": "aks-same-region-image-facts.json",
    "eastus": "aks-eastus-image-facts.json",
    "eastus-standard": "aks-eastus-standard-image-facts.json",
}

COLD_RATIOS = [("2", "4"), ("1", "4"), ("3", "4"), ("2", "3"), ("1", "2"), ("1", "3"),
               ("2s", "4"), ("2", "2s")]

RUN_BLOCKS = [
    ("westeurope", "", "westeurope"),
    ("eastus", "-eastus", "eastus"),
    ("westeurope-zstd", "-zstd", "westeurope"),
    ("eastus-zstd", "-eastus-zstd", "eastus"),
    ("westeurope-zstd-prefetch", "-defaults", "westeurope"),
    ("eastus-zstd-prefetch", "-eastus-defaults", "eastus"),
    ("westeurope-uncompressed-prefetch", "-uncompressed-prefetch", "westeurope"),
    ("eastus-repeat", "-eastus-repeat", "eastus-repeat"),
    ("westeurope-back-to-back", "-back-to-back", "westeurope-back-to-back"),
]

JOINED_BLOCKS = {
    "westeurope-back-to-back": ["-back-to-back-cache1", "-back-to-back-plain", "-back-to-back-cache2"],
}

PLAIN_COMPARISONS = [
    ("westeurope", "westeurope-zstd"),
    ("westeurope-zstd", "westeurope-zstd-prefetch"),
    ("westeurope", "westeurope-uncompressed-prefetch"),
    ("westeurope-uncompressed-prefetch", "westeurope-zstd-prefetch"),
    ("westeurope", "westeurope-zstd-prefetch"),
    ("eastus", "eastus-zstd"),
    ("eastus-zstd", "eastus-zstd-prefetch"),
    ("eastus", "eastus-zstd-prefetch"),
    ("westeurope-zstd-prefetch", "westeurope-back-to-back"),
]

REPEATED_CACHE_GROUPS = {"eastus": "eastus-repeat"}


def cell_pattern(prefix):
    return re.compile(
        rf"^{re.escape(prefix)}-config(?P<config>\ds?)-(?P<image>fn-[a-z-]+)-\d{{8}}T\d{{6}}\.csv$"
    )


def load_cells(prefix):
    pattern = cell_pattern(prefix)
    cells = {}
    for path in [str(path) for path in find_benchmarks(f"{prefix}-config*.csv")]:
        match = pattern.match(os.path.basename(path))
        if match:
            cells[(match["config"], match["image"])] = pd.read_csv(path)
    return cells


def load_joined_cells(prefixes):
    joined = {}
    for prefix in prefixes:
        for key, frame in load_cells(prefix).items():
            frame = frame.copy()
            if key in joined:
                frame["rep"] = frame["rep"] + joined[key]["rep"].max()
                frame = pd.concat([joined[key], frame], ignore_index=True)
            joined[key] = frame
    return joined


def transfer_bytes(region, facts_file=None):
    path = str(benchmark_path(facts_file or FACTS[region]))
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

    cold_and_seed = pd.concat([groups["cold"], groups["seed"]])
    if config == "1":
        row["cold_pull_mean_s"] = round(float(groups["cold"]["pull_seconds"].mean()), 3)
        row["seed_pull_mean_s"] = round(float(groups["seed"]["pull_seconds"].mean()), 3)
        row["transfer_bytes"] = eager_bytes.get(image, "")
        row["min_rx_bytes"] = int(cold_and_seed["node_rx_bytes"].min())
    row["seed_misses_mean"] = round(float(groups["seed"]["cache_misses_delta"].mean()), 1)
    if "registry_fetch_errors" in frame.columns:
        row["failed_fetches_total"] = int(pd.to_numeric(frame["registry_fetch_errors"], errors="coerce").fillna(0).sum())
    if "restart_count" in frame.columns:
        row["restarts_total"] = int(pd.to_numeric(frame["restart_count"], errors="coerce").fillna(0).sum())
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


def cold_ratio(numerator_frame, denominator_frame):
    numerator = repetition_groups(pod_groups(numerator_frame)["cold"])
    denominator = repetition_groups(pod_groups(denominator_frame)["cold"])
    estimate, low, high = ratio_interval(numerator, denominator)
    return round(estimate, 3), round(low, 3), round(high, 3)


def summarize_run(run, out, ratios_out):
    cell_rows, ratios = [], []
    loaded = {}
    for group, suffix, _ in RUN_BLOCKS:
        if group in JOINED_BLOCKS:
            loaded[group] = load_joined_cells(
                [f"{run}{part}-scaleout" for part in JOINED_BLOCKS[group]])
        else:
            loaded[group] = load_cells(f"{run}{suffix}-scaleout")

    for group, suffix, cache_group in RUN_BLOCKS:
        cells = loaded[group]
        eager_bytes = transfer_bytes(group, f"{run}{suffix}-image-facts.json")
        for (config, image), frame in sorted(cells.items()):
            row = cell_row(group, config, image, frame, eager_bytes)
            cache_cell = loaded[cache_group].get(("4", image))
            if config != "4" and cache_cell is not None:
                estimate, low, high = cold_ratio(frame, cache_cell)
                row["cold_over_config4"] = estimate
                row["cold_over_config4_ci_low"] = low
                row["cold_over_config4_ci_high"] = high
            repeated_group = REPEATED_CACHE_GROUPS.get(cache_group)
            repeated_cell = loaded.get(repeated_group, {}).get(("4", image)) if repeated_group else None
            if config != "4" and repeated_cell is not None:
                estimate, low, high = cold_ratio(frame, repeated_cell)
                row["cold_over_repeated_config4"] = estimate
                row["cold_over_repeated_config4_ci_low"] = low
                row["cold_over_repeated_config4_ci_high"] = high
            cell_rows.append(row)
        ratios.extend(ratio_rows(group, cells))

    for first, second in PLAIN_COMPARISONS:
        for (config, image), frame in sorted(loaded[first].items()):
            other = loaded[second].get((config, image))
            if config != "2s" or other is None:
                continue
            estimate, low, high = cold_ratio(frame, other)
            ratios.append({"region": f"{first}-over-{second}", "image": image,
                           "numerator_config": "2s", "denominator_config": "2s",
                           "cold_ratio": estimate, "ci_low": low, "ci_high": high})

    pd.DataFrame(cell_rows).to_csv(out, index=False)
    pd.DataFrame(ratios).to_csv(ratios_out, index=False)
    print(f"wrote {len(cell_rows)} cells to {out} and {len(ratios)} ratios to {ratios_out}")


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--run", help="prefix of a later run, read instead of the same-region and eastus runs")
    parser.add_argument("--out")
    parser.add_argument("--ratios")
    arguments = parser.parse_args()

    if arguments.run:
        summarize_run(
            arguments.run,
            arguments.out or summary_path(f"{arguments.run}-summary.csv"),
            arguments.ratios or summary_path(f"{arguments.run}-ratios.csv"),
        )
        return
    arguments.out = arguments.out or summary_path("aks-summary.csv")
    arguments.ratios = arguments.ratios or summary_path("aks-ratios.csv")

    cell_rows, ratios = [], []
    loaded = {}
    for region, prefix in REGIONS.items():
        cells = load_cells(prefix)
        loaded[region] = cells
        eager_bytes = transfer_bytes(region)
        for (config, image), frame in sorted(cells.items()):
            cell_rows.append(cell_row(region, config, image, frame, eager_bytes))
        ratios.extend(ratio_rows(region, cells))

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
