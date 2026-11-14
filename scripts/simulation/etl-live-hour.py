#!/usr/bin/env python3
"""Cuts a one-hour extract with 24 applications out of the trace week for the live replay"""
import argparse
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd

BOUNDS = [10, 100, 1000, 10000, 100000]
IMAGES = ["fn-go-hello", "fn-python-hello", "fn-java-springboot", "fn-node-api", "fn-python-ml", "fn-pytorch-cpu"]
HOUR_MS = 3_600_000
CHUNK_ROWS = 20_000_000


def class_of(count):
    for i, bound in enumerate(BOUNDS):
        if count <= bound:
            return i
    return len(BOUNDS)


def read_chunks(path):
    return pd.read_csv(path, dtype={"at_ms": np.int64, "app": "category", "dur_ms": np.int64}, chunksize=CHUNK_ROWS)


def weekly_counts_and_busiest_hour(path):
    counts = pd.Series(dtype=np.int64)
    per_hour = pd.Series(dtype=np.int64)
    for chunk in read_chunks(path):
        counts = counts.add(chunk.groupby("app", observed=True).size(), fill_value=0)
        per_hour = per_hour.add((chunk.at_ms // HOUR_MS).value_counts(), fill_value=0)
    return counts.astype(np.int64), int(per_hour.idxmax())


def calls_in_hour(path, hour):
    window = []
    for chunk in read_chunks(path):
        window.append(chunk[(chunk.at_ms // HOUR_MS) == hour])
    window = pd.concat(window, ignore_index=True)
    window["app"] = window.app.astype(str)
    return window


def choose_apps(window, counts, per_class, seed):
    rng = np.random.default_rng(seed)
    chosen = []
    for cls in range(len(BOUNDS) + 1):
        pool = sorted(a for a in window.app.unique() if class_of(counts[a]) == cls)
        take = min(per_class, len(pool))
        chosen += [(a, cls) for a in rng.choice(pool, size=take, replace=False)]
    return chosen


def thin_to_rate_cap(calls, cap_per_minute):
    kept = []
    dropped = {}
    for app, group in calls.groupby("app", sort=False):
        rate = len(group) / 60
        step = 1 if cap_per_minute <= 0 else max(1, math.ceil(rate / cap_per_minute))
        kept.append(group.iloc[::step])
        dropped[app] = len(group) - len(group.iloc[::step])
    return pd.concat(kept, ignore_index=True), dropped


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--trace", default="analysis/traces/ibm-7d-all.csv")
    parser.add_argument("--per-class", type=int, default=4)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--rate-cap", type=int, default=60, help="calls per app per minute; 0 disables thinning")
    parser.add_argument("--out", default="analysis/traces/live-hour.csv")
    parser.add_argument("--save-apps", default="analysis/traces/live-hour-apps.json")
    args = parser.parse_args()

    counts, hour = weekly_counts_and_busiest_hour(args.trace)
    window = calls_in_hour(args.trace, hour)
    chosen = choose_apps(window, counts, args.per_class, args.seed)

    apps = {}
    for i, (app, cls) in enumerate(chosen):
        apps[app] = {"class": cls, "image": IMAGES[i % len(IMAGES)], "weekly": int(counts[app])}

    calls = window[window.app.isin(apps)].copy()
    calls, dropped = thin_to_rate_cap(calls, args.rate_cap)
    calls["at_ms"] -= calls.at_ms.min()
    calls = calls.sort_values(["at_ms", "app"], kind="mergesort")[["at_ms", "app", "dur_ms"]]
    calls.to_csv(args.out, index=False)

    for app in apps:
        apps[app]["calls_in_hour"] = int((calls.app == app).sum())
        apps[app]["dropped_by_rate_cap"] = int(dropped.get(app, 0))
    manifest = {"hour_of_week": hour, "rate_cap_per_minute": args.rate_cap, "seed": args.seed, "apps": apps}
    Path(args.save_apps).write_text(json.dumps(manifest, indent=1) + "\n")

    print(f"hour {hour}: {len(apps)} apps, {len(calls)} invocations, {sum(dropped.values())} dropped by the rate cap")
    for app, meta in apps.items():
        print(f"  {app} class {meta['class']} weekly {meta['weekly']} -> {meta['image']} "
              f"calls in hour {meta['calls_in_hour']} (dropped {meta['dropped_by_rate_cap']})")


if __name__ == "__main__":
    main()
