#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CLUSTER_DEF="${REPO_ROOT}/deploy/local/k3d-cluster.yaml"
REGISTRY="localhost:5000"
AGENTS="${AGENTS:-2}"

# Copy the kubernetes yaml to temp file for inserting the placeholder values for __AGENTS__ and __PWD__
RENDERED="$(mktemp)"
sed -e "s#__PWD__#${REPO_ROOT}#g" -e "s#__AGENTS__#${AGENTS}#g" \
  "${CLUSTER_DEF}" > "${RENDERED}"

echo "Creating k3d cluster"
k3d cluster delete nydus-cache >/dev/null 2>&1 || true
k3d cluster create --config "${RENDERED}"
kubectl wait --for=condition=Ready nodes --all --timeout=120s

echo "Building cluster cache and proxy images"
docker build -f "${REPO_ROOT}/deploy/Dockerfile" -t "${REGISTRY}/nydus-cluster-cache:0.1.0" "${REPO_ROOT}"
docker build -f "${REPO_ROOT}/deploy/Dockerfile.chunk-proxy" -t "${REGISTRY}/chunk-proxy:0.1.0" "${REPO_ROOT}"

echo "Pushing cluster cache and proxy images to registry"
docker push "${REGISTRY}/nydus-cluster-cache:0.1.0"
docker push "${REGISTRY}/chunk-proxy:0.1.0"

echo "Installing the nydus binaries onto the nodes"
"${REPO_ROOT}/scripts/cluster/fetch-nydus-binaries.sh"
"${REPO_ROOT}/scripts/cluster/node-snapshotter.sh" install