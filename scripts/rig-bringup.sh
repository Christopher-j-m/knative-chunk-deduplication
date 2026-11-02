#!/usr/bin/env bash
# Starts the cluster on a azure vm for measurements
# Since the disk is wiped (for cost purposes),
# this also installs the required services on each node again
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER=nydus-cache
AGENTS="${AGENTS:-2}"
KNATIVE_VERSION=knative-v1.22.1

cd "${REPO_ROOT}"

echo "creating the k3d cluster"
if ! k3d cluster list | grep -q "^${CLUSTER}"; then
  sed -e "s|__PWD__|${REPO_ROOT}|g" -e "s|__AGENTS__|${AGENTS}|g" deploy/local/k3d-cluster.yaml > /tmp/k3d-cluster.yaml
  k3d cluster create --config /tmp/k3d-cluster.yaml
fi
kubectl config use-context "k3d-${CLUSTER}"

echo "waiting for nodes to get ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=300s
kubectl get nodes

echo "building and pushing the cluster-cache service images"
docker build -f deploy/Dockerfile -t localhost:5000/nydus-cluster-cache:0.1.0 .
docker build -f deploy/Dockerfile.chunk-proxy -t localhost:5000/chunk-proxy:0.1.0 .
docker push localhost:5000/nydus-cluster-cache:0.1.0
docker push localhost:5000/chunk-proxy:0.1.0

echo "installing and starting the nydus snapshotter..."
./scripts/fetch-nydus-binaries.sh
./scripts/node-snapshotter.sh install
./scripts/node-snapshotter.sh start
./scripts/node-snapshotter.sh status

echo "pushing the images into the local registry"
CORPUS_IMAGES="${CORPUS_IMAGES:-fn-go-hello fn-python-hello fn-java-springboot fn-node-api fn-python-ml fn-pytorch-cpu}"
for img in ${CORPUS_IMAGES}; do
  docker tag "localhost:5000/${img}:oci" "localhost:5000/${img}:latest"
  docker push "localhost:5000/${img}:oci"
  docker push "localhost:5000/${img}:latest"
done

# Convert the images to nydus format for laty pulling
CORPUS_IMAGES="${CORPUS_IMAGES}" "${REPO_ROOT}/scripts/convert-corpus-uncompressed.sh"

echo "deploying the cache service and chunk proxy..."
kubectl apply -f deploy/k8s/cache-service/deployment.yaml -f deploy/k8s/cache-service/service.yaml
kubectl rollout status deploy/nydus-cluster-cache --timeout=120s
kubectl apply -f deploy/k8s/chunk-proxy/daemonset.yaml
kubectl -n kube-system rollout status ds/chunk-proxy --timeout=300s

echo "installing knative..."
if kubectl get ns knative-serving >/dev/null 2>&1; then
  echo "knative already installed"
else
  kubectl apply -f "https://github.com/knative/serving/releases/download/${KNATIVE_VERSION}/serving-crds.yaml"
  kubectl apply -f "https://github.com/knative/serving/releases/download/${KNATIVE_VERSION}/serving-core.yaml"
  kubectl apply -f "https://github.com/knative/net-kourier/releases/download/${KNATIVE_VERSION}/kourier.yaml"
  kubectl wait --for=condition=Ready pods --all -n knative-serving --timeout=300s
  kubectl apply --server-side --force-conflicts -f deploy/k8s/knative/serving-config.yaml
  kubectl -n knative-serving annotate configmap config-deployment kubectl.kubernetes.io/last-applied-configuration- --overwrite >/dev/null 2>&1 || true
fi

echo "switching to config with nydus & cluster cache enabled (config 4)"
./scripts/config-switch.sh 4
./scripts/registry-netem.sh off || true
./scripts/registry-netem.sh status
