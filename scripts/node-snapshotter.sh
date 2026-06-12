#!/usr/bin/env bash
set -euo pipefail

# Manages the nydus snapshotter as a host process inside the k3d node
# containers, which ship no service manager: install copies the binaries and
# configs onto the nodes, start launches the daemon with a detached docker
# exec (required again after every node restart), stop kills the daemon and
# its nydusd children, status reports process and socket per node.
ACTION="${1:?usage: node-snapshotter.sh <install|start|stop|status> [node]}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ALL_NODES=(k3d-nydus-cache-server-0 k3d-nydus-cache-agent-0 k3d-nydus-cache-agent-1)
if [[ -n "${2:-}" ]]; then
  NODES=("$2")
else
  NODES=("${ALL_NODES[@]}")
fi

BIN_DIR=/opt/nydus/bin
SOCKET=/run/containerd-nydus/containerd-nydus-grpc.sock
LOG_FILE=/var/log/nydus-snapshotter.log

# The node containers ship no pgrep or pkill, so process lookup and shutdown
# go through ps and grep instead
daemon_running() {
  docker exec "$1" sh -c "ps | grep -v grep | grep -q containerd-nydus-grpc"
}

kill_matching() {
  docker exec "$1" sh -c "ps | grep -v grep | grep $2 | awk '{print \$1}' | xargs kill 2>/dev/null || true"
}

for node in "${NODES[@]}"; do
  case "${ACTION}" in
    install)
      docker exec "${node}" mkdir -p "${BIN_DIR}" /etc/nydus
      for binary in containerd-nydus-grpc nydusd nydus-image; do
        docker cp "${REPO_ROOT}/deploy/local/bin/${binary}" "${node}:${BIN_DIR}/${binary}"
      done
      docker cp "${REPO_ROOT}/deploy/local/snapshotter/config.toml" "${node}:/etc/nydus/config.toml"
      docker cp "${REPO_ROOT}/deploy/local/snapshotter/nydusd-config.json" "${node}:/etc/nydus/nydusd-config.json"
      echo "${node}: installed"
      ;;
    start)
      if daemon_running "${node}"; then
        echo "${node}: already running"
        continue
      fi
      docker exec -d "${node}" sh -c \
        "exec ${BIN_DIR}/containerd-nydus-grpc --config /etc/nydus/config.toml >>${LOG_FILE} 2>&1"
      # the socket appearing is the readiness signal containerd will look for;
      # it can take a while after a node restart because the daemon first
      # recovers (or cleans up) every nydusd it managed before the restart
      for _ in $(seq 1 240); do
        if docker exec "${node}" sh -c "test -S ${SOCKET}"; then break; fi
        sleep 0.5
      done
      docker exec "${node}" sh -c "test -S ${SOCKET}" \
        || { echo "${node}: FAIL - socket never appeared, see ${LOG_FILE} on the node" >&2; exit 1; }
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
      echo "unknown action ${ACTION}; use install, start, stop or status" >&2
      exit 1
      ;;
  esac
done
