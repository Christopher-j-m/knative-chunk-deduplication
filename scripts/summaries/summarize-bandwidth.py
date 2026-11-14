#!/usr/bin/env python3
"""Collects every distance and bandwidth cell into one table and splits each cold start into its parts"""
import glob
import os
import re

import pandas as pd
from result_paths import summary_path, find_benchmarks

OUT = summary_path("bandwidth-summary.csv")

PATTERNS = {
    "distance": re.compile(r"distance-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                        r"-rtt(?P<rtt>\d+)ms-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
    "bandwidth-two-pods": re.compile(r"bandwidth-two-pods-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                       r"-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
    "bandwidth-one-pod": re.compile(r"bandwidth-one-pod-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                       r"-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
    "distance-repeat": re.compile(r"distance-repeat-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                         r"-rtt(?P<rtt>\d+)ms-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
    "distance-eager": re.compile(r"distance-eager-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                        r"-rtt(?P<rtt>\d+)ms-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
}

MODE_SPLIT_S = 9.0


def modes(values):
    if values.empty:
        return float("nan"), float("nan"), False
    fast = values[values <= values.min() + MODE_SPLIT_S]
    slow = values[values > values.min() + MODE_SPLIT_S]
    has_fast_mode = not slow.empty
    return (fast.median(),
            slow.median() if not slow.empty else fast.median(),
            has_fast_mode)


def compute_contention(frame):
    scaleout = frame[frame["phase"] == "scaleout"]
    spreads = []

    for rep in scaleout["rep"].unique():
        rep_cold = scaleout[(scaleout["rep"] == rep) & (scaleout["warm"] == "no")]["ready_seconds"]
        if len(rep_cold) > 1:
            spread = rep_cold.max() - rep_cold.min()
            spreads.append(spread)

    if not spreads:
        return float("nan")
    return pd.Series(spreads).median()


def cell_row(path, experiment, fields):
    frame = pd.read_csv(path)
    scaleout = frame[frame["phase"] == "scaleout"]
    cold = scaleout[scaleout["warm"] == "no"]["ready_seconds"]
    warm = scaleout[scaleout["warm"] == "yes"]["ready_seconds"]
    fast, slow, has_fast_mode = modes(cold)

    basename = os.path.basename(path)
    timestamp_match = re.search(r"(\d{8}T\d{6})", basename)
    timestamp = timestamp_match.group(1) if timestamp_match else ""
    probe_period_s = 1 if timestamp >= "20260819T200000" else 10

    return {
        "experiment": experiment,
        "config": int(fields["config"]),
        "image": fields["image"],
        "rtt_ms": int(fields.get("rtt", 0)),
        "rate_mbit": int(fields["rate"]),
        "contended": experiment != "bandwidth-one-pod",
        "cold_median_s": round(cold.median(), 3),
        "cold_fast_s": round(fast, 3),
        "cold_slow_s": round(slow, 3),
        "warm_median_s": round(warm.median(), 3) if not warm.empty else float("nan"),
        "reps": len(cold),
        "probe_period_s": probe_period_s,
        "has_fast_mode": has_fast_mode,
        "contention_raw": compute_contention(frame),
    }


def decompose(table):
    app, fetch, contention, transfer = [], [], [], []
    for _, row in table.iterrows():
        warm = row["warm_median_s"]
        cold = row["cold_fast_s"]

        app.append(round(warm, 3))
        residual = round(cold - warm, 3) if cold == cold else float("nan")
        if row["config"] == 4:
            fetch.append(residual)
            transfer.append(0.0)
        else:
            fetch.append(0.0)
            transfer.append(residual)
        contention.append(round(row["contention_raw"], 3) if row["contention_raw"] == row["contention_raw"] else float("nan"))

    table["app_init_s"] = app
    table["fetch_s"] = fetch
    table["contention_s"] = contention
    table["transfer_s"] = transfer
    return table.drop(columns=["contention_raw"])


def main():
    rows = []
    for experiment, pattern in PATTERNS.items():
        for path in [str(path) for path in find_benchmarks("*.csv")]:
            match = pattern.search(os.path.basename(path))
            if match:
                rows.append(cell_row(path, experiment, match.groupdict()))

    table = decompose(pd.DataFrame(rows))
    table = table.sort_values(["image", "config", "rtt_ms", "rate_mbit"])
    table.to_csv(OUT, index=False)
    print(f"{len(table)} cells written to {OUT}")
    print(table.head(12).to_string(index=False))


if __name__ == "__main__":
    main()
