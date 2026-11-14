#!/usr/bin/env bash
# Converts the corpus images to uncompressed Nydus images, which the cluster cache needs
set -euo pipefail

REGISTRY="localhost:5000"

IMAGES=(
  "python-3.14-slim" "python-3.13-slim" "node-26-slim" "node-24-slim"
  "golang-1.26-alpine" "rust-1.95-slim" "node-express" "node-fastify"
  "fn-python-hello" "fn-python-ml" "fn-python-api" "fn-node-hello"
  "fn-node-api" "fn-go-hello" "fn-rust-hello" "fn-java-hello"
  "fn-java-springboot" "fn-tf-serving" "fn-pytorch-cpu"
  "tensorflow-gpu" "pytorch-cuda"
)

if [[ -n "${CORPUS_IMAGES:-}" ]]; then
  read -r -a IMAGES <<<"${CORPUS_IMAGES}"
fi

echo "Converting images to uncompressed Nydus format"
for img in "${IMAGES[@]}"; do
  echo "Converting ${img} (compressor=none)..."
  nydusify convert \
    --source "${REGISTRY}/${img}:latest" \
    --target "${REGISTRY}/${img}:nydus-none" \
    --compressor none
done

echo "Done converting all images to uncompressed Nydus format"
