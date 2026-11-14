#!/usr/bin/env python3
"""Compares a live replay of the one-hour trace extract with the simulator's predictions"""
import argparse
import csv
import json
import lzma
import math
import re
import statistics
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
PREDICTIONS = ROOT / "analysis/results/simulation"

WRITTEN = re.compile(r"http\.response\.written=(\d+)")
METHOD = re.compile(r"http\.request\.method=(\w+)")
BLOB = re.compile(r"http\.request\.uri=\"?/v2/([^/]+)/blobs/")

TRIGGER_BEFORE_S = 5.0
TRIGGER_AFTER_S = 1.0


def read_model_rows(name, model):
    with (PREDICTIONS / name).open() as handle:
        return [row for row in csv.DictReader(handle) if row["model"] == model]


def as_milliseconds(cell):
    if not cell:
        return None
    return int(cell)


def load_predictions(model):
    cold_starts = {}
    for row in read_model_rows("live-hour-predicted-cold-starts.csv", model):
        cold_starts[row["app"]] = int(row["cold_starts"])

    latencies = {}
    for row in read_model_rows("live-hour-predicted-latency.csv", model):
        latencies[row["image"]] = {
            "cached_first_ms": as_milliseconds(row["cached_first_ms"]),
            "uncached_first_ms": as_milliseconds(row["uncached_first_ms"]),
            "repeat_ms": as_milliseconds(row["repeat_ms"]),
        }
    return cold_starts, latencies


def read_pods(path):
    pods = {}
    with path.open() as handle:
        for line in handle:
            stamp, _, rest = line.strip().partition(",")
            fields = rest.split()
            if len(fields) < 4:
                continue
            name, node, _phase, created = fields[:4]
            if name in pods:
                if node != "<none>" and pods[name]["node"] == "<none>":
                    pods[name]["node"] = node
                continue
            created_at = int(stamp)
            if created != "<none>":
                created_at = datetime.strptime(created, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()
            pods[name] = {"app": name.split("-", 1)[0], "node": node, "created": created_at}
    return sorted(pods.values(), key=lambda pod: pod["created"])


def read_requests(path):
    with path.open() as handle:
        rows = list(csv.DictReader(handle))
    for row in rows:
        row["sent"] = int(row["sent_at_unix_ms"]) / 1000
        row["latency_ms"] = int(row["latency_ms"])
        row["status"] = int(row["status"])
    return rows


def open_registry_log(path):
    if path.exists():
        return path.open(errors="replace")

    return lzma.open(path.with_name(path.name + ".xz"), "rt", errors="replace")


def registry_totals(path):
    requests = 0
    written = 0
    with open_registry_log(path) as handle:
        for line in handle:
            if not BLOB.search(line):
                continue
            method = METHOD.search(line)
            if method and method.group(1) != "GET":
                continue
            match = WRITTEN.search(line)
            requests += 1
            written += int(match.group(1)) if match else 0
    return requests, written


def cold_start_latencies(pods, requests, images):
    by_app = {}
    for request in requests:
        by_app.setdefault(request["app"], []).append(request)

    seen_in_cluster = set()
    seen_on_node = set()
    samples = []
    for pod in pods:
        image = images[pod["app"]]
        if image not in seen_in_cluster:
            kind = "seed"
        elif (image, pod["node"]) not in seen_on_node:
            kind = "first on node"
        else:
            kind = "repeat on node"
        seen_in_cluster.add(image)
        seen_on_node.add((image, pod["node"]))

        window = [
            request["latency_ms"]
            for request in by_app.get(pod["app"], [])
            if pod["created"] - TRIGGER_BEFORE_S <= request["sent"] <= pod["created"] + TRIGGER_AFTER_S
        ]
        if window:
            samples.append({"image": image, "kind": kind, "latency_ms": max(window)})
    return samples


def quantile(values, fraction):
    ordered = sorted(values)
    index = max(0, math.ceil(fraction * len(ordered)) - 1)
    return ordered[min(index, len(ordered) - 1)]


def summarize_run(run_dir):
    run_text = (run_dir / "run.txt").read_text()
    config = re.search(r"config=(\w+)", run_text).group(1)
    predicted_cold, predicted_latency = load_predictions("plain" if config == "2s" else "proxy")
    apps = json.loads((run_dir / "apps.json").read_text())["apps"]
    images = {app: meta["image"] for app, meta in apps.items()}

    pods = read_pods(run_dir / "pods.csv")
    requests = read_requests(run_dir / "requests.csv")
    counters = json.loads((run_dir / "cache-counters.json").read_text())
    registry_requests, registry_bytes = registry_totals(run_dir / "registry-requests.log")

    print(f"\n## {run_dir.name} (configuration {config})")
    failed = sum(1 for request in requests if request["status"] == 0 or request["status"] >= 500)
    print(f"requests {len(requests)}, failed {failed}, pods {len(pods)}")

    print("\n| app | image | predicted cold starts | live cold starts | miss |")
    print("|---|---|---|---|---|")
    live_by_app = {}
    for pod in pods:
        live_by_app[pod["app"]] = live_by_app.get(pod["app"], 0) + 1
    for app in apps:
        predicted = predicted_cold.get(app, 0)
        live = live_by_app.get(app, 0)
        print(f"| {app} | {images[app]} | {predicted} | {live} | {live - predicted:+d} |")
    total_predicted = sum(predicted_cold.values())
    print(f"| total | | {total_predicted} | {len(pods)} | {len(pods) - total_predicted:+d} |")

    hits = counters["hits_after"] - counters["hits_before"]
    misses = counters["misses_after"] - counters["misses_before"]
    put_bytes = counters["put_bytes_after"] - counters["put_bytes_before"]
    print(f"\nregistry blob GETs {registry_requests}, registry bytes {registry_bytes / 1e6:.1f} MB")
    if hits + misses:
        print(f"cache hit fraction by requests {hits / (hits + misses):.3f}")
    print(f"cache hits {hits}, misses {misses}, inserted bytes {put_bytes / 1e6:.1f} MB, "
          f"stored after {counters['stored_bytes_after'] / 1e6:.1f} MB")

    samples = cold_start_latencies(pods, requests, images)
    latencies = [request["latency_ms"] for request in requests]
    print("\nresponse time p50 {} ms, p95 {} ms, p99 {} ms, p99.9 {} ms, max {} ms".format(
        *(quantile(latencies, f) for f in (0.5, 0.95, 0.99, 0.999)), max(latencies)))
    for kind in ("seed", "first on node", "repeat on node"):
        values = [sample["latency_ms"] for sample in samples if sample["kind"] == kind]
        if values:
            print(f"cold start {kind}: n {len(values)}, mean {statistics.mean(values):,.0f} ms, "
                  f"median {statistics.median(values):,.0f} ms")

    first_key = "cached_first_ms" if config == "4" else "uncached_first_ms"
    kind_keys = {"seed": "uncached_first_ms", "first on node": first_key, "repeat on node": "repeat_ms"}
    for kind, key in kind_keys.items():
        predicted = [predicted_latency[s["image"]][key] for s in samples
                     if s["kind"] == kind and s["image"] in predicted_latency]
        if predicted and None not in predicted:
            print(f"predicted {kind} over the same images: mean {statistics.mean(predicted):,.0f} ms")

    print("\n| image | kind | predicted (ms) | live mean (ms) | live n |")
    print("|---|---|---|---|---|")
    first_key = "cached_first_ms" if config == "4" else "uncached_first_ms"
    for image, predicted in predicted_latency.items():
        kinds = (("uncached_first_ms", "seed"), (first_key, "first on node"), ("repeat_ms", "repeat on node"))
        for key, kind in kinds:
            values = [s["latency_ms"] for s in samples if s["image"] == image and s["kind"] == kind]
            mean = f"{statistics.mean(values):,.0f}" if values else "-"
            expected = "-" if predicted[key] is None else f"{predicted[key]:,}"
            print(f"| {image} | {kind} | {expected} | {mean} | {len(values)} |")
    return {"config": config, "pods": len(pods), "registry_bytes": registry_bytes, "hits": hits, "misses": misses}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("runs", nargs="+", type=Path, help="live-hour-config<N>-<timestamp> result directories")
    args = parser.parse_args()

    for run_dir in args.runs:
        summarize_run(run_dir)


if __name__ == "__main__":
    main()
