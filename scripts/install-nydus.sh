#!/usr/bin/env bash
set -euo pipefail

NYDUS_VERSION="v2.4.1"
ARCH="$(uname -m)"

case "${ARCH}" in
  x86_64) ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: ${ARCH}" >&2
    exit 1
    ;;
esac

echo "Installing Nydus ${NYDUS_VERSION}..."

TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

curl -fsSL "https://github.com/dragonflyoss/nydus/releases/download/${NYDUS_VERSION}/nydus-static-${NYDUS_VERSION}-linux-${ARCH}.tgz" \
  -o "$TMPDIR/nydus.tgz"

tar -xzf "$TMPDIR/nydus.tgz" -C "$TMPDIR"

sudo install -m 755 "$TMPDIR/nydus-static/nydusify" /usr/local/bin/
sudo install -m 755 "$TMPDIR/nydus-static/nydus-image" /usr/local/bin/

echo "Installed:"
nydusify --version
nydus-image --version
