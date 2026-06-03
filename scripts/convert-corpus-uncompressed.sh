#!/usr/bin/env bash
set -euo pipefail

REGISTRY="localhost:5000"

# The cluster cache serves uncompressed chunk bytes, so the Nydus blobs must be
# built with no compression: a blob's bytes for a chunk then equal the chunk's
# content digest, which is what the cache stores.
IMAGES=(
  "python-3.14-slim" "python-3.13-slim" "node-26-slim" "node-24-slim"
  "golang-1.26-alpine" "rust-1.95-slim" "node-express" "node-fastify"
  "fn-python-hello" "fn-python-ml" "fn-python-api" "fn-node-hello"
  "fn-node-api" "fn-go-hello" "fn-rust-hello" "fn-java-hello"
  "fn-java-springboot" "fn-tf-serving" "fn-pytorch-cpu"
  "tensorflow-gpu" "pytorch-cuda"
)

echo "Converting images to uncompressed Nydus format"
for img in "${IMAGES[@]}"; do
  echo "Converting ${img} (compressor=none)..."
  nydusify convert \
    --source "${REGISTRY}/${img}:latest" \
    --target "${REGISTRY}/${img}:nydus-none" \
    --compressor none
done

echo "Done converting all images to uncompressed Nydus format"
