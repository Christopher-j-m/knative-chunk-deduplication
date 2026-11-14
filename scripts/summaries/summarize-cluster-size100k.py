#!/usr/bin/env python3
"""Summarizes the cluster-size experiment with the 100,000-packet emulator queue and finds where the cache breaks even"""
import argparse
import csv
import importlib.util
import re
import sys
from datetime import datetime, timedelta
from pathlib import Path
from result_paths import summary_path, find_benchmarks, BENCHMARKS

ROOT = Path(__file__).resolve().parent.parent.parent
SCRIPTS = Path(__file__).resolve().parent

sys.path.insert(0, str(SCRIPTS))
from cell_bootstrap import difference_interval, mean_interval, pooled_mean, ratio_interval  # noqa: E402

_traffic_spec = importlib.util.spec_from_file_location("summarize_registry_traffic", SCRIPTS / "summarize-registry-traffic.py")
traffic = importlib.util.module_from_spec(_traffic_spec)
_traffic_spec.loader.exec_module(traffic)

CONFIG_ORDER = ["4", "2s", "2s-prefetchall", "1", "2"]


def cell_pattern(prefix):
    return re.compile(
        rf"^{re.escape(prefix)}-config(?P<config>[0-9s]+)-(?P<image>.+?)-n(?P<nodes>\d+)-"
        r"rtt(?P<rtt>\d+)ms-rate(?P<rate>[^-]+)-\d{8}T\d{6}\.csv$"
    )


def find_cells(prefix):
    cells = {}
    for tag, label_suffix in ((prefix, ""), (f"{prefix}-prefetch", "-prefetchall")):
        pattern = cell_pattern(tag)
        for path in find_benchmarks(f"{tag}-config*.csv"):
            match = pattern.match(path.name)
            if match:
                cells[(match["image"], match["config"] + label_suffix, int(match["nodes"]))] = path
    return cells


def read_rows(path):
    with path.open() as handle:
        return list(csv.DictReader(handle))


def cold_groups(rows):
    groups = {}
    for row in rows:
        if row["phase"] == "scaleout" and row["warm"] == "no":
            groups.setdefault(row["rep"], []).append(float(row["ready_seconds"]))
    return [groups[rep] for rep in sorted(groups, key=int)]


def startup_sums(rows):
    sums = {}
    for row in rows:
        if row["phase"] == "seed" or (row["phase"] == "scaleout" and row["warm"] == "no"):
            sums[row["rep"]] = sums.get(row["rep"], 0.0) + float(row["ready_seconds"])
    return [sums[rep] for rep in sorted(sums, key=int)]


def parse_stamp(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def registry_bytes(rows, requests, image, slack_seconds=2.0):
    seed_bytes, scale_bytes, scale_fetches, seed_fetches = [], [], [], []
    for rep in sorted({row["rep"] for row in rows}, key=int):
        seed = next(row for row in rows if row["rep"] == rep and row["phase"] == "seed")
        scale = [row for row in rows if row["rep"] == rep and row["phase"] == "scaleout"]
        seed_start = parse_stamp(seed["phase_started_at"])
        scale_start = parse_stamp(scale[0]["phase_started_at"])
        scale_end = scale_start + timedelta(seconds=max(float(row["ready_seconds"]) for row in scale) + slack_seconds)
        fetches, written = traffic.count_between(requests, image, seed_start, scale_start)
        seed_fetches.append(fetches)
        seed_bytes.append(written)
        fetches, written = traffic.count_between(requests, image, scale_start, scale_end)
        scale_fetches.append(fetches)
        scale_bytes.append(written)

    def mean(values):
        return round(sum(values) / len(values))

    return {"seed_registry_bytes": mean(seed_bytes), "scaleout_registry_bytes": mean(scale_bytes),
            "seed_registry_fetches": mean(seed_fetches), "scaleout_registry_fetches": mean(scale_fetches)}


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", default="cluster-size100k")
    parser.add_argument("--registry-log", type=Path)
    parser.add_argument("--output")
    args = parser.parse_args()
    output = args.output or str(summary_path(f"{args.prefix}-summary.csv"))

    cells = find_cells(args.prefix)
    if not cells:
        sys.exit(f"no {args.prefix} cells in {BENCHMARKS}")
    rows_by_cell = {key: read_rows(path) for key, path in cells.items()}
    requests = traffic.read_requests(args.registry_log) if args.registry_log else None

    summary = []
    for (image, config, nodes) in sorted(cells, key=lambda key: (key[0], key[2], CONFIG_ORDER.index(key[1]))):
        rows = rows_by_cell[(image, config, nodes)]
        cold = cold_groups(rows)
        sums = startup_sums(rows)
        low, high = mean_interval(cold)
        sum_low, sum_high = mean_interval(sums)
        row = {
            "image": image, "config": config, "nodes": nodes, "file": cells[(image, config, nodes)].name,
            "reps": len(sums), "cold_pods": sum(len(group) for group in cold),
            "cold_mean_s": round(pooled_mean(cold), 3), "cold_ci_low_s": round(low, 3), "cold_ci_high_s": round(high, 3),
            "cold_max_s": round(max(max(group) for group in cold), 3),
            "seed_mean_s": round(sum(float(r["ready_seconds"]) for r in rows if r["phase"] == "seed") / len(sums), 3),
            "startup_sum_mean_s": round(pooled_mean(sums), 3),
            "startup_sum_ci_low_s": round(sum_low, 3), "startup_sum_ci_high_s": round(sum_high, 3),
            "netem_dropped": sum(int(r.get("netem_dropped") or 0) for r in rows if r["phase"] == "seed")
            + sum(int(r.get("netem_dropped") or 0) for r in {r["rep"]: r for r in rows if r["phase"] == "scaleout"}.values()),
        }

        reference = rows_by_cell.get((image, "4", nodes))
        if reference is not None and config != "4":
            ratio, ratio_low, ratio_high = ratio_interval(cold, cold_groups(reference))
            saving, saving_low, saving_high = difference_interval(sums, startup_sums(reference))
            row.update({"cold_over_cache": round(ratio, 3), "cold_over_cache_ci_low": round(ratio_low, 3),
                        "cold_over_cache_ci_high": round(ratio_high, 3),
                        "sum_saved_by_cache_s": round(saving, 3), "sum_saved_ci_low_s": round(saving_low, 3),
                        "sum_saved_ci_high_s": round(saving_high, 3)})
        if requests is not None:
            row.update(registry_bytes(rows, requests, image))
        summary.append(row)

    fields = []
    for row in summary:
        fields += [key for key in row if key not in fields]
    with open(output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=fields)
        writer.writeheader()
        writer.writerows(summary)

    for row in summary:
        extra = ""
        if "cold_over_cache" in row:
            extra = (f"  x{row['cold_over_cache']:.2f}  cache saves {row['sum_saved_by_cache_s']:+.1f} s "
                     f"({row['sum_saved_ci_low_s']:+.1f} to {row['sum_saved_ci_high_s']:+.1f})")
        print(f"{row['image']:16} n={row['nodes']} {row['config']:15} cold {row['cold_mean_s']:6.2f} "
              f"sum {row['startup_sum_mean_s']:6.1f}{extra}")


if __name__ == "__main__":
    main()
