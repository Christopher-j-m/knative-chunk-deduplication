#!/usr/bin/env bash
set -euo pipefail

# Brings up the local k3d cluster, builds and pushes the two service images,
# and prepares the registry for the image corpus. Re-runnable: it deletes any
# existing cluster first.
#
# RUN-TIME NOTE: this script has not been executed in the authoring environment
# (no k3d/kubectl present there). Run it on a host with docker + k3d installed
# and verify each section's output before moving on.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CLUSTER_DEF="${REPO_ROOT}/deploy/local/k3d-cluster.yaml"
REGISTRY="localhost:5000"
# One server plus this many agents, matching the default the rig bring-up uses
AGENTS="${AGENTS:-2}"

echo "Rendering cluster definition with repo path ${REPO_ROOT}"
RENDERED="$(mktemp)"
sed -e "s#__PWD__#${REPO_ROOT}#g" -e "s#__AGENTS__#${AGENTS}#g" \
  "${CLUSTER_DEF}" > "${RENDERED}"

echo "Recreating k3d cluster"
k3d cluster delete nydus-cache >/dev/null 2>&1 || true
k3d cluster create --config "${RENDERED}"

echo "Waiting for nodes to be Ready"
kubectl wait --for=condition=Ready nodes --all --timeout=120s

echo "Building and pushing service images"
docker build -f "${REPO_ROOT}/deploy/Dockerfile" -t "${REGISTRY}/nydus-cluster-cache:0.1.0" "${REPO_ROOT}"
docker build -f "${REPO_ROOT}/deploy/Dockerfile.chunk-proxy" -t "${REGISTRY}/chunk-proxy:0.1.0" "${REPO_ROOT}"
docker push "${REGISTRY}/nydus-cluster-cache:0.1.0"
docker push "${REGISTRY}/chunk-proxy:0.1.0"

echo "Installing the nydus binaries onto the nodes"
"${REPO_ROOT}/scripts/fetch-nydus-binaries.sh"
"${REPO_ROOT}/scripts/node-snapshotter.sh" install

echo "Cluster up. Load the image corpus next with scripts/convert-corpus-uncompressed.sh"
echo "pointed at ${REGISTRY}, then apply deploy/k8s/cache-service."
