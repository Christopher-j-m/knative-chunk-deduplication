#!/usr/bin/env bash
# Installs, removes or checks Dragonfly on the local rig with the settings of the AKS experiments
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ACTION="${1:?usage: dragonfly-rig.sh <install|uninstall|healthy|registry-url>}"
NAMESPACE=dragonfly-system
RELEASE=dragonfly
CHART_VERSION=1.7.0

registry_url() {
  local address
  address="$(docker inspect -f '{{(index .NetworkSettings.Networks "k3d-nydus-cache").IPAddress}}' registry.localhost)"
  [[ -n "${address}" ]] || { echo "registry.localhost has no address on k3d-nydus-cache" >&2; exit 1; }
  echo "http://${address}:5000"
}

all_clients_healthy() {
  local node
  for node in $("${REPO_ROOT}/scripts/cluster/cluster-nodes.sh"); do
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
