#!/usr/bin/env bash
# Installs or removes Dragonfly on the local rig with the chart and settings of
# the managed-cloud campaign. Every client runs on its node's network, so
# nydusd on that node reaches it on the loopback address
# Usage: dragonfly-rig.sh <install|uninstall|healthy|registry-url>
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACTION="${1:?usage: dragonfly-rig.sh <install|uninstall|healthy|registry-url>}"
NAMESPACE=dragonfly-system
RELEASE=dragonfly
CHART_VERSION=1.7.0

# The clients run on the node network, where the registry's container name
# does not resolve, so they are given its address on the cluster network
registry_url() {
  local address
  address="$(docker inspect -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' registry.localhost)"
  [[ -n "${address}" ]] || { echo "registry.localhost has no address on k3d-nydus-cache" >&2; exit 1; }
  echo "http://${address}:5000"
}

# Every node's client must answer its health endpoint before a cell may start
all_clients_healthy() {
  local node
  for node in $("${REPO_ROOT}/scripts/cluster-nodes.sh"); do
    docker exec "${node}" wget -qO- -T 5 http://127.0.0.1:4003/healthy >/dev/null 2>&1 || return 1
  done
}

case "${ACTION}" in
  install)
    helm repo add dragonfly https://dragonflyoss.github.io/helm-charts/ >/dev/null 2>&1 || true
    helm repo update dragonfly >/dev/null
    values="$(mktemp)"
    trap 'rm -f "${values}"' EXIT
    sed "s|__REGISTRY__|$(registry_url)|" "${REPO_ROOT}/deploy/local/dragonfly/values.yaml" >"${values}"
    helm upgrade --install "${RELEASE}" dragonfly/dragonfly --version "${CHART_VERSION}" \
      --namespace "${NAMESPACE}" --create-namespace -f "${values}" --wait --timeout 20m
    kubectl -n "${NAMESPACE}" rollout status ds/dragonfly-client --timeout=600s
    for _ in $(seq 1 60); do
      all_clients_healthy && { echo "Dragonfly installed, every client healthy"; exit 0; }
      sleep 5
    done
    echo "Dragonfly installed but a client does not answer on 127.0.0.1:4003" >&2
    exit 1
    ;;
  uninstall)
    # A fresh install per switch also leaves no stale scheduler rows behind in
    # the manager's database, which a scheduler restart would otherwise strand
    if helm status "${RELEASE}" -n "${NAMESPACE}" >/dev/null 2>&1; then
      helm uninstall "${RELEASE}" -n "${NAMESPACE}" --wait --timeout 10m
    fi
    kubectl delete namespace "${NAMESPACE}" --ignore-not-found --timeout=300s
    echo "Dragonfly removed"
    ;;
  healthy)
    all_clients_healthy
    ;;
  registry-url)
    registry_url
    ;;
  *)
    echo "unknown action ${ACTION}" >&2
    exit 1
    ;;
esac
