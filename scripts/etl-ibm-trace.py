#!/usr/bin/env python3
"""Convert the IBM Cloud Code Engine traces into the canonical replay format.

Reads one weekly pickle from the public EuroSys '26 dataset, selects a time
window and an app subset, and writes a CSV with columns at_ms,app,dur_ms
sorted by arrival time. The app identity combines namespace and app hashes
because app hashes alone may repeat across namespaces.
"""

import argparse
import json
import sys
from pathlib import Path

import numpy as np
import pandas as pd


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--week-file", required=True, help="path to one week_*.pickle")
    parser.add_argument("--days", type=float, default=7.0, help="window length in days from week start")
    parser.add_argument("--top", type=int, default=200, help="keep this many most-invoked apps")
    parser.add_argument("--all", action="store_true", help="keep every app in the week, ignoring --top")
    parser.add_argument("--tail-sample", type=int, default=0, help="also sample this many long-tail apps")
    parser.add_argument("--seed", type=int, default=1, help="seed for the tail sample")
    parser.add_argument("--busiest-day", action="store_true", help="pick the single highest-traffic day instead of the window start")
    parser.add_argument("--apps", help="JSON file with a fixed app list, overrides top/tail selection")
    parser.add_argument("--save-apps", help="write the selected app list to this JSON file")
    parser.add_argument("--out", required=True, help="output canonical CSV path")
    return parser.parse_args()


def load_week(path):
    # one row per app per week, with per-invocation lists inside each row
    week = pd.read_pickle(path)
    week["app"] = week["NamespaceHash"].astype(str) + "/" + week["AppHash"].astype(str)
    return week


def explode_invocations(week, app_map):
    # unpack the per-row invocation lists into flat arrays; plain numpy
    # concatenation keeps memory manageable at ~200M invocations per week
    rows = week[week["app"].isin(app_map)]
    if rows.empty:
        sys.exit("no invocations matched the requested apps")

    names = sorted(set(app_map.values()))
    code_of = {name: i for i, name in enumerate(names)}

    times, durs, codes = [], [], []
    for _, row in rows.iterrows():
        at = np.asarray(row["InvocationTimes"], dtype=np.float64)
        dur = np.nan_to_num(np.asarray(row["AppExecTimes"], dtype=np.float64))
        times.append(at)
        durs.append(dur)
        codes.append(np.full(len(at), code_of[app_map[row["app"]]], dtype=np.int32))

    return pd.DataFrame({
        "at_s": np.concatenate(times),
        "dur_ms": np.concatenate(durs).round().astype(np.int64),
        "app": pd.Categorical.from_codes(np.concatenate(codes), categories=names),
    })


def select_apps(week, args):
    # a saved app map keeps the same apps and short names across week files
    if args.apps:
        return json.loads(Path(args.apps).read_text())

    by_volume = week.groupby("app")["NumEvents"].sum().sort_values(ascending=False)
    if args.all:
        selected = list(by_volume.index)
    else:
        selected = list(by_volume.head(args.top).index)

    if args.tail_sample > 0:
        tail = by_volume.iloc[args.top:]
        rng = np.random.default_rng(args.seed)
        take = min(args.tail_sample, len(tail))
        selected += list(rng.choice(tail.index, size=take, replace=False))

    # short stable ids keep the canonical CSV and the simulator light; the
    # mapping back to the original hashes travels in the save-apps file
    return {app: f"a{i:04d}" for i, app in enumerate(selected)}


def pick_window(invs, days, busiest_day):
    start = invs["at_s"].min()

    if busiest_day:
        # rank whole days by invocation count and start the window at the winner
        day_index = ((invs["at_s"] - start) // 86400).astype(int)
        busiest = day_index.value_counts().idxmax()
        window_start = start + busiest * 86400
    else:
        window_start = start

    window_end = window_start + days * 86400
    return invs[(invs["at_s"] >= window_start) & (invs["at_s"] < window_end)], window_start


def summarize(invs):
    iat = invs["at_ms"].diff().dropna()
    sub_second = (iat < 1000).mean() * 100
    print(f"apps: {invs['app'].nunique()}")
    print(f"invocations: {len(invs)}")
    span_h = (invs["at_ms"].max() - invs["at_ms"].min()) / 3.6e6
    print(f"window span: {span_h:.1f} h")
    print(f"inter-arrival p50: {iat.median():.1f} ms, p99: {iat.quantile(0.99):.1f} ms")
    print(f"sub-second inter-arrivals: {sub_second:.1f}% (paper reports 94.5%)")
    print(f"duration p50: {invs['dur_ms'].median():.0f} ms, p99: {invs['dur_ms'].quantile(0.99):.0f} ms")


def main():
    args = parse_args()

    week = load_week(args.week_file)
    app_map = select_apps(week, args)

    if args.save_apps:
        apps_path = Path(args.save_apps)
        apps_path.parent.mkdir(parents=True, exist_ok=True)
        apps_path.write_text(json.dumps(app_map, indent=1))

    invs = explode_invocations(week, app_map)
    invs, window_start = pick_window(invs, args.days, args.busiest_day)
    if invs.empty:
        sys.exit("selected window contains no invocations")

    invs = invs.copy()
    invs["at_ms"] = ((invs["at_s"] - window_start) * 1000).round().astype(np.int64)
    invs.loc[invs["dur_ms"] < 0, "dur_ms"] = 0
    invs = invs.sort_values(["at_ms", "app"], kind="mergesort").reset_index(drop=True)

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    invs[["at_ms", "app", "dur_ms"]].to_csv(out, index=False)
    print(f"wrote {out}")

    summarize(invs)


if __name__ == "__main__":
    main()
