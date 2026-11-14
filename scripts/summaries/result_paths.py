"""Gives the topic folder a summary or benchmark file belongs to, so scripts can find and place results by name"""
import re
import sys
from pathlib import Path

RESULTS = Path(__file__).resolve().parent.parent.parent / "analysis" / "results"
SUMMARIES = RESULTS / "summaries"
BENCHMARKS = RESULTS / "benchmarks"

FOLDERS = [
    ("cold-start", r"ladder-|scaleout"),
    ("deduplication", r"registry-traffic|plain-registry-traffic|rig-cross-image|aks-cross-image"),
    ("cache-operation", r"serving-capacity|degradation|aks-cache-capacity"),
    ("plain-nydus", r"plain-|proxy-prefetch-check"),
    ("distance-bandwidth", r"distance|bandwidth-|readiness-timing|emulator-queue-check"),
    ("cluster-size", r"cluster-size|second-replica"),
    ("real-registry", r"aks"),
    ("production-week", r"live-hour"),
]

RUNS = {
    "plain-nydus": [
        "plain-defaults-distance100k",
        "plain-prefetch",
        "plain-prefetch-adjacent-repeat100k",
        "plain-prefetch-adjacent100k",
        "plain-prefetch-uncompressed100k",
        "plain-prefetch-zstd100k",
        "plain-prefetch100k",
        "proxy-prefetch-check",
    ],
    "distance-bandwidth": [
        "bandwidth-at-80ms",
        "bandwidth-one-pod",
        "bandwidth-two-pods",
        "distance",
        "distance-eager",
        "distance-repeat",
        "distance100k",
        "emulator-queue-check",
        "readiness-timing",
    ],
    "cluster-size": [
        "cluster-size",
        "cluster-size100k",
        "cluster-size100k-prefetch",
        "second-replica-one-pod",
        "second-replica-two-pods",
    ],
    "real-registry": [
        "aks-eastus",
        "aks-eastus-standard",
        "aks-plain-nydus",
        "aks-plain-nydus-back-to-back",
        "aks-plain-nydus-defaults",
        "aks-plain-nydus-eastus",
        "aks-plain-nydus-eastus-defaults",
        "aks-plain-nydus-eastus-repeat",
        "aks-plain-nydus-eastus-zstd",
        "aks-plain-nydus-uncompressed-prefetch",
        "aks-plain-nydus-zstd",
        "aks-same-region",
        "aks-touched-chunks",
    ],
}

SHARED_FILES = {
    "bandwidth-two-pods-network-check.csv",
    "cluster-size-network-check.csv",
    "distance-network-check.csv",
}


def topic(name):
    for folder, pattern in FOLDERS:
        if re.match(pattern, name):
            return folder
    return None


def run_name(folder, name):
    if name in SHARED_FILES:
        return None

    matches = []
    for run in RUNS.get(folder, []):
        if re.match(re.escape(run) + r"($|[-.])", name):
            matches.append(run)

    if not matches:
        return None
    return max(matches, key=len)


def summary_path(name):
    folder = topic(name)
    if folder is None:
        return SUMMARIES / name
    return SUMMARIES / folder / name


def benchmark_folder(name):
    folder = topic(name)
    if folder is None:
        return BENCHMARKS

    run = run_name(folder, name)
    if run is None:
        return BENCHMARKS / folder
    return BENCHMARKS / folder / run


def benchmark_path(name):
    return benchmark_folder(name) / name


def find_benchmarks(pattern):
    places = [pattern, f"*/{pattern}"]
    for folder in sorted(RUNS):
        places.append(f"{folder}/*/{pattern}")

    matches = []
    for place in places:
        matches += [path for path in BENCHMARKS.glob(place) if path.is_file()]
    return sorted(matches, key=lambda path: path.name)


if __name__ == "__main__":
    print(benchmark_folder(sys.argv[1]))
