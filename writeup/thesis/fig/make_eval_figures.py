"""Generate the evaluation-chapter figures from the benchmark CSVs.

Reads the raw campaign CSVs from analysis/results/benchmarks and writes
publication PDFs into this directory. Run with:
uv run --with matplotlib,pandas make_eval_figures.py
"""

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

BENCH = os.path.join(
    os.path.dirname(__file__),
    "../../../analysis/results/benchmarks",
)
OUT = os.path.dirname(os.path.abspath(__file__))
SUMMARIES = os.path.join(
    os.path.dirname(__file__),
    "../../../analysis/results/summaries",
)

# The guarded managed-cloud campaign and its cross-region anchors
CLOUD_PREFIX = "aks2-scaleout"
# Where the managed-cloud figure cuts its axis
CLOUD_AXIS_TOP = 20

# Interval estimates come from the shared benchmark helper
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "../../../scripts"))
from cell_bootstrap import mean_interval, repetition_groups  # noqa: E402

# One fixed color per configuration, identical across every figure
CONFIG_COLORS = {
    1: "#0072b2",
    2: "#e69f00",
    3: "#56b4e9",
    4: "#009e73",
    "2s": "#cc79a7",
}
# The managed-cloud campaign has only the four numbered configurations
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
    pattern = os.path.join(BENCH, f"{prefix}-config{config}-{image}-*.csv")
    files = sorted(glob.glob(pattern))
    if not files:
        return None
    return pd.read_csv(files[-1])


def phase_stats(frame, phase, warm):
    rows = frame[(frame["phase"] == phase) & (frame["warm"] == warm)]
    values = rows["ready_seconds"]
    return values.mean(), values.min(), values.max()


# Mean of a phase with its 95 percent bootstrap interval, so the error bar
# shows how far a repeat of the cell could land rather than the extremes
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

        # A bar far above the axis cut is drawn to the cut and labeled with
        # its value, so the other bars stay readable
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
        # short black tick marking the matching warm floor on each bar
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


# The distance sweep measured every configuration in one session with the
# one-second probe, and plain Nydus at its defaults in the session after it
DISTANCE_PREFIX = "distance-clean"
DEFAULTS_PREFIX = "distance-defaults100k"
DISTANCE_RTTS = [0, 10, 25, 50, 80, 100]

# Only the measurement itself ends in its bare timestamp; drain records,
# prefetch verdicts and set-aside cells carry a further suffix
MEASUREMENT_FILE = re.compile(r"-\d{8}T\d{6}\.csv$")


def newest_measurement(pattern):
    files = sorted(path for path in glob.glob(os.path.join(BENCH, pattern))
                   if MEASUREMENT_FILE.search(path))
    if not files:
        return None
    return pd.read_csv(files[-1])


def sweep_stats(frame, warm):
    values = frame[(frame["phase"] == "scaleout") & (frame["warm"] == warm)]["ready_seconds"]
    return values.median(), values.min(), values.max()


def distance_series(image):
    """Collects one line per configuration of the distance sweep for one image.

    Each line holds the median cold start per round trip and the range of the
    cold pods as its band. Plain Nydus at its defaults (zstd conversion with
    prefetch on) comes from its own session and is drawn dashed.
    """
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
            median, low, high = sweep_stats(frame, "no")
            xs.append(rtt)
            medians.append(median)
            lows.append(low)
            highs.append(high)
        if xs:
            series.append((label, color, style, marker, xs, medians, lows, highs))
    return series


def distance_sweep():
    """Draw the cold start of every configuration over registry distance.

    Configuration 2 climbs to about 45 s while the others stay below 18 s, so
    the time axis is logarithmic, which keeps the lower lines apart.
    """
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
    fig.savefig(os.path.join(OUT, "eval_distance_sweep.pdf"), bbox_inches="tight")
    plt.close(fig)


# Where the same-session one-pod and two-pod pair is drawn, just left of the
# zero-delay points so it does not sit on top of them
REPLICA_TICK_MS = -4


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
    # eager, netem and anchor cells. Every point on every line is two-pod.
    # Just left of 0 ms sits a separate measurement in which one pod and two
    # pods ran back to back in the same session, so its tick is the cost of
    # the second replica without a difference between sessions
    replicas = pd.read_csv(os.path.join(SUMMARIES, "replica-summary.csv"))
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

            # the same-session pair, drawn faint so the two-pod line stays
            # the thing the eye follows: hollow for one pod, filled for two
            pair = replicas[(replicas["image"] == image) & (replicas["config"] == config)]
            if not pair.empty:
                solo_value = float(pair["solo_median_s"].iloc[0])
                pair_value = float(pair["pair_median_s"].iloc[0])
                ax_rtt.plot([REPLICA_TICK_MS], [solo_value], marker="o", markersize=3.5,
                            markerfacecolor="none", markeredgecolor=CONFIG_COLORS[config],
                            alpha=0.6, zorder=1)
                ax_rtt.plot([REPLICA_TICK_MS], [pair_value], marker="o", markersize=2.5,
                            color=CONFIG_COLORS[config], alpha=0.6, zorder=1)
                ax_rtt.plot([REPLICA_TICK_MS, REPLICA_TICK_MS], [solo_value, pair_value],
                            color=CONFIG_COLORS[config], linestyle=":", linewidth=0.9,
                            alpha=0.5, zorder=1)
                second_pod_tax[config].append(float(pair["second_replica_cost_s"].iloc[0]))

        # the final gap between the two lines, so the reader has a number
        # for how far ahead the cache still is once both curves reach 80 ms
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
    # rounded first, so a cost of -0.005 s prints as 0.0 rather than -0.0
    costs = {config: [round(value, 1) + 0.0 for value in values]
             for config, values in second_pod_tax.items()}
    tax_text = (f"cost of the second pod at 0 ms (means, one session):\n"
               f"eager {min(costs[1]):.1f} to {max(costs[1]):.1f} s\n"
               f"cache {min(costs[4]):.1f} to {max(costs[4]):.1f} s")
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
# figure, and calling it here would only leave an unused file behind. The
# cache-ceiling figure was removed for the same reason; its numbers are in
# the text and in summaries/cache-saturation-summary.csv
def main():
    cold_by_config()
    distance_sweep()
    envelope()
    floor_decomposition()
    print("figures written to", OUT)


if __name__ == "__main__":
    main()
