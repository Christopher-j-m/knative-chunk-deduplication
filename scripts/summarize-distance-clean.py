#!/usr/bin/env python3
"""Summarizes the registry-distance sweep, one row per image, configuration and round trip.

Each cell holds five repetitions of a seed and a scale-out. A row reports the
cold mean, minimum and maximum over the cold scale-out pods, the warm and seed
means, and the cold mean's ratio to the cluster cache (configuration 4) at the
same image and round trip, with a bootstrap interval over repetitions. Three
check columns carry what the protocol demands of a cell: the packets netem
dropped, the cache traffic, and the hits per cold pod.

Usage: summarize-distance-clean.py [--prefix distance-clean] [--rate 600mbit]
       [--output analysis/results/summaries/distance-clean-summary.csv]
"""

import argparse
import re
import sys
from pathlib import Path

import pandas as pd

ROOT = Path(__file__).resolve().parent.parent
BENCHMARKS = ROOT / "analysis" / "results" / "benchmarks"
SUMMARIES = ROOT / "analysis" / "results" / "summaries"

sys.path.insert(0, str(Path(__file__).resolve().parent))
from cell_bootstrap import mean_interval, pooled_mean, ratio_interval, repetition_groups  # noqa: E402

CONFIG_ORDER = ["4", "2s", "1", "2"]


def cell_files(prefix, rate):
    """Finds every measurement file of the sweep and the cell it belongs to."""
    pattern = re.compile(
        rf"^{re.escape(prefix)}-config(?P<config>[0-9s]+)-(?P<image>.+)-rtt(?P<rtt>\d+)ms-rate{rate}-\d{{8}}T\d{{6}}\.csv$"
    )
    cells = {}
    for path in sorted(BENCHMARKS.glob(f"{prefix}-config*.csv")):
        match = pattern.match(path.name)
        if match:
            key = (match["image"], match["config"], int(match["rtt"]))
            # A cell measured twice keeps its newest file
            cells[key] = path
    return cells


def summarize_cell(frame):
    """Reduces one cell to its means, range and checks."""
    cold = frame[(frame.phase == "scaleout") & (frame.warm == "no")]
    warm = frame[(frame.phase == "scaleout") & (frame.warm == "yes")]
    seed = frame[frame.phase == "seed"]

    # The scale-out counters cover the whole phase, so one row per repetition
    # carries them; dividing by the cold pods of that repetition gives hits per pod
    scale_counters = frame[frame.phase == "scaleout"].groupby("rep").first()
    cold_pods = cold.groupby("rep").size()
    hits_per_cold_pod = (scale_counters.cache_hits_delta / cold_pods).mean()

    cold_low, cold_high = mean_interval(repetition_groups(cold))
    return {
        "reps": frame.rep.nunique(),
        "cold_pods": len(cold),
        "cold_mean_s": pooled_mean(repetition_groups(cold)),
        "cold_mean_low_s": cold_low,
        "cold_mean_high_s": cold_high,
        "cold_min_s": cold.ready_seconds.min(),
        "cold_max_s": cold.ready_seconds.max(),
        "cold_median_s": cold.ready_seconds.median(),
        "warm_mean_s": warm.ready_seconds.mean() if len(warm) else float("nan"),
        "seed_mean_s": seed.ready_seconds.mean(),
        "seed_min_s": seed.ready_seconds.min(),
        "seed_max_s": seed.ready_seconds.max(),
        "netem_dropped": int(frame.netem_dropped.sum()),
        "seed_cache_hits": int(seed.cache_hits_delta.sum()),
        "seed_cache_misses": int(seed.cache_misses_delta.sum()),
        "scaleout_cache_hits": int(scale_counters.cache_hits_delta.sum()),
        "scaleout_cache_misses": int(scale_counters.cache_misses_delta.sum()),
        "hits_per_cold_pod": hits_per_cold_pod,
        "registry_fetch_errors": int(frame.registry_fetch_errors.fillna(0).sum()),
        "restarts": int(frame.restart_count.fillna(0).sum()),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--prefix", default="distance-clean")
    parser.add_argument("--rate", default="600mbit")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    output = args.output or SUMMARIES / f"{args.prefix}-summary.csv"

    cells = cell_files(args.prefix, args.rate)
    if not cells:
        sys.exit(f"no {args.prefix} cells in {BENCHMARKS}")
    frames = {key: pd.read_csv(path) for key, path in cells.items()}

    rows = []
    for (image, config, rtt), frame in sorted(
        frames.items(), key=lambda item: (item[0][0], item[0][2], CONFIG_ORDER.index(item[0][1]))
    ):
        row = {"image": image, "config": config, "rtt_ms": rtt, "file": cells[(image, config, rtt)].name}
        row.update(summarize_cell(frame))

        # The ratio says how many times slower a configuration's cold start is
        # than the cache's at the same image and distance
        reference = frames.get((image, "4", rtt))
        if reference is not None:
            cold = frame[(frame.phase == "scaleout") & (frame.warm == "no")]
            reference_cold = reference[(reference.phase == "scaleout") & (reference.warm == "no")]
            ratio, low, high = ratio_interval(repetition_groups(cold), repetition_groups(reference_cold))
            row.update({"ratio_to_config4": ratio, "ratio_low": low, "ratio_high": high})
        rows.append(row)

    summary = pd.DataFrame(rows)
    output.parent.mkdir(parents=True, exist_ok=True)
    summary.to_csv(output, index=False, float_format="%.3f")
    print(f"wrote {len(summary)} rows to {output}")
    columns = ["image", "config", "rtt_ms", "cold_mean_s", "cold_min_s", "cold_max_s", "seed_mean_s",
               "ratio_to_config4", "netem_dropped", "scaleout_cache_hits", "scaleout_cache_misses"]
    print(summary[[c for c in columns if c in summary]].to_string(index=False, float_format="%.2f"))


if __name__ == "__main__":
    main()
