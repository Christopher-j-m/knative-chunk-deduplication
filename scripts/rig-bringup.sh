#!/usr/bin/env bash
# Brings a cold Azure rig up to configuration 4 with the corpus loaded.
# The local NVMe is wiped on deallocate, so this runs at the start of every
# campaign session. The registry that used to hold the corpus is gone, so
# the OCI tags come from the host docker store and the nydus tags are
# rebuilt locally; the script otherwise composes the existing bring-up
# procedures rather than inventing new ones
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER=nydus-cache
# One server plus this many agents. The published campaigns all run three
# nodes; the node-count sweep asks for six agents and cordons back down
AGENTS="${AGENTS:-2}"
KNATIVE_VERSION=knative-v1.22.1

cd "${REPO_ROOT}"

echo "== 1/8 creating the k3d cluster =="
if ! k3d cluster list | grep -q "^${CLUSTER}"; then
  sed -e "s|__PWD__|${REPO_ROOT}|g" -e "s|__AGENTS__|${AGENTS}|g" \
    deploy/local/k3d-cluster.yaml > /tmp/k3d-cluster.yaml
  k3d cluster create --config /tmp/k3d-cluster.yaml
fi
kubectl config use-context "k3d-${CLUSTER}"

echo "== 2/8 waiting for nodes =="
kubectl wait --for=condition=Ready nodes --all --timeout=300s
kubectl get nodes

echo "== 3/8 building and pushing the cluster-cache service images =="
# these two images are this project's own code, not part of the corpus, so
# they are rebuilt locally on every cold start instead of round-tripping
# through ACR. Pushed as "localhost:5000", not "registry.localhost:5000": the
# host's docker daemon only trusts 127.0.0.0/8 and ::1/128 as insecure by
# default, and its own resolver does not treat "registry.localhost" as
# loopback the way the host's NSS does, so a push to that name gets refused
# for speaking plain HTTP. The repository name is not hostname-qualified in
# the registry itself, so the k8s manifests still find it under
# registry.localhost:5000, which is what the nodes' containerd needs
docker build -f deploy/Dockerfile -t localhost:5000/nydus-cluster-cache:0.1.0 .
docker build -f deploy/Dockerfile.chunk-proxy -t localhost:5000/chunk-proxy:0.1.0 .
docker push localhost:5000/nydus-cluster-cache:0.1.0
docker push localhost:5000/chunk-proxy:0.1.0

echo "== 4/8 installing and starting the nydus snapshotter =="
./scripts/fetch-nydus-binaries.sh
./scripts/node-snapshotter.sh install
./scripts/node-snapshotter.sh start
./scripts/node-snapshotter.sh status

echo "== 5/8 loading the corpus into the local registry =="
# The ACR that once held the corpus is gone, so the OCI tags come from the
# host docker store and the nydus variants are rebuilt from them. The default
# list is the six-image ladder the cold-start comparison reports; narrow it
# with CORPUS_IMAGES when a campaign only reads part of the corpus, because
# every extra image costs a conversion pass
CORPUS_IMAGES="${CORPUS_IMAGES:-fn-go-hello fn-python-hello fn-java-springboot fn-node-api fn-python-ml fn-pytorch-cpu}"
for img in ${CORPUS_IMAGES}; do
  docker tag "localhost:5000/${img}:oci" "localhost:5000/${img}:latest"
  docker push "localhost:5000/${img}:oci"
  docker push "localhost:5000/${img}:latest"
done
CORPUS_IMAGES="${CORPUS_IMAGES}" "${REPO_ROOT}/scripts/convert-corpus-uncompressed.sh"

echo "== 6/8 deploying the cache service and chunk proxy =="
# applied as explicit files, not "kubectl apply -f deploy/k8s/": that
# directory also holds the aks/ and knative/ and monitoring/ subtrees, which
# a bare -f (no -R) silently skips anyway, and -R would wrongly pull in the
# AKS-only manifests too
kubectl apply -f deploy/k8s/cache-service/deployment.yaml -f deploy/k8s/cache-service/service.yaml
kubectl rollout status deploy/nydus-cluster-cache --timeout=120s
kubectl apply -f deploy/k8s/chunk-proxy/daemonset.yaml
kubectl -n kube-system rollout status ds/chunk-proxy --timeout=300s

echo "== 7/8 installing Knative Serving =="
if kubectl get ns knative-serving >/dev/null 2>&1; then
  echo "knative-serving namespace already exists, skipping install"
else
  kubectl apply -f "https://github.com/knative/serving/releases/download/${KNATIVE_VERSION}/serving-crds.yaml"
  kubectl apply -f "https://github.com/knative/serving/releases/download/${KNATIVE_VERSION}/serving-core.yaml"
  kubectl apply -f "https://github.com/knative/net-kourier/releases/download/${KNATIVE_VERSION}/kourier.yaml"
  kubectl wait --for=condition=Ready pods --all -n knative-serving --timeout=300s
  # must be server-side + force-conflicts: a plain client-side apply nulls the
  # install-owned queue-sidecar-image field and the admission webhook then
  # rejects the config
  kubectl apply --server-side --force-conflicts -f deploy/k8s/knative/serving-config.yaml
  # the upstream install seeds config-deployment with a client-side
  # last-applied-configuration annotation that still holds the null patch;
  # clearing it once stops a non-fatal warning from recurring on every future
  # apply. Guarded by the namespace check above: re-running the client-side
  # serving-core.yaml apply against an object the server-side apply above
  # already owns resets this annotation and makes the *next* server-side
  # apply fail outright instead of warning, because kubectl's SSA and
  # non-SSA code paths default to different field-manager names and start
  # fighting over ownership of the same map
  kubectl -n knative-serving annotate configmap config-deployment \
    kubectl.kubernetes.io/last-applied-configuration- --overwrite >/dev/null 2>&1 || true
fi

echo "== 8/8 switching to configuration 4 and clearing any leftover shaping =="
# composes the global containerd snapshotter drop-in, a one-at-a-time node
# cycle (k3d v5 has no "node restart", and a multi-node stop/start only acts
# on the first node), restarting the host-process snapshotter (it does not
# survive a node restart, the k3d nodes have no service manager), and the
# chunk-proxy cache toggle
./scripts/config-switch.sh 4
# netem lives in the registry container's namespace, so a fresh cluster
# never has it, but a re-run against a rig that still has it from a previous
# session must not start a campaign already shaped
./scripts/registry-netem.sh off || true
./scripts/registry-netem.sh status

echo "rig ready: configuration 4, netem off"
