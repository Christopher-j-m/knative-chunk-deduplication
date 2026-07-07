#!/usr/bin/env bash
set -euo pipefail

# Pushes the two service images and the two corpus variants to ACR.
# The service images are rebuilt locally; the corpus variants are copied
# from the local registry with skopeo because the Nydus variant's
# layer media types must arrive unmodified (docker push would not do).
# Usage: ACR=<name>.azurecr.io deploy/aks/push-images.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ACR="${ACR:?set ACR to the ACR login server, e.g. <registry>.azurecr.io}"
LOCAL_REG="localhost:5000"

az acr login --name "${ACR%%.*}"

echo "Building and pushing service images"
docker build -f "${REPO_ROOT}/deploy/Dockerfile" -t "${ACR}/nydus-cluster-cache:0.1.0" "${REPO_ROOT}"
docker build -f "${REPO_ROOT}/deploy/Dockerfile.chunk-proxy" -t "${ACR}/chunk-proxy:0.1.0" "${REPO_ROOT}"
docker push "${ACR}/nydus-cluster-cache:0.1.0"
docker push "${ACR}/chunk-proxy:0.1.0"

# fn-pytorch-cpu replaced fn-tf-serving as the largest image: the
# tf-serving corpus build ships an empty model directory and can never
# become ready, while the pytorch handler builds its model in code
CORPUS_IMAGES=(
  "fn-go-hello" "fn-python-hello" "fn-java-springboot"
  "fn-node-api" "fn-python-ml" "fn-pytorch-cpu"
)

echo "Copying the corpus from the local registry"
for img in "${CORPUS_IMAGES[@]}"; do
  for tag in oci nydus-none; do
    skopeo copy --all --src-tls-verify=false --dest-authfile "${HOME}/.docker/config.json" \
      "docker://${LOCAL_REG}/${img}:${tag}" \
      "docker://${ACR}/${img}:${tag}"
  done
done

# Anonymous pull on ACR still runs through the bearer token flow: bare
# requests get a challenge and the token endpoint grants pull tokens without
# credentials, so this check does the same dance the chunk-proxy does
echo "Verifying anonymous pull (no credentials)"
for img in "${CORPUS_IMAGES[@]}"; do
  token=$(curl -s "https://${ACR}/oauth2/token?service=${ACR}&scope=repository:${img}:pull" \
    | sed -n 's/.*"access_token" *: *"\([^"]*\)".*/\1/p')
  [[ -n "${token}" ]] || { echo "FAIL: no anonymous token for ${img}" >&2; exit 1; }
  code=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${token}" \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
    "https://${ACR}/v2/${img}/manifests/nydus-none")
  [[ "${code}" == "200" ]] || { echo "FAIL: anonymous manifest GET for ${img} returned ${code}" >&2; exit 1; }
done
echo "Anonymous pull OK"
