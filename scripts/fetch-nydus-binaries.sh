#!/usr/bin/env bash
set -euo pipefail

# Collects the nydus binaries that get installed onto the cluster nodes into
# deploy/local/bin. The k3d nodes are busybox containers without a dynamic
# loader, so every binary must be statically linked: nydusd and nydus-image
# come from the upstream image (already static), containerd-nydus-grpc is
# built from source with CGO disabled (the release binary needs glibc) and
# with a small patch that teaches its config handling the http-proxy storage
# backend the chunk-proxy relies on; stock v0.15.0 rejects it at container
# creation with "unknown backend type http-proxy"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/deploy/local/bin"
SNAPSHOTTER_VERSION="v0.15.0"
mkdir -p "${OUT_DIR}"

if [[ -x "${OUT_DIR}/nydusd" && -x "${OUT_DIR}/nydus-image" && -x "${OUT_DIR}/containerd-nydus-grpc" ]]; then
  echo "Binaries already present in ${OUT_DIR}; delete them to re-fetch"
  exit 0
fi

echo "Extracting nydusd and nydus-image from the upstream image"
SOURCE_CONTAINER="$(docker create "ghcr.io/containerd/nydus-snapshotter:${SNAPSHOTTER_VERSION}")"
docker cp "${SOURCE_CONTAINER}:/opt/nydus-artifacts/usr/local/bin/nydusd" "${OUT_DIR}/nydusd"
docker cp "${SOURCE_CONTAINER}:/opt/nydus-artifacts/usr/local/bin/nydus-image" "${OUT_DIR}/nydus-image"
docker rm "${SOURCE_CONTAINER}" >/dev/null

echo "Building a static containerd-nydus-grpc with http-proxy backend support"
docker run --rm -v "${OUT_DIR}:/out" \
  -v "${REPO_ROOT}/deploy/local/snapshotter/http-proxy-backend.patch:/http-proxy-backend.patch:ro" \
  golang:1.23-bookworm sh -c \
  "git clone --depth 1 --branch ${SNAPSHOTTER_VERSION} https://github.com/containerd/nydus-snapshotter /src \
   && cd /src && git apply /http-proxy-backend.patch \
   && CGO_ENABLED=0 go build -ldflags '-s -w' -o /out/containerd-nydus-grpc ./cmd/containerd-nydus-grpc \
   && chown $(id -u):$(id -g) /out/containerd-nydus-grpc"

chmod +x "${OUT_DIR}"/nydusd "${OUT_DIR}"/nydus-image "${OUT_DIR}"/containerd-nydus-grpc

# A dynamically linked binary would fail on the nodes with a cryptic exec error,
# so refuse to finish if any of them needs a loader
for binary in nydusd nydus-image containerd-nydus-grpc; do
  if file "${OUT_DIR}/${binary}" | grep -q "dynamically linked"; then
    echo "FAIL: ${binary} is dynamically linked and cannot run on the k3d nodes" >&2
    exit 1
  fi
done
echo "All binaries in ${OUT_DIR} are static"
