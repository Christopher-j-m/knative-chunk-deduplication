#!/usr/bin/env bash
# Runs the full overlap analysis: builds and converts the corpus, then measures the chunk overlap
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="${SCRIPT_DIR}/../.."

echo "Start local registry..."
cd "$PROJECT_DIR"
if ! curl -fs "http://localhost:5000/v2/" >/dev/null; then
  docker run -d --name registry -p 5000:5000 registry:3.1.1
fi

echo "Build images..."
bash scripts/corpus/build-corpus.sh

echo "Capture corpus versions..."
bash scripts/corpus/capture-corpus-versions.sh

echo "Convert to Nydus format..."
bash scripts/corpus/convert-corpus.sh

echo "Capture corpus versions (after conversion)"
bash scripts/corpus/capture-corpus-versions.sh
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
bash scripts/overlap/export-filesystems.sh "${IMAGES[@]}"

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
pip install -q -r scripts/overlap/requirements.txt
python scripts/overlap/visualize.py analysis/results/overlap-full.json analysis/results/figures/full
python scripts/overlap/visualize.py analysis/results/overlap-workload.json analysis/results/figures/workload
python scripts/overlap/visualize.py analysis/results/overlap-full-fixed.json analysis/results/figures/fixed-full
python scripts/overlap/visualize.py analysis/results/overlap-workload-fixed.json analysis/results/figures/fixed-workload

echo "Results saved to:"
echo "  analysis/results/chunks.json                        (chunk metadata + compressed sizes)"
echo "  analysis/results/chunks-layers.json                 (OCI + Nydus blob/layer digests)"
echo "  analysis/results/overlap-full.json                  (all images, Nydus chunking + blob overlap)"
echo "  analysis/results/overlap-workload.json              (knative-workload only, Nydus + blob overlap)"
echo "  analysis/results/overlap-full-fixed.json            (all images, lambda-style 512 KiB)"
echo "  analysis/results/overlap-workload-fixed.json        (knative-workload, lambda-style 512 KiB)"
echo "  analysis/results/overlap-full-comparison.json       (Nydus vs fixed strategy summary, full)"
echo "  analysis/results/overlap-workload-comparison.json   (Nydus vs fixed strategy summary, workload)"
echo "  analysis/corpus-versions.json                       (tool versions and image digests of the corpus)"
echo "Figures saved to 'analysis/results/figures/'"
