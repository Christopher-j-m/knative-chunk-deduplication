#!/usr/bin/env bash
set -euo pipefail

# Collects the nydus binaries that get installed onto the cluster nodes into
# deploy/local/bin. The k3d nodes are busybox containers without a dynamic
# loader, so every binary must be statically linked: nydusd and nydus-image
# come from the upstream static release, containerd-nydus-grpc is built from
# source with CGO disabled (the release binary needs glibc) and with a small
# patch that teaches its config handling the http-proxy storage backend the
# chunk-proxy relies on; stock v0.15.0 rejects it at container creation with
# "unknown backend type http-proxy".
# nydusd must be v2.3.2 or newer: the registry backend used against Azure
# Container Registry needs the token parsing fix from
# dragonflyoss/nydus#1689, because ACR answers the bearer challenge with an
# access_token field and older daemons fail every read with
# "missing field token"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/deploy/local/bin"
SNAPSHOTTER_VERSION="v0.15.0"
NYDUS_VERSION="2.3.2"
mkdir -p "${OUT_DIR}"

if [[ -x "${OUT_DIR}/nydusd" && -x "${OUT_DIR}/nydus-image" && -x "${OUT_DIR}/containerd-nydus-grpc" ]]; then
  echo "Binaries already present in ${OUT_DIR}; delete them to re-fetch"
  exit 0
fi

echo "Downloading nydusd and nydus-image ${NYDUS_VERSION} from the upstream release"
RELEASE_ZIP="$(mktemp -d)/nydus-static.zip"
curl -fsSL -o "${RELEASE_ZIP}" \
  "https://github.com/dragonflyoss/nydus/releases/download/v${NYDUS_VERSION}/nydus-static-${NYDUS_VERSION}-linux-amd64.zip"
unzip -j -o "${RELEASE_ZIP}" "nydusd" "nydus-image" -d "${OUT_DIR}"
rm -rf "$(dirname "${RELEASE_ZIP}")"

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
