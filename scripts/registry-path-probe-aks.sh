#!/usr/bin/env bash
# Times the registry path from one AKS node, the way a lazily pulling nydusd sees it.
# For every registry it measures the round trip (TCP handshakes to the registry's front end),
# then ranged 1 MiB reads of one blob, first one at a time and then 16 in flight at once,
# each read following the registry's redirect to its storage as nydusd does.
# The registry is asked anonymously, as the cluster nodes pull. One CSV row per request.
# Usage: KUBECONFIG=... registry-path-probe-aks.sh <output csv> <repository> <blob digest> <registry> [<registry> ...]
set -euo pipefail

OUT="${1:?usage: registry-path-probe-aks.sh <output csv> <repository> <blob digest> <registry> [<registry> ...]}"
REPOSITORY="${2:?repository missing}"
BLOB="${3:?blob digest missing}"
shift 3
[[ $# -gt 0 ]] || { echo "name at least one registry" >&2; exit 1; }
SINGLE_READS="${SINGLE_READS:-20}"
PARALLEL_ROUNDS="${PARALLEL_ROUNDS:-3}"
HANDSHAKES="${HANDSHAKES:-10}"

# Any node will do; the first node-setup pod enters its host's network namespace
SETUP_POD="$(kubectl -n kube-system get pods -l app=nydus-node-setup -o jsonpath='{.items[0].metadata.name}')"
NODE="$(kubectl -n kube-system get pod "${SETUP_POD}" -o jsonpath='{.spec.nodeName}')"

echo "registry,node,mode,round,index,http_code,num_redirects,time_connect,time_appconnect,time_redirect,time_starttransfer,time_total,size_download,measured_at" >"${OUT}"

for registry in "$@"; do
  echo "probing ${registry} from ${NODE}" >&2

  # The script runs on the node itself, so nothing of the timing passes
  # through kubectl; it prints finished CSV rows
  kubectl -n kube-system exec -i "${SETUP_POD}" -c agent -- nsenter -t 1 -m -n -- \
    env REGISTRY="${registry}" REPOSITORY="${REPOSITORY}" BLOB="${BLOB}" NODE="${NODE}" \
    SINGLE_READS="${SINGLE_READS}" PARALLEL_ROUNDS="${PARALLEL_ROUNDS}" HANDSHAKES="${HANDSHAKES}" \
    bash -s >>"${OUT}" <<'NODE_SCRIPT'
set -euo pipefail
token="$(curl -s "https://${REGISTRY}/oauth2/token?service=${REGISTRY}&scope=repository:${REPOSITORY}:pull" \
  | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
[ -n "${token}" ] || { echo "no anonymous token from ${REGISTRY}" >&2; exit 1; }
url="https://${REGISTRY}/v2/${REPOSITORY}/blobs/${BLOB}"
format="%{http_code},%{num_redirects},%{time_connect},%{time_appconnect},%{time_redirect},%{time_starttransfer},%{time_total},%{size_download}"
stamp() { date -u +%FT%T.%3NZ; }

# A TCP handshake to the front end costs exactly one round trip
for index in $(seq 1 "${HANDSHAKES}"); do
  line="$(curl -s -o /dev/null -m 15 -w "${format}" "https://${REGISTRY}/v2/")"
  echo "${REGISTRY},${NODE},handshake,0,${index},${line},$(stamp)"
done

# One read at a time, each on a fresh connection, a different MiB each time
for index in $(seq 1 "${SINGLE_READS}"); do
  start=$(( index * 1048576 )); end=$(( start + 1048575 ))
  line="$(curl -s -o /dev/null -m 60 -L -r "${start}-${end}" -H "Authorization: Bearer ${token}" -w "${format}" "${url}")"
  echo "${REGISTRY},${NODE},single,0,${index},${line},$(stamp)"
done

# Sixteen reads in flight at once, as nydusd's and the proxy's parallel fetches put them
for round in $(seq 1 "${PARALLEL_ROUNDS}"); do
  directory="$(mktemp -d)"
  for index in $(seq 1 16); do
    start=$(( (round * 16 + index) * 1048576 )); end=$(( start + 1048575 ))
    curl -s -o /dev/null -m 60 -L -r "${start}-${end}" -H "Authorization: Bearer ${token}" \
      -w "${format}" "${url}" >"${directory}/${index}" &
  done
  wait
  for index in $(seq 1 16); do
    echo "${REGISTRY},${NODE},parallel16,${round},${index},$(cat "${directory}/${index}"),$(stamp)"
  done
  rm -rf "${directory}"
done
NODE_SCRIPT
done

# A short summary per registry and mode: median of the totals and of the connects
python3 - "${OUT}" <<'PYEOF'
import csv
import statistics
import sys
from collections import defaultdict

totals, connects, codes = defaultdict(list), defaultdict(list), defaultdict(set)
with open(sys.argv[1], newline="") as handle:
    for row in csv.DictReader(handle):
        key = (row["registry"], row["mode"])
        totals[key].append(float(row["time_total"]))
        connects[key].append(float(row["time_connect"]))
        codes[key].add(row["http_code"])
for key in sorted(totals):
    print(f"{key[0]} {key[1]}: n={len(totals[key])} median total {statistics.median(totals[key]) * 1000:.1f} ms,"
          f" median connect {statistics.median(connects[key]) * 1000:.1f} ms, status {sorted(codes[key])}")
PYEOF
