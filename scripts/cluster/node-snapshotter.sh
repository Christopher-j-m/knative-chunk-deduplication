#!/usr/bin/env bash
# Handling of the nydus snapshotter on local clusters, since on them theres no way to run background services themselves
# There are 4 commands:
# Install = Copies the nydus snapshotter binary to each node
# Start/Stop = Starts/Stops the nydus snapshotter on each node as a host process inside the node
# Status = Prints if the snapshotter is running on each node or not
set -euo pipefail

ACTION="${1:?usage: node-snapshotter.sh <install|start|stop|status> [node]}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Get the node names of the cluster (locally these are node containers) as list
mapfile -t ALL_NODES < <("${REPO_ROOT}/scripts/cluster/cluster-nodes.sh")
if [[ "${#ALL_NODES[@]}" -eq 0 ]]; then
  echo "no cluster nodes found" >&2
  exit 1
fi
# Only manage a specific node (if specified as script input)
if [[ -n "${2:-}" ]]; then
  NODES=("$2")
# ...else all of them
else
  NODES=("${ALL_NODES[@]}")
fi

# We dont want to pre-download container images fully in the background
PREFETCH="${PREFETCH:-off}"

BACKEND="${BACKEND:-http-proxy}"
BIN_DIR=/opt/nydus/bin
SOCKET=/run/containerd-nydus/containerd-nydus-grpc.sock
LOG_FILE=/var/log/nydus-snapshotter.log

# check if the containerd-nydus-grpc process is running inside a specific cluster node
daemon_running() {
  docker exec "$1" sh -c "ps | grep -v grep | grep -q containerd-nydus-grpc"
}

# Kills a specific process inside a specific cluster node
kill_matching() {
  docker exec "$1" sh -c "ps | grep -v grep | grep $2 | awk '{print \$1}' | xargs kill 2>/dev/null || true"
}

# Create a temp config file with specific prefetch value (enabled/disabled)
RENDERED_NYDUSD_CONFIG="$(mktemp)"
trap 'rm -f "${RENDERED_NYDUSD_CONFIG}"' EXIT
if [[ "${ACTION}" == "install" ]]; then
  BACKEND="${BACKEND}" "${REPO_ROOT}/scripts/cluster/render-nydusd-config.sh" "${PREFETCH}" >"${RENDERED_NYDUSD_CONFIG}"
fi

# Run the specific commands
for node in "${NODES[@]}"; do
  case "${ACTION}" in
    install)
      docker exec "${node}" mkdir -p "${BIN_DIR}" /etc/nydus
      for binary in containerd-nydus-grpc nydusd nydus-image; do
        docker cp "${REPO_ROOT}/deploy/local/bin/${binary}" "${node}:${BIN_DIR}/${binary}"
      done
      docker cp "${REPO_ROOT}/deploy/local/snapshotter/config.toml" "${node}:/etc/nydus/config.toml"
      docker cp "${RENDERED_NYDUSD_CONFIG}" "${node}:/etc/nydus/nydusd-config.json"
      echo "${node}: installed (prefetch ${PREFETCH}, backend ${BACKEND})"
      ;;
    start)
      if daemon_running "${node}"; then
        echo "${node}: already running"
        continue
      fi
      docker exec -d "${node}" sh -c \
        "exec ${BIN_DIR}/containerd-nydus-grpc --config /etc/nydus/config.toml >>${LOG_FILE} 2>&1"
      # Wait until the startup is complete
      for _ in $(seq 1 240); do
        if docker exec "${node}" sh -c "test -S ${SOCKET}"; then break; fi
        sleep 0.5
      done
      # Test if startup was actually successful
      docker exec "${node}" sh -c "test -S ${SOCKET}" \
        || { echo "${node}: socket apparently didnt appear => see ${LOG_FILE} on the node" >&2; exit 1; }
      echo "${node}: started"
      ;;
    stop)
      kill_matching "${node}" containerd-nydus-grpc
      kill_matching "${node}" "${BIN_DIR}/nydusd"
      echo "${node}: stopped"
      ;;
    status)
      if daemon_running "${node}"; then
        running="running"
      else
        running="not running"
      fi
      if docker exec "${node}" sh -c "test -S ${SOCKET}"; then
        socket="socket present"
      else
        socket="no socket"
      fi
      echo "${node}: ${running}, ${socket}"
      ;;
    *)
      echo "only install, start, stop or status allowed" >&2
      exit 1
      ;;
  esac
done
