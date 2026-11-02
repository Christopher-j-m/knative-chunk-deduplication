#!/usr/bin/env bash
# Prints the nydusd config with prefetch switched on or off and with the chosen storage backend
# This script doesnt change the config file in the repo, it only prints the result
# (node-snapshotter.sh saves this output to a temp file and copies it onto the nodes)
# => Prefetch = download image ahead of time, not lazily when needed by demand
# => BACKEND=http-proxy (default) sends every read through the chunk proxy on the node
# => BACKEND=registry is stock nydusd talking to the registry itself, without the chunk proxy
# => BACKEND=dragonfly is stock nydusd reading through the Dragonfly client on its node
set -euo pipefail

MODE="${1:?usage: [BACKEND=http-proxy|registry|dragonfly] render-nydusd-config.sh <off|all>}"
BACKEND="${BACKEND:-http-proxy}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE="${REPO_ROOT}/deploy/local/snapshotter/nydusd-config.json"

# The Dragonfly client needs to be told which registry a request is meant for
REGISTRY_URL=""
if [[ "${BACKEND}" == "dragonfly" ]]; then
  REGISTRY_URL="$("${REPO_ROOT}/scripts/dragonfly-rig.sh" registry-url)"
fi

python3 - "${SOURCE}" "${MODE}" "${BACKEND}" "${REGISTRY_URL}" <<'PYEOF'
import json
import sys

source, mode, backend, registry_url = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

with open(source) as handle:
    config = json.load(handle)

prefetch = config.setdefault("fs_prefetch", {})

if mode == "off":
    prefetch["enable"] = False
    prefetch["prefetch_all"] = False
elif mode == "all":
    prefetch["enable"] = True
    prefetch["prefetch_all"] = True
else:
    sys.exit(f"unknown prefetch mode {mode!r}; choose off or all")

# The snapshotter fills in the registry host and repository for each image,
# so the stock block only names how to reach it: plain http on the rig
registry_block = {
    "type": "registry",
    "config": {
        "scheme": "http",
        "timeout": 10,
        "connect_timeout": 10,
        "retry_limit": 2,
    },
}
if backend == "registry":
    config["device"]["backend"] = registry_block
elif backend == "dragonfly":
    # The same stock block, with the node's Dragonfly client as a mirror in
    # front of the registry, as in the managed-cloud campaign
    registry_block["config"]["mirrors"] = [{
        "host": "http://127.0.0.1:4001",
        "auth_through": False,
        "headers": {"X-Dragonfly-Registry": registry_url},
        "ping_url": "http://127.0.0.1:4003/healthy",
    }]
    config["device"]["backend"] = registry_block
elif backend != "http-proxy":
    sys.exit(f"unknown backend {backend!r}; choose http-proxy, registry or dragonfly")

json.dump(config, sys.stdout, indent=2)
sys.stdout.write("\n")
PYEOF
