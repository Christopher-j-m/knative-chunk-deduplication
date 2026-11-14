#!/usr/bin/env bash
# Times handshakes and ranged reads against one or more registries from an AKS node
set -euo pipefail

OUT="${1:?usage: registry-path-probe-aks.sh <output csv> <repository> <blob digest> <registry> [<registry> ...]}"
REPOSITORY="${2:?repository missing}"
BLOB="${3:?blob digest missing}"
shift 3
[[ $# -gt 0 ]] || { echo "name at least one registry" >&2; exit 1; }
SINGLE_READS="${SINGLE_READS:-20}"
PARALLEL_ROUNDS="${PARALLEL_ROUNDS:-3}"
HANDSHAKES="${HANDSHAKES:-10}"

SETUP_POD="$(kubectl -n kube-system get pods -l app=nydus-node-setup -o jsonpath='{.items[0].metadata.name}')"
NODE="$(kubectl -n kube-system get pod "${SETUP_POD}" -o jsonpath='{.spec.nodeName}')"

echo "registry,node,mode,round,index,http_code,num_redirects,time_connect,time_appconnect,time_redirect,time_starttransfer,time_total,size_download,measured_at" >"${OUT}"

for registry in "$@"; do
  echo "probing ${registry} from ${NODE}" >&2

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

for index in $(seq 1 "${HANDSHAKES}"); do
  line="$(curl -s -o /dev/null -m 15 -w "${format}" "https://${REGISTRY}/v2/")"
  echo "${REGISTRY},${NODE},handshake,0,${index},${line},$(stamp)"
done

for index in $(seq 1 "${SINGLE_READS}"); do
  start=$(( index * 1048576 )); end=$(( start + 1048575 ))
  line="$(curl -s -o /dev/null -m 60 -L -r "${start}-${end}" -H "Authorization: Bearer ${token}" -w "${format}" "${url}")"
  echo "${REGISTRY},${NODE},single,0,${index},${line},$(stamp)"
done

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
