#!/usr/bin/env bash
set -euo pipefail

# Renders the Knative workload manifest for one corpus function to stdout.
# Usage: ACR=<reg> SERVICE=<image> TAG=<oci|nydus-none> [PORT=8080] [NAME=<service>] [INITIAL_SCALE=<n>] render-workload.sh
# NAME lets several services run the same image under different names, the
# way a trace replay maps many applications onto one corpus image; it
# defaults to the image name. INITIAL_SCALE sets the pods a new revision
# starts with; 0 needs allow-zero-initial-scale in the autoscaler config
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACR="${ACR:?set ACR to the ACR login server}"
SERVICE="${SERVICE:?set SERVICE to the function image name}"
TAG="${TAG:?set TAG to oci or nydus-none}"
PORT="${PORT:-8080}"
NAME="${NAME:-${SERVICE}}"
INITIAL_SCALE="${INITIAL_SCALE:-1}"

sed -e "s|__ACR__|${ACR}|g" \
    -e "s|__NAME__|${NAME}|g" \
    -e "s|__SERVICE__|${SERVICE}|g" \
    -e "s|__TAG__|${TAG}|g" \
    -e "s|__PORT__|${PORT}|g" \
    -e "s|__INITIAL_SCALE__|${INITIAL_SCALE}|g" \
  "${REPO_ROOT}/deploy/k8s/aks/workload-template.yaml"
