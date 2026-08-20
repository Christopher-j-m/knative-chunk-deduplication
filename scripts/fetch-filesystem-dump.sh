#!/usr/bin/env bash
# Fetches one unpacked image tree from the archive container.
#
# The dumps under analysis/results/filesystems are the raw evidence behind the
# chapter 4 overlap numbers. They are not in git (5.8 GB) and they cannot be
# regenerated: analysis/corpus.json names its images by mutable tag with no
# digest pinning, so re-pulling today yields different content than the thesis
# reports. They live in blob storage instead, one tarball per image.
#
#   ./scripts/fetch-filesystem-dump.sh              # list what is available
#   ./scripts/fetch-filesystem-dump.sh fn-python-ml # fetch one tree
set -euo pipefail

SA="${ARCHIVE_ACCOUNT:-<storage-account>}"
CONTAINER=filesystems
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="${DEST:-${REPO_ROOT}/analysis/results/filesystems}"

# On the VM the managed identity carries Storage Blob Data Reader, so no key is
# needed. Elsewhere, export ARCHIVE_KEY to fall back to shared-key auth.
if [[ -n "${ARCHIVE_KEY:-}" ]]; then
  auth=(--account-key "${ARCHIVE_KEY}")
else
  auth=(--auth-mode login)
fi

if [[ $# -eq 0 ]]; then
  echo "available image trees:"
  az storage blob list --account-name "${SA}" -c "${CONTAINER}" "${auth[@]}" \
    --query "[].{name:name,MB:properties.contentLength}" -o tsv |
    awk '{printf "  %-26s %d MB\n", gensub(/\.tar\.gz$/,"",1,$1), $2/1048576}'
  echo
  echo "fetch one with: $0 <name>"
  exit 0
fi

name="$1"
tgz="$(mktemp -d)/${name}.tar.gz"

echo "fetching ${name} ..."
az storage blob download --account-name "${SA}" -c "${CONTAINER}" \
  -n "${name}.tar.gz" -f "${tgz}" "${auth[@]}" -o none

mkdir -p "${DEST}"
tar xzf "${tgz}" -C "${DEST}"
rm -rf "$(dirname "${tgz}")"

echo "extracted to ${DEST}/${name}"
