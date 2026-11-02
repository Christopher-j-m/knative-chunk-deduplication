#!/usr/bin/env python3
"""Count what the local rig's registry served to each node in each phase.

Reads the registry container's access log and the cells of one prefix, and
splits every blob response by repetition, phase and receiving node. A
repetition's seed phase runs from its seed start to its scale-out start, the
scale-out phase from there to half a minute after its slowest pod was ready. The reset's
own manifest reads are left out. For a configuration that shares data across
nodes, the cold scale-out nodes should receive almost nothing from the
registry, which is the evidence that the sharing path moved the data.
Usage: summarize-registry-phase-bytes.py --prefix ladder2 --config 3 [--out FILE]
"""

import argparse
import csv
import glob
import json
import os
import re
import subprocess
from datetime import datetime, timedelta

HERE = os.path.dirname(os.path.abspath(__file__))
BENCH = os.path.join(HERE, "../analysis/results/benchmarks")
SUMMARIES = os.path.join(HERE, "../analysis/results/summaries")

LINE = re.compile(r'^time="([^"]+)".*http\.request\.method=GET http\.request\.remoteaddr="([\d.]+):\d+" '
                  r'http\.request\.uri="(/v2/[^"]+/blobs/[^"]+)" http\.request\.useragent="([^"]*)".*'
                  r'http\.response\.status=(\d+) http\.response\.written=(\d+)')


def parse_time(stamp):
    # the registry logs nanoseconds, which datetime cannot hold
    stamp = stamp.rstrip("Z")
    if "." in stamp:
        head, fraction = stamp.split(".")
        stamp = f"{head}.{fraction[:6]}"
    return datetime.fromisoformat(stamp)


def node_addresses():
    nodes = subprocess.run([os.path.join(HERE, "cluster-nodes.sh")], capture_output=True, text=True,
                           check=True).stdout.split()
    addresses = {}
    for node in nodes:
        details = json.loads(subprocess.run(["docker", "inspect", node], capture_output=True, text=True,
                                            check=True).stdout)
        address = details[0]["NetworkSettings"]["Networks"]["k3d-nydus-cache"]["IPAddress"]
        addresses[address] = node
    return addresses


def registry_responses():
    log = subprocess.run(["docker", "logs", "registry.localhost"], capture_output=True, text=True).stderr
    for line in log.splitlines():
        match = LINE.match(line)
        if match and not match.group(4).startswith("Python-urllib"):
            yield parse_time(match.group(1)), match.group(2), int(match.group(6))


def phase_windows(cell_path):
    with open(cell_path, newline="") as handle:
        rows = list(csv.DictReader(handle))
    starts, slowest = {}, {}
    for row in rows:
        rep = int(row["rep"])
        starts.setdefault(rep, {})[row["phase"]] = parse_time(row["phase_started_at"])
        if row["phase"] == "scaleout":
            slowest[rep] = max(slowest.get(rep, 0.0), float(row["ready_seconds"]))
    windows = []
    for rep in sorted(starts):
        seed, scaleout = starts[rep]["seed"], starts[rep]["scaleout"]
        # the scale-out window closes half a minute after its slowest pod is
        # ready, so the next cell's warm-up never falls into it
        end = scaleout + timedelta(seconds=slowest[rep] + 30)
        windows.append((rep, "seed", seed, scaleout))
        windows.append((rep, "scaleout", scaleout, end))
    roles = {(int(row["rep"]), row["phase"], row["node"]): row["warm"] for row in rows}
    return windows, roles


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prefix", required=True)
    parser.add_argument("--config", required=True)
    parser.add_argument("--out")
    arguments = parser.parse_args()

    addresses = node_addresses()
    responses = list(registry_responses())
    rows = []
    pattern = os.path.join(BENCH, f"{arguments.prefix}-config{arguments.config}-fn-*.csv")
    for cell in sorted(glob.glob(pattern)):
        name = os.path.basename(cell)
        image = re.search(r"-(fn-[a-z-]+)-rtt", name).group(1)
        rtt = int(re.search(r"-rtt(\d+)ms", name).group(1))
        windows, roles = phase_windows(cell)
        for rep, phase, begin, end in windows:
            served = {}
            for stamp, address, written in responses:
                if begin <= stamp < end and address in addresses:
                    served[addresses[address]] = served.get(addresses[address], 0) + written
            for node, written in sorted(served.items()):
                role = "seed" if phase == "seed" else ("warm" if roles.get((rep, phase, node)) == "yes" else "cold")
                rows.append({"image": image, "rtt_ms": rtt, "rep": rep, "phase": phase, "node": node,
                             "role": role, "registry_bytes": written})

    out = arguments.out or os.path.join(SUMMARIES, f"{arguments.prefix}-config{arguments.config}-registry-bytes.csv")
    with open(out, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=["image", "rtt_ms", "rep", "phase", "node", "role", "registry_bytes"])
        writer.writeheader()
        writer.writerows(rows)
    print(f"wrote {len(rows)} rows to {out}")


if __name__ == "__main__":
    main()
