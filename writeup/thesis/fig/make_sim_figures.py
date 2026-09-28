"""Generate the trace-driven simulation figures from the scale campaign CSVs.

Reads the simulation results from analysis/results/simulation and writes
publication PDFs into this directory. Run with:
uv run --with matplotlib,pandas make_sim_figures.py
Set SIM_DIR to point at another results directory, for example a smoke run.
"""

import os

import matplotlib
import matplotlib.pyplot as plt
import matplotlib.ticker
import pandas as pd

matplotlib.use("Agg")

SIM = os.environ.get("SIM_DIR") or os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    "../../../analysis/results/simulation",
)
OUT = os.path.dirname(os.path.abspath(__file__))

# One fixed Okabe-Ito color per eviction policy, identical across figures
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

# the two configurations keep the colors the evaluation chapter uses for them
CACHED = "#009e73"
UNCACHED = "#e69f00"

# where each byte that left the node came from
ORIGIN_COLORS = {"cache": CACHED, "registry": UNCACHED}

CLASS_LABELS = ["F1 ≤10", "F2 11–100", "F3 101–1k", "F4 1k–10k", "F5 10k–100k", "F6 >100k"]
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
    return pd.read_csv(os.path.join(SIM, name))


def at_80ms(rows):
    # the 80 ms coefficients are the ones the study reports; block C runs the
    # same cells at the 0 ms pair and must not be mixed in
    return rows[(rows.registry_chunk_ms > 10) & (rows.map_strategy == "volume") & (rows.policy == "lru")]


def block_a(campaign, catalog=MAIN_CATALOG):
    rows = campaign[(campaign.trace == MAIN_TRACE) & (campaign.working_sets == catalog) & (campaign.capacity_bytes == 0)]
    return at_80ms(rows)


def fig_scale_origin(campaign):
    """Cluster-eligible startup bytes by origin and the byte hit rate against
    cluster size.

    Node-local hits are left out of the bars because they are three orders of
    magnitude larger than the rest and would hide it; the bars are the bytes
    that had to leave the node, split by whether the cluster cache or the
    registry served them, and the marker is what the uncached configuration
    pulls from the registry for the same cold starts. One column per placement
    rule and one row per measure, so the two measures never share an axis.
    """
    rows = block_a(campaign)
    if rows.empty:
        return
    # the byte panels keep their own scales: random placement moves three
    # orders of magnitude more bytes than packed, and one shared axis would
    # flatten the packed bars into the baseline
    fig, axes = plt.subplots(2, 2, figsize=(7.0, 4.8), sharex="col",
                             gridspec_kw={"height_ratios": [3, 2]})
    axes[1][1].sharey(axes[1][0])

    for col, placement in enumerate(("packed", "random")):
        cached = rows[(rows.placement == placement) & (rows.cluster_cache == "yes")].sort_values("nodes")
        # the uncached registry bytes are looked up by node count so a cell
        # that exists in only one configuration cannot shift the marks
        uncached = rows[(rows.placement == placement) & (rows.cluster_cache == "no")]
        uncached = uncached.set_index("nodes").registry_bytes.reindex(cached.nodes)
        positions = list(range(len(cached)))

        top = axes[0][col]
        cache_gb = cached.cluster_hit_bytes.values / 1e9
        registry_gb = cached.registry_bytes.values / 1e9
        top.bar(positions, cache_gb, width=0.62, color=ORIGIN_COLORS["cache"],
                label="served by the cluster cache", linewidth=0.8, edgecolor="white")
        top.bar(positions, registry_gb, bottom=cache_gb, width=0.62, color=ORIGIN_COLORS["registry"],
                label="fetched from the registry", linewidth=0.8, edgecolor="white")
        top.plot(positions, uncached.values / 1e9, linestyle="none", marker="_", markersize=18,
                 markeredgewidth=2.2, color="#000000", label="registry bytes without a cluster cache")
        top.set_title(f"{placement} placement", fontsize=9)

        low = axes[1][col]
        low.plot(positions, cached.cluster_hit_rate_bytes, marker="o", markersize=4, color=CACHED,
                 linewidth=1.6, label="cluster byte hit rate")
        low.plot(positions, (cached.nodes - 1) / cached.nodes, linestyle=":", color="#888888",
                 linewidth=0.9, label="ceiling (N−1)/N")
        low.set_xticks(positions)
        low.set_xticklabels([f"{n:,}" for n in cached.nodes])
        low.set_xlabel("Nodes in the cluster")
        low.set_ylim(0, 1.02)

    axes[0][0].set_ylabel("Bytes leaving the node over the week (GB)")
    axes[0][1].set_ylabel("")
    axes[1][0].set_ylabel("Cluster byte hit rate")
    handles, labels = axes[0][0].get_legend_handles_labels()
    more, more_labels = axes[1][0].get_legend_handles_labels()
    fig.legend(handles + more, labels + more_labels, fontsize=7, loc="lower center", ncol=3,
               bbox_to_anchor=(0.5, -0.04))
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "sim_scale_origin.pdf"), bbox_inches="tight")
    plt.close(fig)
    return "sim_scale_origin.pdf"


def fig_frequency_classes(classes, nodes=10000, placement="random"):
    """Cold-start share and cold-start latency per invocation-frequency class.

    The latency panel uses the large-cluster bound, where every cold start
    leaves the node; under packed placement both bars sit on the warm floor.
    """
    rows = classes[(classes.trace == MAIN_TRACE) & (classes.working_sets == MAIN_CATALOG)
                   & (classes.capacity_bytes == 0) & (classes.placement == placement) & (classes.nodes == nodes)]
    rows = at_80ms(rows)
    if rows.empty:
        return
    cached = rows[rows.cluster_cache == "yes"].reset_index(drop=True)
    uncached = rows[rows.cluster_cache == "no"].reset_index(drop=True)
    positions = range(len(cached))

    fig, axes = plt.subplots(1, 2, figsize=(7.0, 2.9))

    # left: how often each class cold-starts, which the cache does not change
    axes[0].bar(positions, cached.cold_share * 100, width=0.62, color="#0072b2", linewidth=0.8, edgecolor="white")
    axes[0].set_ylabel("Invocations that cold-start (%)")
    axes[0].set_ylim(0, 105)

    # right: what the cold start costs, with and without the cluster tier
    width = 0.36
    axes[1].bar([p - width / 2 for p in positions], uncached.cold_p50_ms / 1000, width=width,
                color=UNCACHED, label="without cluster cache", linewidth=0.8, edgecolor="white")
    axes[1].bar([p + width / 2 for p in positions], cached.cold_p50_ms / 1000, width=width,
                color=CACHED, label="with cluster cache", linewidth=0.8, edgecolor="white")
    axes[1].set_yscale("log")
    axes[1].set_ylabel("Median cold-start latency (s, log)")
    axes[1].set_yticks([2, 3, 5, 10, 20])
    axes[1].get_yaxis().set_major_formatter(matplotlib.ticker.ScalarFormatter())
    axes[1].get_yaxis().set_minor_formatter(matplotlib.ticker.NullFormatter())
    axes[1].legend(fontsize=7, loc="upper right")

    for ax in axes:
        ax.set_xticks(list(positions))
        ax.set_xticklabels(CLASS_LABELS[:len(cached)], fontsize=7.5, rotation=20, ha="right")
        ax.set_xlabel("Invocation-frequency class (calls per week)")
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "sim_frequency_classes.pdf"), bbox_inches="tight")
    plt.close(fig)
    return "sim_frequency_classes.pdf"


def fig_capacity_scale(campaign):
    """Byte hit rate against cache capacity at two cluster sizes, per policy."""
    rows = campaign[(campaign.trace == MAIN_TRACE) & (campaign.working_sets == MAIN_CATALOG)
                    & (campaign.capacity_bytes > 0) & (campaign.cluster_cache == "yes")]
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
    """Response time against quantile depth, one line pair per cluster size.

    Random placement is the bound under which the tail separates; under
    packed placement the pairs coincide at every size.
    """
    rows = latency[(latency.trace == MAIN_TRACE) & (latency.working_sets == MAIN_CATALOG)
                   & (latency.capacity_bytes == 0) & (latency.placement == placement)]
    rows = at_80ms(rows)
    rows = rows[rows.nodes.isin(node_counts)]
    if rows.empty:
        return

    quantiles = [0.50, 0.95, 0.99, 0.999, 0.9999]
    columns = ["p50_ms", "p95_ms", "p99_ms", "p999_ms", "p9999_ms"]
    # plotted against the tail depth 1/(1-q), so each decade of rarity is one
    # equal step and the point where the configurations part is legible
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
                    label=f"{nodes:,} nodes, {'with' if flag == 'yes' else 'without'} cluster cache")
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
    campaign = load("scale-campaign.csv")
    classes = load("scale-classes.csv")
    latency = load("scale-latency.csv")
    written = [
        fig_scale_origin(campaign),
        fig_frequency_classes(classes),
        fig_capacity_scale(campaign),
        fig_response_tail(latency),
    ]
    print("wrote", ", ".join(name for name in written if name))


if __name__ == "__main__":
    main()
