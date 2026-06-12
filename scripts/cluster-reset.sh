#!/usr/bin/env bash
set -euo pipefail

# Returns the cluster to a true cold state between benchmark runs: removes the
# workload and purges its image from every node, drops the in-memory cache
# contents, and clears the node-side nydus caches. Without the image purge a
# repeat run measures a warm start, because containerd still holds the image
# and its snapshots from the previous run. Safe to run in any configuration:
# the snapshotter is only restarted on nodes that carry the nydus drop-in
# (configs 2 and 4).
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODES=(k3d-nydus-cache-server-0 k3d-nydus-cache-agent-0 k3d-nydus-cache-agent-1)
DROPIN=/var/lib/rancher/k3s/agent/etc/containerd/config.toml.d/nydus-global.toml
CTR="ctr -a /run/k3s/containerd/containerd.sock -n k8s.io"
WORKLOAD_REPO=registry.localhost:5000/fn-python-hello

# Removes every reference to the workload image from one node — Knative
# resolves tags to digests, so both the tagged and the digest-pinned
# references must go or the next pull finds the content already present.
# Retries because the kubelet tears the previous run's containers down
# asynchronously and a busy snapshot blocks removal for a few seconds
purge_workload_images() {
  local node="$1" refs ref
  for _ in $(seq 1 12); do
    refs="$(docker exec "${node}" ${CTR} images ls -q 2>/dev/null | grep -F "${WORKLOAD_REPO}" || true)"
    if [[ -z "${refs}" ]]; then
      return 0
    fi
    for ref in ${refs}; do
      docker exec "${node}" ${CTR} images rm --sync "${ref}" >/dev/null 2>&1 || true
    done
    sleep 5
  done
  echo "${node}: could not purge ${WORKLOAD_REPO}; the next run would not be cold" >&2
  return 1
}

echo "Removing the workload so its image can be purged from the nodes"
kubectl delete ksvc fn-python-hello --ignore-not-found --timeout=120s
kubectl wait --for=delete pod -l serving.knative.dev/service=fn-python-hello --timeout=120s 2>/dev/null || true

echo "Restarting cache-service to clear in-memory contents"
kubectl rollout restart deploy/nydus-cluster-cache
kubectl rollout status deploy/nydus-cluster-cache --timeout=120s

echo "Purging workload images and clearing node nydus caches"
for node in "${NODES[@]}"; do
  purge_workload_images "${node}"
  "${REPO_ROOT}/scripts/node-snapshotter.sh" stop "${node}"
  docker exec "${node}" sh -c 'rm -rf /var/lib/rancher/k3s/agent/containerd-nydus/cache/* 2>/dev/null || true'
  if docker exec "${node}" sh -c "test -f ${DROPIN}"; then
    "${REPO_ROOT}/scripts/node-snapshotter.sh" start "${node}"
  fi
done

echo "Restarting the chunk-proxy DaemonSet"
kubectl -n kube-system rollout restart ds/chunk-proxy
kubectl -n kube-system rollout status ds/chunk-proxy --timeout=180s
echo "Reset complete"
