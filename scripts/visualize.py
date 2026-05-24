#!/usr/bin/env python3
"""Generate figures from overlap analysis results."""

import json
import sys
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np
import seaborn as sns

def load_results(path: str) -> dict:
    with open(path) as f:
        return json.load(f)


def plot_pairwise_heatmap(results: dict, output_dir: Path):
    """Heatmap of pairwise chunk overlap between images."""
    pairs = results["pairwise_overlap"]
    images = sorted(set(p["image_a"] for p in pairs) | set(p["image_b"] for p in pairs))
    n = len(images)
    idx = {name: i for i, name in enumerate(images)}

    matrix = np.zeros((n, n))
    for p in pairs:
        i, j = idx[p["image_a"]], idx[p["image_b"]]
        val = p["shared_bytes"] / (1024 * 1024)
        matrix[i][j] = val
        matrix[j][i] = val

    fig, ax = plt.subplots(figsize=(12, 10))
    sns.heatmap(matrix, xticklabels=images, yticklabels=images,
                annot=True, fmt=".0f", cmap="YlOrRd", ax=ax)
    ax.set_title("Pairwise Chunk Overlap (MB shared)")
    plt.xticks(rotation=45, ha="right")
    plt.yticks(rotation=0)
    plt.tight_layout()
    plt.savefig(output_dir / "heatmap_overlap.pdf", dpi=300)
    plt.close()


def plot_coverage_curves(results: dict, output_dir: Path):
    """Coverage curves across scenarios (byte hit rate band)."""
    scenarios = results.get("coverage_scenarios", [])
    if not scenarios:
        return

    fig, ax = plt.subplots(figsize=(12, 6))
    colors = plt.cm.tab10(np.linspace(0, 1, len(scenarios)))

    for i, scenario in enumerate(scenarios):
        curve = scenario["curve"]
        indices = list(range(len(curve)))
        byte_rates = [p["potential_hit_rate_bytes"] * 100 for p in curve]
        ax.plot(indices, byte_rates, marker="o", color=colors[i],
                label=scenario["name"], linewidth=2, markersize=4)

    ax.set_ylabel("Ideal Cache Hit Rate Upper Bound — Bytes (%)")
    ax.set_xlabel("Image index (order of arrival)")
    ax.set_title("Coverage Curves by Workload Scenario")
    ax.set_ylim(0, 100)
    ax.legend(loc="lower right")
    ax.grid(True, alpha=0.3)
    plt.tight_layout()
    plt.savefig(output_dir / "coverage_curves_scenarios.pdf", dpi=300)
    plt.close()


def plot_frequency_cdf(results: dict, output_dir: Path):
    """CDF of chunk reference counts."""
    freqs = results["chunk_frequencies"]
    ref_counts = sorted([f["ref_count"] for f in freqs])

    fig, ax = plt.subplots(figsize=(8, 5))
    cdf_y = np.arange(1, len(ref_counts) + 1) / len(ref_counts)
    ax.step(ref_counts, cdf_y, where="post", color="darkgreen")
    ax.set_xlabel("Number of images referencing chunk")
    ax.set_ylabel("CDF (fraction of chunks)")
    ax.set_title("Chunk Reference Count Distribution")
    ax.grid(True, alpha=0.3)
    plt.tight_layout()
    plt.savefig(output_dir / "frequency_cdf.pdf", dpi=300)
    plt.close()


def plot_dedup_summary(results: dict, output_dir: Path):
    """Bar chart - total vs unique vs redundant bytes, both dedup ratios."""
    m = results["corpus_metrics"]
    total_mb = m["total_bytes"] / (1024 * 1024)
    unique_mb = m["unique_bytes"] / (1024 * 1024)
    redundant_mb = m["redundant_bytes"] / (1024 * 1024)

    fig, ax = plt.subplots(figsize=(7, 5))
    bars = ax.bar(["Total", "Unique", "Redundant"],
                  [total_mb, unique_mb, redundant_mb],
                  color=["#4a90d9", "#50c878", "#e74c3c"])
    ax.set_ylabel("Size (MB)")
    ax.set_title(
        f"Corpus Deduplication — "
        f"by chunks: {m['dedup_ratio_chunks']*100:.1f}%, "
        f"by bytes: {m['dedup_ratio_bytes']*100:.1f}%"
    )
    for bar in bars:
        height = bar.get_height()
        ax.annotate(f"{height:.0f} MB", xy=(bar.get_x() + bar.get_width()/2, height),
                    xytext=(0, 3), textcoords="offset points", ha="center")
    plt.tight_layout()
    plt.savefig(output_dir / "dedup_summary.pdf", dpi=300)
    plt.close()


def main():
    if len(sys.argv) < 2:
        print("Usage: visualize.py <overlap-results.json> [output-dir]")
        sys.exit(1)

    results = load_results(sys.argv[1])
    output_dir = Path(sys.argv[2]) if len(sys.argv) > 2 else Path("analysis/results/figures")
    output_dir.mkdir(parents=True, exist_ok=True)

    plot_pairwise_heatmap(results, output_dir)
    plot_coverage_curves(results, output_dir)
    plot_frequency_cdf(results, output_dir)
    plot_dedup_summary(results, output_dir)

    print(f"Figures saved to {output_dir}/")


if __name__ == "__main__":
    main()
