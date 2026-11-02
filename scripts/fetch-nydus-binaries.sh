#!/usr/bin/env bash
# Download nydus binaries (nydus image, nydus daemon, nydus snapshotter) locally to copy it later on onto each cluster node (aks & local)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/deploy/local/bin"
SNAPSHOTTER_VERSION="v0.15.0"
NYDUS_VERSION="2.3.2"
mkdir -p "${OUT_DIR}"

if [[ -x "${OUT_DIR}/nydusd" && -x "${OUT_DIR}/nydus-image" && -x "${OUT_DIR}/containerd-nydus-grpc" ]]; then
  echo "Binaries already in ${OUT_DIR} so nothing to do..."
  exit 0
fi

echo "Downloading nydusd and nydus-image"
RELEASE_ZIP="$(mktemp -d)/nydus-static.zip"
curl -fsSL -o "${RELEASE_ZIP}" "https://github.com/dragonflyoss/nydus/releases/download/v${NYDUS_VERSION}/nydus-static-${NYDUS_VERSION}-linux-amd64.zip"
unzip -j -o "${RELEASE_ZIP}" "nydusd" "nydus-image" -d "${OUT_DIR}"
rm -rf "$(dirname "${RELEASE_ZIP}")"

# Instead of using a fork for the nydus snapshotter we just use a small git patch to add http backend support for our cluster cache proxy
echo "Building containerd-nydus-grpc with http-proxy backend support"
docker run --rm -v "${OUT_DIR}:/out" \
  -v "${REPO_ROOT}/deploy/local/snapshotter/http-proxy-backend.patch:/http-proxy-backend.patch:ro" \
  golang:1.23-bookworm sh -c \
  "git clone --depth 1 --branch ${SNAPSHOTTER_VERSION} https://github.com/containerd/nydus-snapshotter /src \
   && cd /src && git apply /http-proxy-backend.patch \
   && CGO_ENABLED=0 go build -ldflags '-s -w' -o /out/containerd-nydus-grpc ./cmd/containerd-nydus-grpc \
   && chown $(id -u):$(id -g) /out/containerd-nydus-grpc"

chmod +x "${OUT_DIR}"/nydusd "${OUT_DIR}"/nydus-image "${OUT_DIR}"/containerd-nydus-grpc

# Linked binaries cause an error, which doesnt seem to be easily fixable => just use static binaries that carry all of its dependencies itself
for binary in nydusd nydus-image containerd-nydus-grpc; do
  if file "${OUT_DIR}/${binary}" | grep -q "dynamically linked"; then
    echo "${binary} is dynamically linked" >&2
    exit 1
  fi
done
