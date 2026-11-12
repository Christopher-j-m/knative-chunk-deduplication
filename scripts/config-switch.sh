#!/usr/bin/env bash
# Change the configuration of the local test cluster
# 1  = oci images without nydus or cluster cache
# 2  = nydus without cluster cache, every read still goes through the chunk proxy
# 2s = stock nydus without cluster cache, nydusd reads the registry itself and no proxy is involved
# 3  = stock nydus reading through Dragonfly's peer-to-peer clients, no proxy or cache involved
# 4  = nydus with cluster cache
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${1:?usage: config-switch.sh <1|2|2s|3|4>}"

# Get all cluster nodes
mapfile -t ALL_NODES < <("${REPO_ROOT}/scripts/cluster-nodes.sh")
if [[ "${#ALL_NODES[@]}" -eq 0 ]]; then
  echo "no cluster nodes found" >&2
  exit 1
fi
SERVER="${ALL_NODES[0]}"
AGENTS=("${ALL_NODES[@]:1}")
DROPIN=/var/lib/rancher/k3s/agent/etc/containerd/config.toml.d/nydus-global.toml

# copy the nydus config file to every node => this causes containerd to use nydus for all images instead
place_global_dropin() {
  for node in "${AGENTS[@]}" "${SERVER}"; do
    docker exec "${node}" mkdir -p "$(dirname "${DROPIN}")"
    docker cp "${REPO_ROOT}/deploy/local/snapshotter/nydus-global.toml" "${node}:${DROPIN}"
  done
}

# removes the nydus config file from every node again, so that containerd doesnt use nydus anymore
remove_global_dropin() {
  for node in "${AGENTS[@]}" "${SERVER}"; do
    docker exec "${node}" rm -f "${DROPIN}"
  done
}

# Restart every cluster node so the new containerd config file (e.g. with nydus settings) is read
cycle_nodes() {
  local balancer="${SERVER%-server-0}-serverlb"
  for node in "${AGENTS[@]}" "${SERVER}"; do
    docker restart "${node}"
  done

  # A restarted node can come back on another address, while the load balancer
  # in front of the control plane keeps the address it looked up at its own
  # start and then refuses every kubectl call, so it is restarted as well
  if docker inspect "${balancer}" >/dev/null 2>&1; then
    docker restart "${balancer}"
  fi

  for _ in $(seq 1 60); do
    kubectl get nodes >/dev/null 2>&1 && break
    sleep 5
  done
  kubectl wait --for=condition=Ready nodes --all --timeout=300s
}

# Enable the cluster cache route by rewriting the second input param
set_cache_enabled() {
  kubectl -n kube-system patch ds/chunk-proxy --type=json -p \
    "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/1\",\"value\":\"--cache-enabled=$1\"}]"
  kubectl -n kube-system rollout status ds/chunk-proxy --timeout=300s
}

# delete the python-hello test service and deploy it again via workfload file (either oci or nydus)
apply_workload() {
  for _ in $(seq 1 30); do
    if kubectl delete ksvc fn-python-hello --ignore-not-found --timeout=120s \
        && kubectl apply -f "$1"; then
      return 0
    fi
    sleep 5
  done
  echo "error in apply_workload while removing the python test service" >&2
  return 1
}

# Copy nydusd onto every node with the given storage backend. Every nydus config
# installs explicitly, otherwise switching back from 2s would keep the stock backend.
# The prefetch mode comes from PREFETCH and stays off unless a campaign asks for it
install_backend() {
  PREFETCH="${PREFETCH:-off}" BACKEND="$1" "${REPO_ROOT}/scripts/node-snapshotter.sh" install
}

# Fail the switch when a node does not carry the backend the config asked for.
# Stock nydusd and Dragonfly share the registry type and differ only in the
# mirror, so the mirror count is part of what is checked
assert_backend() {
  local expected="$1" expected_mirrors="${2:-0}"
  local node actual
  for node in "${AGENTS[@]}" "${SERVER}"; do
    actual="$(docker exec "${node}" cat /etc/nydus/nydusd-config.json \
      | python3 -c 'import json,sys; backend = json.load(sys.stdin)["device"]["backend"]; print(backend["type"], len(backend.get("config", {}).get("mirrors", [])))')"
    if [[ "${actual}" != "${expected} ${expected_mirrors}" ]]; then
      echo "${node}: nydusd backend is ${actual}, expected ${expected} with ${expected_mirrors} mirrors" >&2
      exit 1
    fi
  done
  echo "nydusd backend on every node: ${expected} with ${expected_mirrors} mirrors"
}

# Dragonfly only runs under configuration 3, so its clients cannot take
# processor time from the other configurations' cells
remove_dragonfly() {
  "${REPO_ROOT}/scripts/dragonfly-rig.sh" uninstall
}

# Handle the different input params based on which config we want to setup
case "${CONFIG}" in
  1)
    echo "Config 1: plain containerd with oci images"
    remove_dragonfly
    remove_global_dropin
    cycle_nodes
    "${REPO_ROOT}/scripts/node-snapshotter.sh" stop
    apply_workload "${REPO_ROOT}/deploy/k8s/knative/workload-oci.yaml"
    ;;
  2)
    echo "Config 2: nydus without cluster cache"
    remove_dragonfly
    place_global_dropin
    cycle_nodes
    install_backend http-proxy
    "${REPO_ROOT}/scripts/node-snapshotter.sh" start
    assert_backend http-proxy
    set_cache_enabled "false"
    apply_workload "${REPO_ROOT}/deploy/k8s/knative/workload-nydus.yaml"
    ;;
  2s)
    echo "Config 2s: stock nydusd, registry backend, no proxy"
    remove_dragonfly
    place_global_dropin
    cycle_nodes
    install_backend registry
    "${REPO_ROOT}/scripts/node-snapshotter.sh" start
    assert_backend registry 0
    apply_workload "${REPO_ROOT}/deploy/k8s/knative/workload-nydus.yaml"
    ;;
  4)
    echo "Config 4: nydus + cluster cache"
    remove_dragonfly
    place_global_dropin
    cycle_nodes
    install_backend http-proxy
    "${REPO_ROOT}/scripts/node-snapshotter.sh" start
    assert_backend http-proxy
    set_cache_enabled "true"
    apply_workload "${REPO_ROOT}/deploy/k8s/knative/workload-nydus.yaml"
    ;;
  3)
    echo "Config 3: stock nydusd reading through Dragonfly"
    place_global_dropin
    cycle_nodes
    # The snapshotter must run before anything else starts, because with the
    # global drop-in every pod, Dragonfly's own included, mounts through it.
    # Dragonfly follows the node restart, so its clients start on the final
    # containers, and it is up before the first function image is mounted
    install_backend dragonfly
    "${REPO_ROOT}/scripts/node-snapshotter.sh" start
    assert_backend registry 1
    "${REPO_ROOT}/scripts/dragonfly-rig.sh" install
    set_cache_enabled "false"
    apply_workload "${REPO_ROOT}/deploy/k8s/knative/workload-nydus.yaml"
    ;;
  *)
    echo "Only config 1, 2, 2s, 3, 4 are implemented" >&2
    exit 1
    ;;
esac
echo "Switched to config ${CONFIG}"
