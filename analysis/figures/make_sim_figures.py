"""Creates the figures (pdf) for the thesis based on the simulation results"""

import os

import matplotlib
import matplotlib.pyplot as plt
import matplotlib.ticker
import pandas as pd

matplotlib.use("Agg")

SIMULATION_DIR = os.environ.get("SIMULATION_DIR") or os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    "../results/simulation",
)
OUT = os.path.dirname(os.path.abspath(__file__))

POLICY_COLORS = {
    "lru": "#0072b2",
    "lru2": "#56b4e9",
    "size": "#e69f00",
    "refcount": "#009e73",
    "gdsf": "#d55e00",
}
POLICY_LABELS = {
    "lru": "LRU",
    "lru2": "LRU-2",
    "size": "Largest-first",
    "refcount": "Fewest-images",
    "gdsf": "Size-frequency",
}

MAIN_TRACE = os.environ.get("SIM_TRACE", "ibm-7d-all")
MAIN_CATALOG = "n792"

plt.rcParams.update({
    "font.size": 9,
    "axes.spines.top": False,
    "axes.spines.right": False,
    "axes.grid": True,
    "axes.grid.axis": "y",
    "grid.color": "#dddddd",
    "grid.linewidth": 0.6,
    "axes.axisbelow": True,
    "legend.frameon": False,
})


def load(name):
    return pd.read_csv(os.path.join(SIMULATION_DIR, name))


def at_80ms(rows):
    return rows[(rows.cache_chunk_ms > 1) & (rows.map_strategy == "volume") & (rows.policy == "lru")]


def fig_capacity_scale(replay):
    rows = replay[(replay.trace == MAIN_TRACE) & (replay.working_sets == MAIN_CATALOG)
                    & (replay.capacity_bytes > 0) & (replay.cluster_cache == "yes")]
    if rows.empty:
        return
    peak = rows.capacity_bytes.max()
    node_counts = sorted(rows.nodes.unique())

    fig, axes = plt.subplots(1, len(node_counts), figsize=(7.0, 2.9), sharey=True)
    for ax, nodes in zip(axes, node_counts):
        cell = rows[rows.nodes == nodes]
        for policy in POLICY_LABELS:
            line = cell[cell.policy == policy].sort_values("capacity_bytes")
            ax.plot(line.capacity_bytes / peak, line.cluster_hit_rate_bytes, marker="o", markersize=3.5,
                    linewidth=1.5, color=POLICY_COLORS[policy], label=POLICY_LABELS[policy])
        ax.axhline((nodes - 1) / nodes, color="#888888", linewidth=0.8, linestyle=":")
        ax.set_title(f"{nodes:,} nodes", fontsize=9)
        ax.set_xlabel("Capacity as a fraction of the unbounded peak")
        ax.set_xticks([0.1, 0.25, 0.5, 0.75, 1.0])
    axes[0].set_ylabel("Cluster byte hit rate")
    axes[0].set_ylim(0, 1.02)
    axes[0].legend(fontsize=7, loc="lower right")
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "sim_capacity_scale.pdf"), bbox_inches="tight")
    plt.close(fig)
    return "sim_capacity_scale.pdf"


def fig_response_tail(latency, node_counts=(3, 100, 10000), placement="random"):
    rows = latency[(latency.trace == MAIN_TRACE) & (latency.working_sets == MAIN_CATALOG)
                   & (latency.capacity_bytes == 0) & (latency.placement == placement)]
    rows = at_80ms(rows)
    rows = rows[rows.nodes.isin(node_counts)]
    if rows.empty:
        return

    quantiles = [0.50, 0.95, 0.99, 0.999, 0.9999]
    columns = ["p50_ms", "p95_ms", "p99_ms", "p999_ms", "p9999_ms"]
    depth = [1 / (1 - q) for q in quantiles]
    node_colors = dict(zip(sorted(rows.nodes.unique()), ["#0072b2", "#d55e00", "#cc79a7"]))
    styles = {"yes": ("-", 1.8), "no": ("--", 1.8)}

    fig, ax = plt.subplots(figsize=(4.8, 3.0))
    for nodes in sorted(rows.nodes.unique()):
        for flag in ("no", "yes"):
            row = rows[(rows.nodes == nodes) & (rows.cluster_cache == flag)]
            if row.empty:
                continue
            style, width = styles[flag]
            ax.plot(depth, [row.iloc[0][c] for c in columns], linestyle=style, linewidth=width,
                    marker="o", markersize=3.5, color=node_colors[nodes],
                    label=f"{nodes:,} nodes, {'with cluster cache' if flag == 'yes' else 'plain Nydus'}")
    ax.set_xscale("log")
    ax.set_yscale("log")
    ax.set_xticks(depth)
    ax.set_xticklabels(["p50", "p95", "p99", "p99.9", "p99.99"], fontsize=7.5)
    ax.set_xlabel("Quantile of invocation response time")
    ax.set_ylabel("Response time (ms, log)")
    ax.get_yaxis().set_major_formatter(matplotlib.ticker.ScalarFormatter())
    ax.legend(fontsize=6.5, loc="upper left")
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "sim_response_tail.pdf"), bbox_inches="tight")
    plt.close(fig)
    return "sim_response_tail.pdf"


def main():
    replay = load("scale-replay.csv")
    latency = load("scale-latency.csv")
    written = [
        fig_capacity_scale(replay),
        fig_response_tail(latency),
    ]
    print("wrote", ", ".join(name for name in written if name))


if __name__ == "__main__":
    main()
