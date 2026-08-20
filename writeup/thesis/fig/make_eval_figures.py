"""Generate the evaluation-chapter figures from the benchmark CSVs.

Reads the raw campaign CSVs from analysis/results/benchmarks and writes
publication PDFs into this directory. Run with:
uv run --with matplotlib,pandas make_eval_figures.py
"""

import glob
import os

import matplotlib
from matplotlib.lines import Line2D
import matplotlib.pyplot as plt
import pandas as pd

matplotlib.use("Agg")

BENCH = os.path.join(
    os.path.dirname(__file__),
    "../../../analysis/results/benchmarks",
)
OUT = os.path.dirname(os.path.abspath(__file__))
SUMMARIES = os.path.join(
    os.path.dirname(__file__),
    "../../../analysis/results/summaries",
)

# One fixed color per configuration, identical across every figure
CONFIG_COLORS = {
    1: "#0072b2",
    2: "#e69f00",
    3: "#56b4e9",
    4: "#009e73",
}
CONFIG_LABELS = {
    1: "Config 1 (OCI)",
    2: "Config 2 (Nydus)",
    3: "Config 3 (Nydus + Dragonfly)",
    4: "Config 4 (Nydus + cluster cache)",
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
    pattern = os.path.join(BENCH, f"{prefix}-config{config}-{image}-*.csv")
    files = sorted(glob.glob(pattern))
    if not files:
        return None
    return pd.read_csv(files[-1])


def phase_stats(frame, phase, warm):
    rows = frame[(frame["phase"] == phase) & (frame["warm"] == warm)]
    values = rows["ready_seconds"]
    return values.mean(), values.min(), values.max()


def cold_by_config():
    fig, ax = plt.subplots(figsize=(7.2, 3.4))
    n_configs = len(CONFIG_COLORS)
    width = 0.19

    for slot, config in enumerate(sorted(CONFIG_COLORS)):
        xs, means, err_low, err_high, warms = [], [], [], [], []
        for idx, (image, _) in enumerate(IMAGES):
            frame = load_cell("aks-scaleout", config, image)
            if frame is None:
                continue
            mean, lo, hi = phase_stats(frame, "scaleout", "no")
            warm_mean, _, _ = phase_stats(frame, "scaleout", "yes")
            x = idx + (slot - (n_configs - 1) / 2) * width
            xs.append(x)
            means.append(mean)
            err_low.append(mean - lo)
            err_high.append(hi - mean)
            warms.append(warm_mean)

        ax.bar(xs, means, width=width * 0.92, color=CONFIG_COLORS[config],
               label=CONFIG_LABELS[config], zorder=2)
        ax.errorbar(xs, means, yerr=[err_low, err_high], fmt="none",
                    ecolor="#444444", elinewidth=0.8, capsize=1.6, zorder=3)
        # short black tick marking the matching warm floor on each bar
        for x, warm in zip(xs, warms):
            ax.hlines(warm, x - width * 0.46, x + width * 0.46,
                      color="black", linewidth=1.1, zorder=4)

    ax.set_xticks(range(len(IMAGES)))
    ax.set_xticklabels([label for _, label in IMAGES])
    ax.set_ylabel("Scale-out cold start (s)")
    ax.legend(ncol=2, loc="upper left", fontsize=8)
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "eval_cold_by_config.pdf"))
    plt.close(fig)


def distance_anchors():
    anchors = [("fn-python-hello", "python-hello (197 MB)"),
               ("fn-pytorch-cpu", "pytorch-cpu (1.27 GB)")]
    configs = [1, 2, 4]
    regions = [("aks-scaleout", "same region"),
               ("aks-eastus-scaleout", "eastus ($\\approx$80 ms)")]

    fig, axes = plt.subplots(1, 2, figsize=(7.2, 3.0))
    width = 0.24

    for ax, (image, title) in zip(axes, anchors):
        for slot, config in enumerate(configs):
            xs, means, err_low, err_high = [], [], [], []
            for ridx, (prefix, _) in enumerate(regions):
                frame = load_cell(prefix, config, image)
                if frame is None:
                    continue
                mean, lo, hi = phase_stats(frame, "scaleout", "no")
                xs.append(ridx + (slot - 1) * width)
                means.append(mean)
                err_low.append(mean - lo)
                err_high.append(hi - mean)
            ax.bar(xs, means, width=width * 0.9, color=CONFIG_COLORS[config],
                   label=CONFIG_LABELS[config] if image == anchors[0][0] else None,
                   zorder=2)
            ax.errorbar(xs, means, yerr=[err_low, err_high], fmt="none",
                        ecolor="#444444", elinewidth=0.8, capsize=1.6, zorder=3)
            for x, mean in zip(xs, means):
                ax.annotate(f"{mean:.1f}", (x, mean), ha="center", va="bottom",
                            fontsize=7, xytext=(0, 1.5),
                            textcoords="offset points")
        ax.set_xticks(range(len(regions)))
        ax.set_xticklabels([label for _, label in regions], fontsize=8)
        ax.set_title(title, fontsize=9)
        ax.set_ylabel("Scale-out cold start (s)")

    axes[1].set_ylabel(None)
    fig.legend(loc="upper center", ncol=3, fontsize=8,
               bbox_to_anchor=(0.5, 1.04))
    fig.tight_layout(rect=(0, 0, 1, 0.94))
    fig.savefig(os.path.join(OUT, "eval_distance_anchors.pdf"),
                bbox_inches="tight")
    plt.close(fig)


def load_sweep_cell(config, image, rtt, rate="600mbit"):
    pattern = os.path.join(
        BENCH, f"netem-sweep-config{config}-{image}-rtt{rtt}ms-rate{rate}-*.csv"
    )
    files = sorted(glob.glob(pattern))
    if not files:
        return None
    return pd.read_csv(files[-1])


def sweep_stats(frame, warm):
    values = frame[(frame["phase"] == "scaleout") & (frame["warm"] == warm)]["ready_seconds"]
    return values.median(), values.min(), values.max()


def distance_sweep():
    rtts = [0, 10, 25, 50, 80, 100]
    panels = [("fn-python-hello", "python-hello (197 MB)"),
              ("fn-python-ml", "python-ml (546 MB)")]

    fig, axes = plt.subplots(1, 2, figsize=(7.2, 3.0))
    for ax, (image, title) in zip(axes, panels):
        for config in (2, 4):
            xs, medians, lows, highs, warms = [], [], [], [], []
            for rtt in rtts:
                frame = load_sweep_cell(config, image, rtt)
                if frame is None:
                    continue
                median, low, high = sweep_stats(frame, "no")
                warm_median, _, _ = sweep_stats(frame, "yes")
                xs.append(rtt)
                medians.append(median)
                lows.append(low)
                highs.append(high)
                warms.append(warm_median)
            ax.fill_between(xs, lows, highs, color=CONFIG_COLORS[config],
                            alpha=0.15, linewidth=0)
            ax.plot(xs, medians, color=CONFIG_COLORS[config], marker="o",
                    markersize=3.5, linewidth=1.6,
                    label=CONFIG_LABELS[config] if image == panels[0][0] else None)
            # dashed warm floor of the same configuration, explained in the caption
            ax.plot(xs, warms, color=CONFIG_COLORS[config], linestyle="--",
                    linewidth=1.0)
        ax.set_title(title, fontsize=9)
        ax.set_xlabel("Emulated registry RTT (ms)")
        ax.set_xticks(rtts)
        ax.set_ylabel("Scale-out cold start (s)")

    axes[1].set_ylabel(None)
    fig.legend(loc="upper center", ncol=2, fontsize=8, bbox_to_anchor=(0.5, 1.04))
    fig.tight_layout(rect=(0, 0, 1, 0.94))
    fig.savefig(os.path.join(OUT, "eval_distance_sweep.pdf"), bbox_inches="tight")
    plt.close(fig)


def crossimage_reuse():
    pairs = [
        ("fn-python-ml", "python-ml\n(shares python base)", 1592),
        ("fn-go-hello", "go-hello\n(negative control)", 41),
    ]
    fig, ax = plt.subplots(figsize=(6.2, 2.2))

    for idx, (image, _, _) in enumerate(pairs):
        files = sorted(glob.glob(os.path.join(
            BENCH, f"aks-crossimage-fn-python-hello-{image}-*.csv")))
        frame = pd.read_csv(files[-1])
        cross = frame[frame["phase"] == "cross"]
        hits = int(cross["cache_hits_delta"].iloc[0])
        misses = int(cross["cache_misses_delta"].iloc[0])
        total = hits + misses

        hit_share = hits / total * 100
        miss_share = misses / total * 100
        ax.barh(idx, hit_share, color=CONFIG_COLORS[4], zorder=2,
                label="served from cache (seeded by python-hello)" if idx == 0 else None)
        ax.barh(idx, miss_share, left=hit_share, color="#b0b0b0", zorder=2,
                label="fetched from registry" if idx == 0 else None)
        hit_word = "hit" if hits == 1 else "hits"
        ax.annotate(f"{hits} {hit_word} ({hit_share:.1f} %)",
                    (hit_share / 2 if hit_share > 20 else hit_share + 2, idx),
                    va="center", ha="center" if hit_share > 20 else "left",
                    fontsize=8, color="white" if hit_share > 20 else "#333333")
        ax.annotate(f"{misses} misses", (hit_share + miss_share / 2, idx),
                    va="center", ha="center", fontsize=8, color="#333333")

    ax.set_yticks(range(len(pairs)))
    ax.set_yticklabels([label for _, label, _ in pairs], fontsize=8)
    ax.invert_yaxis()
    ax.set_xlim(0, 100)
    ax.set_xlabel("Share of the touched chunk set (%)")
    ax.grid(axis="x", color="#dddddd", linewidth=0.6)
    ax.grid(axis="y", visible=False)
    ax.legend(loc="upper center", bbox_to_anchor=(0.5, -0.38), ncol=2,
              fontsize=7.5)
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "eval_crossimage_reuse.pdf"),
                bbox_inches="tight")
    plt.close(fig)


def bandwidth_sweep():
    """Draw the registry-rate sweep as it was measured under the coarse probe.

    The thesis does not include this figure: its cached curve was inflated by
    the ten-second readiness probe, and the envelope figure shows the same
    comparison from cells measured on the pinned probe. The drawing is kept
    so the older cells stay reproducible from their own summary.
    """
    summary = pd.read_csv(os.path.join(SUMMARIES, "rate-sweep-summary.csv"))
    panels = [("fn-python-hello", "python-hello (48.7 MB)"),
              ("fn-java-springboot", "springboot (94.0 MB)"),
              ("fn-pytorch-cpu", "pytorch-cpu (267.5 MB)")]
    rates = sorted(summary["rate_mbit"].unique())

    fig, axes = plt.subplots(1, 3, figsize=(7.2, 2.9), sharey=True)
    for ax, (image, title) in zip(axes, panels):
        for config in (1, 2, 4):
            cells = summary[(summary["image"] == image) & (summary["config"] == config)]
            cells = cells.sort_values("rate_mbit")
            if cells.empty:
                continue

            xs = list(cells["rate_mbit"])
            medians = list(cells["cold_median_s"])
            ax.fill_between(xs, list(cells["cold_min_s"]), list(cells["cold_max_s"]),
                            color=CONFIG_COLORS[config], alpha=0.15, linewidth=0)
            ax.plot(xs, medians, color=CONFIG_COLORS[config], marker="o",
                    markersize=3.5, linewidth=1.6,
                    label=CONFIG_LABELS[config] if image == panels[0][0] else None)

            # the warm floor of the cached configuration, the fastest a pod can
            # start once nothing has to be fetched at all
            if config == 4:
                ax.axhline(cells["warm_median_s"].median(), color=CONFIG_COLORS[4],
                           linestyle="--", linewidth=1.0)

        ax.set_xscale("log")
        ax.set_xticks(rates)
        ax.set_xticklabels([str(rate) for rate in rates], fontsize=7)
        ax.minorticks_off()
        ax.set_title(title, fontsize=9)
        ax.set_xlabel("Registry egress rate (Mbit/s)")

    axes[0].set_ylabel("Scale-out cold start (s)")
    fig.legend(loc="upper center", ncol=3, fontsize=8, bbox_to_anchor=(0.5, 1.06))
    fig.tight_layout(rect=(0, 0, 1, 0.94))
    fig.savefig(os.path.join(OUT, "eval_bandwidth_sweep.pdf"), bbox_inches="tight")
    plt.close(fig)


def envelope():
    """Show that the cluster cache leads across the whole measured space.

    Every cell drawn here was measured with the readiness probe pinned at one
    second. There is no crossing anywhere in the measured space: the cache is
    faster at every registry rate up to 10 Gbit/s and at every round-trip
    time measured, with two cold pods starting together as much as one.
    Eager pulling pays hard for the second pod, since both replicas pull the
    full image through the same uplink at once; the cache barely notices,
    since only the first replica touches the registry at all. Everything
    drawn here is a real cell; there is no model term left in this figure.
    """
    summary = pd.read_csv(os.path.join(SUMMARIES, "envelope-summary.csv"))
    # a readiness probe bug inflated cold starts measured before the fix, so
    # only cells timed after the fix belong on the same axis
    summary = summary[summary["probe_period_s"] == 1]

    images = [("fn-python-hello", "python-hello", "-"),
              ("fn-java-springboot", "springboot", "--"),
              ("fn-pytorch-cpu", "pytorch-cpu", ":")]

    fig, (ax_rate, ax_rtt) = plt.subplots(1, 2, figsize=(8.8, 3.5))

    # left panel: cold start against registry rate, one cold pod, no added
    # delay -- the solo cells
    rate_leads = {}
    for image, image_label, style in images:
        cells = summary[(summary["image"] == image) & (summary["campaign"] == "solo")]
        curves = {}
        for config in (1, 4):
            rows = cells[cells["config"] == config].sort_values("rate_mbit")
            if rows.empty:
                continue
            xs = list(rows["rate_mbit"])
            ys = list(rows["cold_fast_s"])
            curves[config] = (xs, ys)
            # a line implies a trend between points; two points on their own
            # do not, so those are left as bare markers
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
    ax_rate.set_title("No crossover up to 10 Gbit/s", fontsize=9)
    lead_text = "\n".join(f"{name} +{lead:.1f} s at {int(rate)}"
                          for name, (lead, rate) in rate_leads.items())
    ax_rate.annotate(f"cache lead at the fastest rate measured:\n{lead_text}",
                     xy=(0.97, 0.97), xycoords="axes fraction", ha="right", va="top",
                     fontsize=6.3, color="#555555")

    # right panel: cold start against round-trip time at 600 Mbit/s, two
    # cold pods scaling out together, both replicas pulling at once -- the
    # eager, netem and anchor cells. Every point on every line is two-pod;
    # the single-pod solo cell for the same rate is drawn as a faint marker
    # at 0 ms only, to show what the second replica itself costs
    second_pod_tax = {1: [], 4: []}
    for image, image_label, style in images:
        curves = {}
        for config in (1, 4):
            campaigns = ["eager"] if config == 1 else ["netem", "anchor"]
            two_pod = summary[(summary["image"] == image) & (summary["config"] == config)
                              & (summary["campaign"].isin(campaigns)) & (summary["rate_mbit"] == 600)]
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

            # the single-pod reference at the same rate, drawn faint and
            # hollow so the two-pod line stays the thing the eye follows
            solo = summary[(summary["image"] == image) & (summary["campaign"] == "solo")
                           & (summary["config"] == config) & (summary["rate_mbit"] == 600)]
            zero_two_pod = dict(curves[config]).get(0.0)
            if not solo.empty and zero_two_pod is not None:
                solo_value = float(solo["cold_fast_s"].iloc[0])
                ax_rtt.plot([0], [solo_value], marker="o", markersize=3.5,
                            markerfacecolor="none", markeredgecolor=CONFIG_COLORS[config],
                            alpha=0.55, zorder=1)
                ax_rtt.plot([0, 0], [solo_value, zero_two_pod], color=CONFIG_COLORS[config],
                           linestyle=":", linewidth=0.9, alpha=0.45, zorder=1)
                second_pod_tax[config].append(zero_two_pod - solo_value)

        # the final gap between the two lines, so the reader has a number
        # for how far ahead the cache still is once both curves reach 80 ms
        if 1 in curves and 4 in curves:
            far = max(rtt for rtt, _ in curves[1])
            far_lead = dict(curves[1])[far] - dict(curves[4])[far]
            ax_rtt.annotate(f"{image_label} +{far_lead:.1f} s at {int(far)} ms",
                            xy=(far, dict(curves[1])[far]),
                            xytext=(4, 4), textcoords="offset points",
                            fontsize=6.5, color="#333333")

    ax_rtt.set_xlabel("Registry round-trip time (ms) -- two cold pods scaling out\n"
                      "together at 600 Mbit/s (faint marker at 0 ms: the same cell, one pod)")
    ax_rtt.set_ylabel("Cold start (s)")
    ax_rtt.set_title("The cache leads throughout, and pays far less for the second pod",
                     fontsize=9)
    tax_text = (f"cost of the second pod at 0 ms:\n"
               f"eager +{min(second_pod_tax[1]):.1f} to +{max(second_pod_tax[1]):.1f} s\n"
               f"cache +{min(second_pod_tax[4]):.1f} to +{max(second_pod_tax[4]):.1f} s")
    # placed in the empty band between the pytorch and springboot curves and
    # clear of the zero-delay markers, since the bottom right corner belongs
    # to the python-hello lines
    ax_rtt.annotate(tax_text, xy=(0.34, 0.60), xycoords="axes fraction",
                    ha="left", va="center", fontsize=6.3, color="#555555")

    config_handles = [Line2D([0], [0], color=CONFIG_COLORS[c], linewidth=1.8,
                             label=CONFIG_LABELS[c]) for c in (1, 4)]
    image_handles = [Line2D([0], [0], color="#333333", linestyle=style, linewidth=1.4,
                            label=image_label) for _, image_label, style in images]
    fig.legend(handles=config_handles + image_handles, loc="upper center",
              ncol=5, fontsize=7.5, bbox_to_anchor=(0.5, 1.06))
    fig.tight_layout(rect=(0, 0, 1, 0.9))
    fig.savefig(os.path.join(OUT, "eval_envelope.pdf"), bbox_inches="tight")
    plt.close(fig)


def floor_decomposition():
    """Stack what a cold start is actually spent on, per image and configuration.

    Only the cluster-cache fetch or the registry transfer band is what a
    caching scheme could remove; the application's own initialization, read
    from the warm median, is a floor no cache touches. Each bar is a single
    uncontended pod at the fastest clean rate available for that image, and
    that rate is spelled out on the axis so no reader compares bars measured
    at different speeds without knowing it.
    """
    summary = pd.read_csv(os.path.join(SUMMARIES, "envelope-summary.csv"))
    # single-pod cells only: the readiness-probe fix and the campaign's
    # solo runs are both required for a cell to belong on this chart
    clean = summary[(summary["probe_period_s"] == 1) & (summary["campaign"] == "solo")]

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

    # the median spread between cold pods that started together in the same
    # repetition, drawn as an error bar rather than stacked, because it is a
    # measure of how much the total wobbles and not a component of it; most
    # of these cells are nominally single pods and show none, but a handful
    # still carry a nonzero spread, so it is read from the data rather than
    # assumed away
    ax.errorbar(labels, bottoms, yerr=spreads, fmt="none", ecolor="#e69f00",
                elinewidth=1.3, capsize=3, zorder=3,
                label="spread across pods that cold-started together in the same repetition")

    ax.set_ylabel("Cold start (s)")
    ax.tick_params(axis="x", labelsize=7.5)
    ax.legend(fontsize=7, ncol=1, loc="upper center", bbox_to_anchor=(0.5, -0.18))
    fig.tight_layout()
    fig.savefig(os.path.join(OUT, "eval_floor_decomposition.pdf"), bbox_inches="tight")
    plt.close(fig)


# bandwidth_sweep is deliberately absent: the thesis no longer includes that
# figure, and calling it here would only leave an unused file behind
def main():
    cold_by_config()
    distance_anchors()
    distance_sweep()
    crossimage_reuse()
    envelope()
    floor_decomposition()
    print("figures written to", OUT)


if __name__ == "__main__":
    main()
