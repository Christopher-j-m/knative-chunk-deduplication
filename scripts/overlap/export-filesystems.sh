#!/usr/bin/env bash
# Unpacks the filesystem of each given image from the local registry into the analysis results folder
set -euo pipefail
REGISTRY="localhost:5000"
EXPORT_DIR="analysis/results/filesystems"
mkdir -p "$EXPORT_DIR"

IMAGES=("$@")
for img in "${IMAGES[@]}"; do
  echo "Exporting ${img}..."
  rm -rf "$EXPORT_DIR/${img}"
  mkdir -p "$EXPORT_DIR/${img}"
  container_id=$(docker create "${REGISTRY}/${img}:latest" /bin/true)
  docker export "$container_id" | tar -C "$EXPORT_DIR/${img}" -xf -
  docker rm "$container_id" > /dev/null
done
echo "Exported ${#IMAGES[@]} images to $EXPORT_DIR/"
