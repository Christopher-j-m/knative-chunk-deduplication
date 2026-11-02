#!/usr/bin/env bash
set -euo pipefail

# Push the dummy images to acr

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ACR="${ACR:?set ACR to the ACR login server, e.g. <registry>.azurecr.io}"
LOCAL_REG="localhost:5000"

az acr login --name "${ACR%%.*}"

echo "Building and pushing the cluster cache and chunk procy images"
docker build -f "${REPO_ROOT}/deploy/Dockerfile" -t "${ACR}/nydus-cluster-cache:0.1.0" "${REPO_ROOT}"
docker build -f "${REPO_ROOT}/deploy/Dockerfile.chunk-proxy" -t "${ACR}/chunk-proxy:0.1.0" "${REPO_ROOT}"
docker push "${ACR}/nydus-cluster-cache:0.1.0"
docker push "${ACR}/chunk-proxy:0.1.0"

CORPUS_IMAGES=(
  "fn-go-hello" "fn-python-hello" "fn-java-springboot"
  "fn-node-api" "fn-python-ml" "fn-pytorch-cpu"
)

# Copy all dummy images from local registry to acr in oci and nydus versions
echo "Copying the dummy images to acr"
for img in "${CORPUS_IMAGES[@]}"; do
  for tag in oci nydus-none; do
    skopeo copy --all --src-tls-verify=false --dest-authfile "${HOME}/.docker/config.json" \
      "docker://${LOCAL_REG}/${img}:${tag}" \
      "docker://${ACR}/${img}:${tag}"
  done
done

echo "Check if we can pull all images without password"
for img in "${CORPUS_IMAGES[@]}"; do
  token=$(curl -s "https://${ACR}/oauth2/token?service=${ACR}&scope=repository:${img}:pull" \
    | sed -n 's/.*"access_token" *: *"\([^"]*\)".*/\1/p')
  [[ -n "${token}" ]] || { echo "error for no anonymous token for ${img}" >&2; exit 1; }
  code=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${token}" \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
    "https://${ACR}/v2/${img}/manifests/nydus-none")
  [[ "${code}" == "200" ]] || { echo "error for anonymous manifest GET for ${img} returned ${code}" >&2; exit 1; }
done
