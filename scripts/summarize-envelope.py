#!/usr/bin/env python3
"""Collect every envelope cell into one table with a cold start decomposed.

Reads the round-trip sweep, the bandwidth sweep, the uncontended sweep, eager OCI
pull measurements, and re-measured anchor cells. Splits each cold start into the
application's own initialization, the fetch the cache still performs, the registry
transfer and the residue left by two pods competing for one host.

The contended column separates two scale-out protocols: True for cells that start
two cold pods per repetition (netem, rate, anchor, eager families), False for solo
cells with one cold pod per repetition.
Run with: uv run --with pandas scripts/summarize-envelope.py
"""

import glob
import os
import re

import pandas as pd

BENCH = os.path.join(os.path.dirname(__file__), "../analysis/results/benchmarks")
OUT = os.path.join(os.path.dirname(__file__),
                   "../analysis/results/summaries/envelope-summary.csv")

PATTERNS = {
    "netem": re.compile(r"netem-sweep-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                        r"-rtt(?P<rtt>\d+)ms-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
    "rate": re.compile(r"rate-sweep-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                       r"-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
    "solo": re.compile(r"solo-sweep-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                       r"-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
    # only present under Branch A, where the anchors were re-measured with a
    # one-second readiness probe
    "anchor": re.compile(r"envelope-anchor-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                         r"-rtt(?P<rtt>\d+)ms-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
    # eager OCI pull measurements against added round-trip time
    "eager": re.compile(r"envelope-eager-config(?P<config>\d)-(?P<image>fn-[a-z-]+)"
                        r"-rtt(?P<rtt>\d+)ms-rate(?P<rate>\d+)mbit-\d{8}T\d{6}\.csv$"),
}

# A cold start splits at roughly nine seconds into a fast and a slow mode on
# this rig; keeping both is what lets the figures show a band instead of a
# median that silently sits on one of them
MODE_SPLIT_S = 9.0


def modes(values):
    """Split cold starts into fast and slow modes by nine-second threshold above minimum.

    When has_fast_mode is True: cold_fast_s is the median of rows below the threshold,
    cold_slow_s is the median of rows above it; the cell genuinely contained both populations.
    When has_fast_mode is False: all rows landed on one side; cold_fast_s is the median
    of whatever bucket contained them, and cold_fast_s reflects quantization, not a true fast mode.
    """
    if values.empty:
        return float("nan"), float("nan"), False
    fast = values[values <= values.min() + MODE_SPLIT_S]
    slow = values[values > values.min() + MODE_SPLIT_S]
    has_fast_mode = not slow.empty
    return (fast.median(),
            slow.median() if not slow.empty else fast.median(),
            has_fast_mode)


def compute_contention(frame):
    """Compute within-repetition contention as median spread across cold pods per rep.

    For each repetition that started multiple cold pods, the spread is
    slowest_cold - fastest_cold. Takes the median of spreads across reps.
    Returns NaN if no repetition had multiple cold pods.

    This measures what two pods competing for one host costs when they start
    under identical conditions, not a cross-cell comparison that mixes different
    probe periods.
    """
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


def cell_row(path, campaign, fields):
    frame = pd.read_csv(path)
    scaleout = frame[frame["phase"] == "scaleout"]
    cold = scaleout[scaleout["warm"] == "no"]["ready_seconds"]
    warm = scaleout[scaleout["warm"] == "yes"]["ready_seconds"]
    fast, slow, has_fast_mode = modes(cold)

    # Extract timestamp from filename to determine probe period
    basename = os.path.basename(path)
    timestamp_match = re.search(r"(\d{8}T\d{6})", basename)
    timestamp = timestamp_match.group(1) if timestamp_match else ""
    probe_period_s = 1 if timestamp >= "20260819T200000" else 10

    return {
        "campaign": campaign,
        "config": int(fields["config"]),
        "image": fields["image"],
        "rtt_ms": int(fields.get("rtt", 0)),
        "rate_mbit": int(fields["rate"]),
        "contended": campaign != "solo",
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
    """Split each cold start into the parts the cache can and cannot remove.

    The warm median is the application's own initialization, a lower bound
    because a warm pod also has a hot page cache. The difference between
    cold_fast_s and warm_median_s is what remains after warm initialization,
    split between fetch (config 4), transfer (config 1-2), and contention (measured
    within-cell spread of competing pods, not cross-cell pairing).

    Contention is computed as within-repetition spread: for each repetition with
    multiple cold pods, the spread is slowest - fastest, and contention_s is
    the median of those spreads. It is NaN for cells with one pod per repetition.
    This avoids comparing cells measured under different probe periods.
    """
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
    for campaign, pattern in PATTERNS.items():
        for path in sorted(glob.glob(os.path.join(BENCH, "*.csv"))):
            match = pattern.search(os.path.basename(path))
            if match:
                rows.append(cell_row(path, campaign, match.groupdict()))

    table = decompose(pd.DataFrame(rows))
    table = table.sort_values(["image", "config", "rtt_ms", "rate_mbit"])
    table.to_csv(OUT, index=False)
    print(f"{len(table)} cells written to {OUT}")
    print(table.head(12).to_string(index=False))


if __name__ == "__main__":
    main()
