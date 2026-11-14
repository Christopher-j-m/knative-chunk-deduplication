"""Creates the figures (pdf) for the evaluation sections"""

import glob
import os
import re
import sys

import matplotlib
import matplotlib.ticker
from matplotlib.lines import Line2D
import matplotlib.pyplot as plt
import pandas as pd

matplotlib.use("Agg")

OUT = os.path.dirname(os.path.abspath(__file__))

CLOUD_PREFIX = "aks-same-region-scaleout"
CLOUD_AXIS_TOP = 20

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "../../scripts/summaries"))
from cell_bootstrap import mean_interval, repetition_groups  # noqa: E402
from result_paths import summary_path, find_benchmarks  # noqa: E402

CONFIG_COLORS = {
    1: "#0072b2",
    2: "#e69f00",
    3: "#56b4e9",
    4: "#009e73",
    "2s": "#cc79a7",
}
CLOUD_CONFIGS = (1, 2, 3, 4)
CONFIG_LABELS = {
    1: "Config 1 (OCI)",
    2: "Config 2 (Nydus)",
    3: "Config 3 (Nydus + Dragonfly)",
    4: "Config 4 (Nydus + cluster cache)",
    "2s": "Config 2s (plain nydusd)",
}

IMAGES = [
    ("fn-go-hello", "go-hello\n27 MB"),
    ("fn-python-hello", "python-hello\n197 MB"),
    ("fn-java-springboot", "springboot\n346 MB"),
    ("fn-node-api", "node-api\n384 MB"),
    ("fn-python-ml", "python-ml\n546 MB"),
    ("fn-pytorch-cpu", "pytorch-cpu\n1.27 GB"),
]

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


def load_cell(prefix, config, image):
    files = [str(path) for path in find_benchmarks(f"{prefix}-config{config}-{image}-*.csv")]
    if not files:
        return None
    return pd.read_csv(files[-1])


def phase_stats(frame, phase, warm):
    rows = frame[(frame["phase"] == phase) & (frame["warm"] == warm)]
    values = rows["ready_seconds"]
    return values.mean(), values.min(), values.max()


def phase_interval(frame, phase, warm):
    rows = frame[(frame["phase"] == phase) & (frame["warm"] == warm)]
    low, high = mean_interval(repetition_groups(rows))
    return rows["ready_seconds"].mean(), low, high


def cold_by_config():
    fig, ax = plt.subplots(figsize=(7.2, 3.4))
    n_configs = len(CLOUD_CONFIGS)
    width = 0.19

    for slot, config in enumerate(CLOUD_CONFIGS):
        xs, means, err_low, err_high, warms = [], [], [], [], []
        for idx, (image, _) in enumerate(IMAGES):
            frame = load_cell(CLOUD_PREFIX, config, image)
            if frame is None:
                continue
            mean, lo, hi = phase_interval(frame, "scaleout", "no")
            warm_mean, _, _ = phase_stats(frame, "scaleout", "yes")
            x = idx + (slot - (n_configs - 1) / 2) * width
            xs.append(x)
            means.append(mean)
            err_low.append(mean - lo)
            err_high.append(hi - mean)
            warms.append(warm_mean)

        shown = [min(mean, CLOUD_AXIS_TOP) for mean in means]
        ax.bar(xs, shown, width=width * 0.92, color=CONFIG_COLORS[config],
               label=CONFIG_LABELS[config], zorder=2)
        for x, mean, low, high in zip(xs, means, err_low, err_high):
            if mean > CLOUD_AXIS_TOP:
                ax.annotate(f"{mean:.0f} s", (x, CLOUD_AXIS_TOP), ha="center",
                            va="bottom", fontsize=7, xytext=(0, 1.5),
                            textcoords="offset points")
                continue
            ax.errorbar([x], [mean], yerr=[[low], [high]], fmt="none",
                        ecolor="#444444", elinewidth=0.8, capsize=1.6, zorder=3)
        for x, warm in zip(xs, warms):
            ax.hlines(warm, x - width * 0.46, x + width * 0.46,
                      color="black", linewidth=1.1, zorder=4)

    ax.set_ylim(0, CLOUD_AXIS_TOP)
    ax.set_xticks(range(len(IMAGES)))
    ax.set_xticklabels([label for _, label in IMAGES])
    ax.set_ylabel("Scale-out cold start (s)")
    ax.legend(ncol=2, loc="upper left", fontsize=8)
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "eval_cold_by_config.pdf"))
    plt.close(fig)


DISTANCE_PREFIX = "distance100k"
DEFAULTS_PREFIX = "plain-defaults-distance100k"
DISTANCE_RTTS = [0, 10, 25, 50, 80, 100]

MEASUREMENT_FILE = re.compile(r"-\d{8}T\d{6}\.csv$")


def newest_measurement(pattern):
    files = sorted(path for path in [str(path) for path in find_benchmarks(pattern)]
                   if MEASUREMENT_FILE.search(path))
    if not files:
        return None
    return pd.read_csv(files[-1])


def distance_stats(frame, warm):
    values = frame[(frame["phase"] == "scaleout") & (frame["warm"] == warm)]["ready_seconds"]
    return values.median(), values.min(), values.max()


def distance_series(image):
    sources = [
        (CONFIG_LABELS[1], CONFIG_COLORS[1], "-", "o", "{prefix}-config1-{image}-rtt{rtt}ms-rate600mbit-*.csv"),
        (CONFIG_LABELS[2], CONFIG_COLORS[2], "-", "o", "{prefix}-config2-{image}-rtt{rtt}ms-rate600mbit-*.csv"),
        ("Config 2s (plain nydusd, prefetch off)", CONFIG_COLORS["2s"], "-", "o",
         "{prefix}-config2s-{image}-rtt{rtt}ms-rate600mbit-*.csv"),
        ("Plain Nydus defaults (zstd, prefetch on)", CONFIG_COLORS["2s"], "--", "^",
         DEFAULTS_PREFIX + "-config2s-{image}-nydus-zstd-prefetchall-rtt{rtt}ms-rate600mbit-*.csv"),
        (CONFIG_LABELS[4], CONFIG_COLORS[4], "-", "o", "{prefix}-config4-{image}-rtt{rtt}ms-rate600mbit-*.csv"),
    ]
    series = []
    for label, color, style, marker, pattern in sources:
        xs, medians, lows, highs = [], [], [], []
        for rtt in DISTANCE_RTTS:
            frame = newest_measurement(pattern.format(prefix=DISTANCE_PREFIX, image=image, rtt=rtt))
            if frame is None:
                continue
            median, low, high = distance_stats(frame, "no")
            xs.append(rtt)
            medians.append(median)
            lows.append(low)
            highs.append(high)
        if xs:
            series.append((label, color, style, marker, xs, medians, lows, highs))
    return series


def distance_figure():
    panels = [("fn-python-hello", "python-hello (197 MB)"),
              ("fn-python-ml", "python-ml (546 MB)")]

    fig, axes = plt.subplots(1, 2, figsize=(7.2, 3.3), sharey=True)
    for ax, (image, title) in zip(axes, panels):
        for label, color, style, marker, xs, medians, lows, highs in distance_series(image):
            ax.fill_between(xs, lows, highs, color=color, alpha=0.15, linewidth=0)
            ax.plot(xs, medians, color=color, linestyle=style, marker=marker,
                    markersize=3.5, linewidth=1.6,
                    label=label if image == panels[0][0] else None)
        ax.set_yscale("log")
        ax.set_yticks([2, 3, 5, 10, 20, 50])
        ax.get_yaxis().set_major_formatter(matplotlib.ticker.ScalarFormatter())
        ax.get_yaxis().set_minor_formatter(matplotlib.ticker.NullFormatter())
        ax.set_title(title, fontsize=9)
        ax.set_xlabel("Emulated registry RTT (ms)")
        ax.set_xticks(DISTANCE_RTTS)
    axes[0].set_ylabel("Scale-out cold start (s)")

    fig.legend(loc="upper center", ncol=3, fontsize=7.5, bbox_to_anchor=(0.5, 1.08))
    fig.tight_layout(rect=(0, 0, 1, 0.92))
    fig.savefig(os.path.join(OUT, "eval_distance.pdf"), bbox_inches="tight")
    plt.close(fig)


REPLICA_TICK_MS = -4


def bandwidth_figure():
    summary = pd.read_csv(summary_path("bandwidth-summary.csv"))
    summary = summary[summary["probe_period_s"] == 1]

    images = [("fn-python-hello", "python-hello", "-"),
              ("fn-java-springboot", "springboot", "--"),
              ("fn-pytorch-cpu", "pytorch-cpu", ":")]

    fig, (ax_rate, ax_rtt) = plt.subplots(1, 2, figsize=(8.8, 3.5))

    rate_leads = {}
    for image, image_label, style in images:
        cells = summary[(summary["image"] == image) & (summary["experiment"] == "bandwidth-one-pod")]
        curves = {}
        for config in (1, 4):
            rows = cells[cells["config"] == config].sort_values("rate_mbit")
            if rows.empty:
                continue
            xs = list(rows["rate_mbit"])
            ys = list(rows["cold_fast_s"])
            curves[config] = (xs, ys)
            if len(xs) < 3:
                ax_rate.plot(xs, ys, linestyle="none", marker="o", markersize=4.5,
                             color=CONFIG_COLORS[config])
            else:
                ax_rate.plot(xs, ys, linestyle=style, marker="o", markersize=3.5,
                             linewidth=1.6, color=CONFIG_COLORS[config])
        if 1 in curves and 4 in curves:
            rate_leads[image_label] = (curves[1][1][-1] - curves[4][1][-1],
                                       curves[1][0][-1])

    ax_rate.set_xscale("log")
    ax_rate.set_xlabel("Registry rate (Mbit/s) -- one cold pod, no added delay")
    ax_rate.set_ylabel("Cold start (s)")
    ax_rate.set_title("The cache leads at every rate", fontsize=9)
    lead_text = "\n".join(f"{name} +{lead:.1f} s at {int(rate)}"
                          for name, (lead, rate) in rate_leads.items())
    ax_rate.annotate(f"cache lead at the fastest rate measured:\n{lead_text}",
                     xy=(0.97, 0.97), xycoords="axes fraction", ha="right", va="top",
                     fontsize=6.3, color="#555555")

    replicas = pd.read_csv(summary_path("second-replica-summary.csv"))
    second_pod_tax = {1: [], 4: []}
    for image, image_label, style in images:
        curves = {}
        for config in (1, 4):
            experiments = ["distance-eager"] if config == 1 else ["distance", "distance-repeat"]
            two_pod = summary[(summary["image"] == image) & (summary["config"] == config)
                              & (summary["experiment"].isin(experiments)) & (summary["rate_mbit"] == 600)]
            if two_pod.empty:
                continue
            two_pod = two_pod.sort_values("rtt_ms")
            xs = list(two_pod["rtt_ms"])
            ys = list(two_pod["cold_fast_s"])
            curves[config] = list(zip(xs, ys))
            if len(xs) < 3:
                ax_rtt.plot(xs, ys, linestyle="none", marker="o", markersize=4.5,
                            color=CONFIG_COLORS[config])
            else:
                ax_rtt.plot(xs, ys, linestyle=style, marker="o", markersize=3.5,
                            linewidth=1.6, color=CONFIG_COLORS[config])

            replica_row = replicas[(replicas["image"] == image) & (replicas["config"] == config)]
            if not replica_row.empty:
                one_pod_value = float(replica_row["one_pod_median_s"].iloc[0])
                two_pods_value = float(replica_row["two_pods_median_s"].iloc[0])
                ax_rtt.plot([REPLICA_TICK_MS], [one_pod_value], marker="o", markersize=3.5,
                            markerfacecolor="none", markeredgecolor=CONFIG_COLORS[config],
                            alpha=0.6, zorder=1)
                ax_rtt.plot([REPLICA_TICK_MS], [two_pods_value], marker="o", markersize=2.5,
                            color=CONFIG_COLORS[config], alpha=0.6, zorder=1)
                ax_rtt.plot([REPLICA_TICK_MS, REPLICA_TICK_MS], [one_pod_value, two_pods_value],
                            color=CONFIG_COLORS[config], linestyle=":", linewidth=0.9,
                            alpha=0.5, zorder=1)
                second_pod_tax[config].append(float(replica_row["second_replica_cost_s"].iloc[0]))

        if 1 in curves and 4 in curves:
            far = max(rtt for rtt, _ in curves[1])
            far_lead = dict(curves[1])[far] - dict(curves[4])[far]
            ax_rtt.annotate(f"{image_label} +{far_lead:.1f} s at {int(far)} ms",
                            xy=(far, dict(curves[1])[far]),
                            xytext=(4, 4), textcoords="offset points",
                            fontsize=6.5, color="#333333")

    ax_rtt.set_xlim(left=REPLICA_TICK_MS - 3)
    ax_rtt.set_xlabel("Registry round-trip time (ms) -- two cold pods scaling out\n"
                      "together at 600 Mbit/s (ticks left of 0 ms: one pod against two)")
    ax_rtt.set_ylabel("Cold start (s)")
    ax_rtt.set_title("The cache leads throughout, and pays less for the second pod",
                     fontsize=9)
    costs = {config: [round(value, 1) + 0.0 for value in values]
             for config, values in second_pod_tax.items()}
    tax_text = (f"cost of the second pod at 0 ms (means, one run):\n"
               f"eager {min(costs[1]):.1f} to {max(costs[1]):.1f} s\n"
               f"cache {min(costs[4]):.1f} to {max(costs[4]):.1f} s")
    ax_rtt.annotate(tax_text, xy=(0.34, 0.60), xycoords="axes fraction",
                    ha="left", va="center", fontsize=6.3, color="#555555")

    config_handles = [Line2D([0], [0], color=CONFIG_COLORS[c], linewidth=1.8,
                             label=CONFIG_LABELS[c]) for c in (1, 4)]
    image_handles = [Line2D([0], [0], color="#333333", linestyle=style, linewidth=1.4,
                            label=image_label) for _, image_label, style in images]
    fig.legend(handles=config_handles + image_handles, loc="upper center",
              ncol=5, fontsize=7.5, bbox_to_anchor=(0.5, 1.06))
    fig.tight_layout(rect=(0, 0, 1, 0.9))
    fig.savefig(os.path.join(OUT, "eval_bandwidth.pdf"), bbox_inches="tight")
    plt.close(fig)


def floor_decomposition():
    summary = pd.read_csv(summary_path("bandwidth-summary.csv"))
    clean = summary[(summary["probe_period_s"] == 1) & (summary["experiment"] == "bandwidth-one-pod")]

    bands = [("app_init_s", "Application init", "#666666"),
             ("fetch_s", "Cluster-cache fetch", "#009e73"),
             ("transfer_s", "Registry transfer", "#0072b2")]

    labels, stacks, spreads = [], [], []
    for image, short in [("fn-python-hello", "python-hello"),
                         ("fn-java-springboot", "springboot"),
                         ("fn-pytorch-cpu", "pytorch-cpu")]:
        cells = clean[clean["image"] == image]
        if cells.empty:
            continue
        rate = int(cells["rate_mbit"].max())
        for config in (1, 4):
            row = cells[(cells["config"] == config) & (cells["rate_mbit"] == rate)]
            if row.empty:
                continue
            labels.append(f"{short}\n{rate} Mbit/s\nconfig {config}")
            stacks.append([max(0.0, float(row[column].iloc[0])) for column, _, _ in bands])
            spread = row["contention_s"].iloc[0]
            spreads.append(float(spread) if pd.notna(spread) else 0.0)

    fig, ax = plt.subplots(figsize=(6.6, 3.4))
    bottoms = [0.0] * len(stacks)
    for index, (_, band_label, color) in enumerate(bands):
        heights = [stack[index] for stack in stacks]
        ax.bar(labels, heights, bottom=bottoms, color=color, label=band_label, width=0.6,
               zorder=2)
        bottoms = [bottom + height for bottom, height in zip(bottoms, heights)]

    ax.errorbar(labels, bottoms, yerr=spreads, fmt="none", ecolor="#e69f00",
                elinewidth=1.3, capsize=3, zorder=3,
                label="spread across pods that cold-started together in the same repetition")

    ax.set_ylabel("Cold start (s)")
    ax.tick_params(axis="x", labelsize=7.5)
    ax.legend(fontsize=7, ncol=1, loc="upper center", bbox_to_anchor=(0.5, -0.18))
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "eval_floor_decomposition.pdf"), bbox_inches="tight")
    plt.close(fig)


def main():
    cold_by_config()
    distance_figure()
    bandwidth_figure()
    floor_decomposition()
    print("figures written to", OUT)


if __name__ == "__main__":
    main()
