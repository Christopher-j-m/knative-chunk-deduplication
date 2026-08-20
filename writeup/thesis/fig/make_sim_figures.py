"""Generate the trace-driven simulation figures from the campaign CSVs.

Reads the simulation results from analysis/results/simulation and writes
publication PDFs into this directory. Run with:
uv run --with matplotlib,pandas make_sim_figures.py
"""

import os

import matplotlib
import matplotlib.pyplot as plt
import pandas as pd

matplotlib.use("Agg")

SIM = os.path.join(
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
}
POLICY_LABELS = {
    "lru": "LRU",
    "lru2": "LRU-2",
    "size": "Largest-first",
    "refcount": "Fewest-images",
}

MAIN_TRACE = "ibm-7d-top200"

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


def load_campaign():
    return pd.read_csv(os.path.join(SIM, "sim-campaign.csv"))


def fig_hit_rate_vs_capacity(campaign):
    # main curve is the volume mapping with seed 1; the shaded band spans the
    # min and max across every seed and mapping strategy at each capacity
    main = campaign[campaign.trace == MAIN_TRACE]
    fig, ax = plt.subplots(figsize=(4.6, 3.0))

    for policy in POLICY_LABELS:
        rows = main[main.policy == policy]
        line = rows[(rows.map_strategy == "volume") & (rows.seed == 1)]
        line = line.sort_values("capacity_bytes")

        spread = rows.groupby("capacity_bytes")["cluster_hit_rate_bytes"]
        x = line.capacity_bytes / 1e6

        ax.fill_between(x, spread.min().values, spread.max().values,
                        color=POLICY_COLORS[policy], alpha=0.15, linewidth=0)
        ax.plot(x, line.cluster_hit_rate_bytes, marker="o", markersize=3,
                color=POLICY_COLORS[policy], label=POLICY_LABELS[policy])

    peak = main.capacity_bytes.max() / 1e6
    ax.axhline(2 / 3, color="#888888", linewidth=0.8, linestyle=":")
    ax.text(peak, 2 / 3 + 0.015, "ceiling (N−1)/N", ha="right", fontsize=7.5, color="#555555")

    ax.set_xlabel("Cluster cache capacity (MB)")
    ax.set_ylabel("Byte hit rate")
    ax.set_ylim(0, 0.75)
    ax.legend(loc="upper left", fontsize=8)
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "sim_hit_rate_vs_capacity.pdf"))
    plt.close(fig)


def fig_registry_traffic_over_time():
    # the interesting window is the seeding phase: after every image has
    # reached every node, the persistent node caches absorb all traffic
    knee = pd.read_csv(os.path.join(SIM, "timeline-knee-lru.csv"))
    off = pd.read_csv(os.path.join(SIM, "timeline-nocache.csv"))

    fig, ax = plt.subplots(figsize=(4.6, 3.0))
    window = 20

    for d, label, color in [
        (off, "No cluster cache", "#e69f00"),
        (knee, "Cluster cache at knee capacity", "#0072b2"),
    ]:
        d = d[d.at_ms <= window * 1000]
        ax.plot(d.at_ms / 1000, d.registry_bytes / 1e9,
                drawstyle="steps-post", color=color, label=label)

    ax.set_xlabel("Simulated time (s)")
    ax.set_ylabel("Cumulative registry traffic (GB)")
    ax.set_xlim(0, window)
    ax.legend(loc="lower right", fontsize=8)
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "sim_registry_traffic_over_time.pdf"))
    plt.close(fig)


def main():
    campaign = load_campaign()
    fig_hit_rate_vs_capacity(campaign)
    fig_registry_traffic_over_time()
    print("wrote sim_hit_rate_vs_capacity.pdf and sim_registry_traffic_over_time.pdf")


if __name__ == "__main__":
    main()
