#!/usr/bin/env bash
# Waits until the nodes of the local rig stop receiving data and records what they received meanwhile
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
QUIET_RATE="${QUIET_RATE:-2097152}"
QUIET_SECONDS="${QUIET_SECONDS:-3}"
QUIET_TIMEOUT="${QUIET_TIMEOUT:-180}"
POLL_SECONDS="${POLL_SECONDS:-1}"
NODE_SOURCE="${NODE_SOURCE:-rig}"

declare -A SETUP_PODS
case "${NODE_SOURCE}" in
  rig)
    mapfile -t NODES < <("${REPO_ROOT}/scripts/cluster/cluster-nodes.sh")
    ;;
  kubernetes)
    mapfile -t NODES < <(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
    for node in "${NODES[@]}"; do
      SETUP_PODS["${node}"]="$(kubectl -n kube-system get pods -l app=nydus-node-setup \
        --field-selector "spec.nodeName=${node}" -o jsonpath='{.items[0].metadata.name}')"
    done
    ;;
  *)
    echo "wait-for-quiet-nodes: unknown NODE_SOURCE ${NODE_SOURCE}; choose rig or kubernetes" >&2
    exit 1
    ;;
esac
if [[ "${#NODES[@]}" -eq 0 ]]; then
  echo "wait-for-quiet-nodes: no cluster nodes found" >&2
  exit 1
fi

read_counters() {
  local node="$1"
  if [[ "${NODE_SOURCE}" == "kubernetes" ]]; then
    kubectl -n kube-system exec "${SETUP_PODS[${node}]}" -c agent -- nsenter -t 1 -n -- cat /proc/net/dev
  else
    docker exec "${node}" cat /proc/net/dev
  fi
}

received_bytes() {
  local node="$1" total only=""
  [[ "${NODE_SOURCE}" == "kubernetes" ]] && only="eth0"
  total="$(read_counters "${node}" | awk -F'[: ]+' -v only="${only}" '
    NR > 2 {
      name = ($1 == "") ? $2 : $1
      value = ($1 == "") ? $3 : $2
      if (name != "lo" && (only == "" || name == only)) { sum += value; found = 1 }
    }
    END { if (found) printf "%.0f", sum }')"
  if [[ -z "${total}" ]]; then
    echo "wait-for-quiet-nodes: could not read the network counters of ${node}" >&2
    exit 2
  fi
  echo "${total}"
}

now() {
  date +%s.%N
}

declare -A STARTING_BYTES LATEST_BYTES
for node in "${NODES[@]}"; do
  STARTING_BYTES["${node}"]="$(received_bytes "${node}")"
  LATEST_BYTES["${node}"]="${STARTING_BYTES[${node}]}"
done

started_at="$(now)"
previous_poll="${started_at}"
quiet_for=0
waited=0
outcome=""

while [[ -z "${outcome}" ]]; do
  sleep "${POLL_SECONDS}"
  poll="$(now)"

  arrived=0
  for node in "${NODES[@]}"; do
    current="$(received_bytes "${node}")"
    arrived=$((arrived + current - LATEST_BYTES[${node}]))
    LATEST_BYTES["${node}"]="${current}"
  done

  if awk -v bytes="${arrived}" -v from="${previous_poll}" -v to="${poll}" -v limit="${QUIET_RATE}" \
      'BEGIN { exit !(bytes <= limit * (to - from)) }'; then
    quiet_for="$(awk -v quiet="${quiet_for}" -v from="${previous_poll}" -v to="${poll}" \
      'BEGIN { printf "%.3f", quiet + to - from }')"
  else
    quiet_for=0
  fi
  previous_poll="${poll}"
  waited="$(awk -v from="${started_at}" -v to="${poll}" 'BEGIN { printf "%.1f", to - from }')"

  if awk -v quiet="${quiet_for}" -v needed="${QUIET_SECONDS}" 'BEGIN { exit !(quiet >= needed) }'; then
    outcome="quiet"
  elif awk -v waited="${waited}" -v limit="${QUIET_TIMEOUT}" 'BEGIN { exit !(waited >= limit) }'; then
    outcome="timeout"
  fi
done

if [[ -n "${DRAIN_RECORD:-}" ]]; then
  if [[ ! -s "${DRAIN_RECORD}" ]]; then
    echo "repetition,node,received_bytes,waited_seconds,outcome,seed_phase_bytes" >"${DRAIN_RECORD}"
  fi
  for node in "${NODES[@]}"; do
    seed_phase_bytes=""
    if [[ "${node}" == "${SEED_NODE:-}" && -n "${SEED_RX_BEFORE:-}" ]]; then
      seed_phase_bytes=$((LATEST_BYTES[${node}] - SEED_RX_BEFORE))
    fi
    echo "${REPETITION:-},${node},$((LATEST_BYTES[${node}] - STARTING_BYTES[${node}])),${waited},${outcome},${seed_phase_bytes}" >>"${DRAIN_RECORD}"
  done
fi

if [[ "${outcome}" == "timeout" ]]; then
  echo "wait-for-quiet-nodes: nodes were still receiving data after ${waited}s" >&2
  exit 1
fi
echo "wait-for-quiet-nodes: nodes quiet after ${waited}s"
