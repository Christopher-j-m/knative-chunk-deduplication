#!/usr/bin/env bash
# Prints the Knative service manifest for one corpus function
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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
