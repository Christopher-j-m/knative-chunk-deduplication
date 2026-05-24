#!/usr/bin/env bash
set -euo pipefail

REGISTRY="localhost:5000"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="${SCRIPT_DIR}/.."
OUTPUT="${PROJECT_DIR}/analysis/provenance.json"

# Collect tool versions
NYDUS_VERSION=$(nydusify --version 2>&1 | head -1 || echo "null")
NYDUS_IMAGE_VERSION=$(nydus-image --version 2>&1 | head -1 || echo "null")
GO_VERSION=$(go version | awk '{print $3}')
DOCKER_VERSION=$(docker --version | awk '{print $3}' | tr -d ',')
SCRIPT_GIT_REV=$(git -C "$PROJECT_DIR" rev-parse HEAD 2>/dev/null || echo "null")

# All images to capture
IMAGES=(
  "python-3.14-slim" "python-3.13-slim"
  "node-26-slim" "node-24-slim"
  "golang-1.26-alpine" "rust-1.95-slim"
  "node-express" "node-fastify"
  "fn-python-hello" "fn-python-ml" "fn-python-api"
  "fn-node-hello" "fn-node-api"
  "fn-go-hello" "fn-rust-hello"
  "fn-java-hello" "fn-java-springboot"
  "fn-tf-serving" "fn-pytorch-cpu"
  "tensorflow-gpu" "pytorch-cuda"
)

{
  echo "{"
  echo "  \"captured_at\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\","
  echo "  \"tool_versions\": {"
  echo "    \"nydusify\": \"${NYDUS_VERSION}\","
  echo "    \"nydus_image\": \"${NYDUS_IMAGE_VERSION}\","
  echo "    \"go\": \"${GO_VERSION}\","
  echo "    \"docker\": \"${DOCKER_VERSION}\""
  echo "  },"
  echo "  \"script_git_revision\": \"${SCRIPT_GIT_REV}\","
  echo "  \"platform\": \"$(uname -m)\","
  echo "  \"images\": ["

  FIRST=true
  for img in "${IMAGES[@]}"; do
    if [ "$FIRST" = true ]; then FIRST=false; else echo ","; fi

    # Post-build: OCI image digest
    OCI_DIGEST=$(docker inspect --format='{{index .RepoDigests 0}}' "${REGISTRY}/${img}:latest" 2>/dev/null || echo "null")

    # Post-conversion: Nydus image digest
    NYDUS_DIGEST=$(docker inspect --format='{{index .RepoDigests 0}}' "${REGISTRY}/${img}:nydus" 2>/dev/null || echo "null")

    # Nydus bootstrap digest (if available)
    BOOTSTRAP_DIGEST="null"
    if [ -f "/tmp/chunk-inspector/${img}/bootstrap" ]; then
      BOOTSTRAP_DIGEST="sha256:$(sha256sum "/tmp/chunk-inspector/${img}/bootstrap" | awk '{print $1}')"
    fi

    printf "    {\"id\": \"%s\", \"oci_digest\": \"%s\", \"nydus_digest\": \"%s\", \"bootstrap_digest\": \"%s\", \"conversion_timestamp\": null}" \
      "$img" "$OCI_DIGEST" "$NYDUS_DIGEST" "$BOOTSTRAP_DIGEST"
  done

  echo ""
  echo "  ]"
  echo "}"
} > "$OUTPUT"

echo "Provenance captured to ${OUTPUT}"
