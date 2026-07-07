#!/usr/bin/env bash
set -euo pipefail

# Renders the Knative workload manifest for one corpus function to stdout.
# Usage: ACR=<reg> SERVICE=<image> TAG=<oci|nydus-none> [PORT=8080] render-workload.sh
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACR="${ACR:?set ACR to the ACR login server}"
SERVICE="${SERVICE:?set SERVICE to the function image name}"
TAG="${TAG:?set TAG to oci or nydus-none}"
PORT="${PORT:-8080}"

sed -e "s|__ACR__|${ACR}|g" \
    -e "s|__SERVICE__|${SERVICE}|g" \
    -e "s|__TAG__|${TAG}|g" \
    -e "s|__PORT__|${PORT}|g" \
  "${REPO_ROOT}/deploy/k8s/aks/workload-template.yaml"
