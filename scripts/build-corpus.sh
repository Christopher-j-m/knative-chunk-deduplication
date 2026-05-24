#!/usr/bin/env bash
set -euo pipefail

REGISTRY="localhost:5000"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGES_DIR="${SCRIPT_DIR}/../images"

declare -A BASE_IMAGES=(
  ["python:3.14-slim"]="python-3.14-slim"
  ["python:3.13-slim"]="python-3.13-slim"
  ["node:26-slim"]="node-26-slim"
  ["node:24-slim"]="node-24-slim"
  ["golang:1.26-alpine"]="golang-1.26-alpine"
  ["rust:1.95-slim"]="rust-1.95-slim"
  ["tensorflow/tensorflow:2.19.0-gpu"]="tensorflow-gpu"
  ["pytorch/pytorch:2.7.0-cuda12.6-cudnn9-runtime"]="pytorch-cuda"
)

echo "Pulling base images"
for source_ref in "${!BASE_IMAGES[@]}"; do
  id="${BASE_IMAGES[$source_ref]}"
  echo "Pulling ${source_ref} -> ${id}..."
  docker pull "${source_ref}"
  docker tag "${source_ref}" "${REGISTRY}/${id}:latest"
  docker push "${REGISTRY}/${id}:latest"
done

# Custom images — build from Dockerfiles
CUSTOM_IMAGES=(
  "node-express"
  "node-fastify"
  "fn-python-hello"
  "fn-python-ml"
  "fn-python-api"
  "fn-node-hello"
  "fn-node-api"
  "fn-go-hello"
  "fn-rust-hello"
  "fn-java-hello"
  "fn-java-springboot"
  "fn-tf-serving"
  "fn-pytorch-cpu"
)

echo "Building images"
for img in "${CUSTOM_IMAGES[@]}"; do
  echo "Building ${img}..."
  docker build -t "${REGISTRY}/${img}:latest" "${IMAGES_DIR}/${img}"
  docker push "${REGISTRY}/${img}:latest"
done

echo "All images built and pushed to ${REGISTRY}"
