#!/usr/bin/env bash
set -euo pipefail

# Prints the nodes' nydusd configuration with the filesystem prefetch setting
# forced to one of two modes. Every published campaign ran with prefetch off,
# because a prefetching daemon fetches chunks the application never reads and
# the counter fingerprints the evaluation rests on stop being deterministic.
# The sensitivity campaign needs the other mode to bound what that choice cost
# the baseline.
#
# Usage: render-nydusd-config.sh <off|all>
MODE="${1:?usage: render-nydusd-config.sh <off|all>}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE="${REPO_ROOT}/deploy/local/snapshotter/nydusd-config.json"

python3 - "${SOURCE}" "${MODE}" <<'PYEOF'
import json
import sys

source, mode = sys.argv[1], sys.argv[2]

with open(source) as handle:
    config = json.load(handle)

prefetch = config.setdefault("fs_prefetch", {})

# The corpus is converted without prefetch patterns, so the bootstraps carry no
# hint list and enabling prefetch without prefetch_all would do nothing at all.
# The two modes below are therefore the only two that differ in practice
if mode == "off":
    prefetch["enable"] = False
    prefetch["prefetch_all"] = False
elif mode == "all":
    prefetch["enable"] = True
    prefetch["prefetch_all"] = True
else:
    sys.exit(f"unknown prefetch mode {mode!r}; choose off or all")

json.dump(config, sys.stdout, indent=2)
sys.stdout.write("\n")
PYEOF
