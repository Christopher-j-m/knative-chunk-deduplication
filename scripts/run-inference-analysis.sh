#!/usr/bin/env bash
set -euo pipefail

# Overlap analysis for the LLM-inference-servers (vLLM, NVIDIA Triton, Hugging Face TGI).
# Pulls these 3 servers , converts them to Nydus format, and measures chunk overlap.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="${SCRIPT_DIR}/.."
cd "$PROJECT_DIR"

REGISTRY="localhost:5000"

# Map image id -> upstream source reference
declare -A IMAGES=(
  ["vllm-openai"]="vllm/vllm-openai:v0.21.0-cu129-ubuntu2404"
  ["triton-server"]="nvcr.io/nvidia/tritonserver:26.04-py3"
  ["tgi"]="ghcr.io/huggingface/text-generation-inference:3.3.4"
)

echo "Start local registry"
docker compose up -d
sleep 5

echo "Build tools"
make build

echo "Pulling inference-server images & push to local registry"
for id in "${!IMAGES[@]}"; do
  src="${IMAGES[$id]}"
  echo "Pulling ${src} -> ${id}..."
  docker pull "$src"
  docker tag "$src" "${REGISTRY}/${id}:latest"
  docker push "${REGISTRY}/${id}:latest"
done

echo "Converting to Nydus format..."
for id in "${!IMAGES[@]}"; do
  echo "Converting ${id}..."
  nydusify convert \
    --source "${REGISTRY}/${id}:latest" \
    --target "${REGISTRY}/${id}:nydus"
done

echo "Extract chunk metadata + layer baselines"
./bin/chunk-inspector --extract-layers \
  --output analysis/results/chunks-inference.json \
  vllm-openai triton-server tgi

echo "Run overlap analysis..."
./bin/overlap-analyzer \
  --input analysis/results/chunks-inference.json \
  --corpus analysis/corpus-inference.json \
  --blob-overlap \
  --output analysis/results/overlap-inference.json

echo "Results saved to:"
echo "  analysis/results/chunks-inference.json    (chunk metadata + OCI/Nydus layer baselines)"
echo "  analysis/results/overlap-inference.json   (corpus metrics, pairwise overlap, blob baseline)"
