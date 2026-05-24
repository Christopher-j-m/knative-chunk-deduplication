#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="${SCRIPT_DIR}/.."

echo "Start local registry..."
cd "$PROJECT_DIR"
docker compose up -d

echo "Build images..."
bash scripts/build-corpus.sh

echo "Capture provenance..."
bash scripts/capture-provenance.sh

echo "Convert to Nydus format..."
bash scripts/convert-corpus.sh

echo "Capture provenance (after conversion)"
bash scripts/capture-provenance.sh
make build

echo "Extract chunk metadata & layer data..."
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
)

./bin/chunk-inspector --extract-layers --output analysis/results/chunks.json "${IMAGES[@]}"

echo "Export filesystems for fixed-chunk comparison..."
bash scripts/export-filesystems.sh "${IMAGES[@]}"

echo "Running overlap analysis (Nydus chunking)..."
./bin/overlap-analyzer \
  --input analysis/results/chunks.json \
  --corpus analysis/corpus.json \
  --blob-overlap \
  --output analysis/results/overlap-full.json

echo "Running overlap analysis (workload images only)..."
./bin/overlap-analyzer \
  --input analysis/results/chunks.json \
  --corpus analysis/corpus.json \
  --filter-role=knative-workload \
  --blob-overlap \
  --output analysis/results/overlap-workload.json

echo "Running fixed-chunk comparison..."
./bin/overlap-analyzer \
  --input analysis/results/chunks.json \
  --corpus analysis/corpus.json \
  --compare-fixed \
  --export-dir analysis/results/filesystems \
  --fixed-output analysis/results/overlap-full-fixed.json \
  --comparison-output analysis/results/overlap-full-comparison.json

./bin/overlap-analyzer \
  --input analysis/results/chunks.json \
  --corpus analysis/corpus.json \
  --filter-role=knative-workload \
  --compare-fixed \
  --export-dir analysis/results/filesystems \
  --fixed-output analysis/results/overlap-workload-fixed.json \
  --comparison-output analysis/results/overlap-workload-comparison.json

echo "Generate result figures for analysis..."
pip install -q -r scripts/requirements.txt
python scripts/visualize.py analysis/results/overlap-full.json analysis/results/figures/full
python scripts/visualize.py analysis/results/overlap-workload.json analysis/results/figures/workload
python scripts/visualize.py analysis/results/overlap-full-fixed.json analysis/results/figures/fixed-full
python scripts/visualize.py analysis/results/overlap-workload-fixed.json analysis/results/figures/fixed-workload

echo "Results saved to:"
echo "  analysis/results/chunks.json                        (chunk metadata + compressed sizes)"
echo "  analysis/results/chunks-layers.json                 (OCI + Nydus blob/layer digests)"
echo "  analysis/results/overlap-full.json                  (all images, Nydus chunking + blob overlap)"
echo "  analysis/results/overlap-workload.json              (knative-workload only, Nydus + blob overlap)"
echo "  analysis/results/overlap-full-fixed.json            (all images, lambda-style 512 KiB)"
echo "  analysis/results/overlap-workload-fixed.json        (knative-workload, lambda-style 512 KiB)"
echo "  analysis/results/overlap-full-comparison.json       (Nydus vs fixed strategy summary, full)"
echo "  analysis/results/overlap-workload-comparison.json   (Nydus vs fixed strategy summary, workload)"
echo "  analysis/provenance.json                            (trace file for reproducibility)"
echo "Figures saved to 'analysis/results/figures/'"
