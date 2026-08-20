#!/usr/bin/env bash
set -euo pipefail

# Selects benchmark configuration 1, 2, or 4 by moving three knobs:
#   1. the cluster-wide containerd snapshotter: config 1 keeps the overlayfs
#      default, configs 2 and 4 switch the global default to nydus through a
#      containerd drop-in, because image pulls always use the global
#      snapshotter and never a per-runtime one
#   2. the workload image (plain OCI for config 1, uncompressed Nydus for 2/4)
#   3. the chunk-proxy cache toggle (--cache-enabled, config 2 vs 4)
# Configuration 3 (Dragonfly) is deferred and exits non-zero.
#
# Changing the global snapshotter needs a containerd restart, so every switch
# cycles the nodes (agents first, server last, so the API server goes down
# last) and then restarts the host-process snapshotter, which does not survive
# a node restart because the k3d nodes have no service manager.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${1:?usage: config-switch.sh <1|2|4>}"

AGENTS=(k3d-nydus-cache-agent-0 k3d-nydus-cache-agent-1)
SERVER=k3d-nydus-cache-server-0
DROPIN=/var/lib/rancher/k3s/agent/etc/containerd/config.toml.d/nydus-global.toml

place_global_dropin() {
  for node in "${AGENTS[@]}" "${SERVER}"; do
    # a genuinely cold node has never had anything dropped into
    # config.toml.d, so the directory does not exist yet and docker cp
    # refuses to create a missing parent
    docker exec "${node}" mkdir -p "$(dirname "${DROPIN}")"
    docker cp "${REPO_ROOT}/deploy/local/snapshotter/nydus-global.toml" "${node}:${DROPIN}"
  done
}

remove_global_dropin() {
  for node in "${AGENTS[@]}" "${SERVER}"; do
    docker exec "${node}" rm -f "${DROPIN}"
  done
}

# Restarts every node so k3s re-renders the containerd config with or without
# the drop-in, then waits for the API server and the nodes to come back
cycle_nodes() {
  for node in "${AGENTS[@]}" "${SERVER}"; do
    docker restart "${node}"
  done
  for _ in $(seq 1 60); do
    kubectl get nodes >/dev/null 2>&1 && break
    sleep 5
  done
  kubectl wait --for=condition=Ready nodes --all --timeout=300s
}

set_cache_enabled() {
  # Patches the chunk-proxy container (index 0), --cache-enabled arg (index 1)
  kubectl -n kube-system patch ds/chunk-proxy --type=json -p \
    "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/1\",\"value\":\"--cache-enabled=$1\"}]"
  kubectl -n kube-system rollout status ds/chunk-proxy --timeout=300s
}

# Replaces the workload service outright so no revision from the previous
# configuration lingers in the traffic split. The node cycle restarts the
# Knative webhook too, and ksvc writes fail until it serves again — its
# Deployment status is stale right after the cycle, so retry the writes
# instead of trusting a condition wait
apply_workload() {
  for _ in $(seq 1 30); do
    if kubectl delete ksvc fn-python-hello --ignore-not-found --timeout=120s \
        && kubectl apply -f "$1"; then
      return 0
    fi
    sleep 5
  done
  echo "apply_workload: Knative webhook never became reachable" >&2
  return 1
}

case "${CONFIG}" in
  1)
    echo "Config 1 - containerd baseline (overlayfs, OCI image)"
    remove_global_dropin
    cycle_nodes
    "${REPO_ROOT}/scripts/node-snapshotter.sh" stop
    apply_workload "${REPO_ROOT}/deploy/k8s/knative/workload-oci.yaml"
    ;;
  2)
    echo "Config 2 - Nydus local-only (cache bypassed)"
    place_global_dropin
    cycle_nodes
    "${REPO_ROOT}/scripts/node-snapshotter.sh" start
    set_cache_enabled "false"
    apply_workload "${REPO_ROOT}/deploy/k8s/knative/workload-nydus.yaml"
    ;;
  4)
    echo "Config 4 - Nydus + cluster cache"
    place_global_dropin
    cycle_nodes
    "${REPO_ROOT}/scripts/node-snapshotter.sh" start
    set_cache_enabled "true"
    apply_workload "${REPO_ROOT}/deploy/k8s/knative/workload-nydus.yaml"
    ;;
  3)
    echo "Config 3 (Nydus + Dragonfly) is deferred and not wired" >&2
    exit 1
    ;;
  *)
    echo "Unknown config ${CONFIG}; choose 1, 2, or 4" >&2
    exit 1
    ;;
esac
echo "Switched to config ${CONFIG}. Run scripts/cluster-reset.sh before measuring."
