"""Bootstrap interval estimates for benchmark cells.

Every cell holds a handful of repetitions, so a mean alone hides how far a
second run of the same cell could land. These helpers resample with
replacement and report the 95 percent percentile interval of the statistic.
The pods of one repetition share a reset, a seed and a start time, so they
are not independent; callers pass them grouped by repetition, and a whole
repetition is drawn at a time. A fixed seed keeps the published intervals
reproducible.
"""

import numpy as np

RESAMPLES = 10_000
SEED = 20261101


def _generator():
    return np.random.default_rng(SEED)


def repetition_groups(frame, column="ready_seconds"):
    """Splits the pods of one cell into one array of values per repetition."""
    return [group[column].to_numpy(dtype=float) for _, group in frame.groupby("rep", sort=True)]


def _as_groups(values):
    # A flat sequence of numbers counts every value as its own unit, which is
    # right for phases with one pod per repetition such as the seed
    groups = [np.atleast_1d(np.asarray(group, dtype=float)) for group in values]
    sums = np.array([group.sum() for group in groups])
    counts = np.array([len(group) for group in groups])
    return sums, counts


def pooled_mean(values):
    """Mean over every pod, whether the values come grouped or flat."""
    sums, counts = _as_groups(values)
    return float(sums.sum() / counts.sum())


def _resampled_means(values, generator):
    # Each resample draws as many repetitions as the cell has and averages
    # over all the pods they contain
    sums, counts = _as_groups(values)
    picks = generator.integers(0, len(sums), size=(RESAMPLES, len(sums)))
    return sums[picks].sum(axis=1) / counts[picks].sum(axis=1)


def _interval(samples):
    low, high = np.percentile(samples, [2.5, 97.5])
    return float(low), float(high)


def mean_interval(values):
    """Percentile interval of the mean of one cell's values."""
    if len(values) < 2:
        return float("nan"), float("nan")
    return _interval(_resampled_means(values, _generator()))


def ratio_interval(numerator, denominator):
    """Ratio of two means with an interval, the two cells resampled independently."""
    generator = _generator()
    estimate = pooled_mean(numerator) / pooled_mean(denominator)
    samples = _resampled_means(numerator, generator) / _resampled_means(denominator, generator)
    return (estimate, *_interval(samples))


def difference_interval(first, second):
    """Difference of two means (first minus second) with an interval."""
    generator = _generator()
    estimate = pooled_mean(first) - pooled_mean(second)
    samples = _resampled_means(first, generator) - _resampled_means(second, generator)
    return (estimate, *_interval(samples))


def removed_share_interval(cold_a, warm_a, cold_b, warm_b):
    """Share of configuration a's cost above its warm floor that configuration b removes.

    The cost above the floor is a cell's cold mean minus its warm mean, and
    all four sets of repetitions are resampled independently.
    """
    generator = _generator()
    above_a = pooled_mean(cold_a) - pooled_mean(warm_a)
    above_b = pooled_mean(cold_b) - pooled_mean(warm_b)
    estimate = 1 - above_b / above_a
    samples_a = _resampled_means(cold_a, generator) - _resampled_means(warm_a, generator)
    samples_b = _resampled_means(cold_b, generator) - _resampled_means(warm_b, generator)
    return (estimate, *_interval(1 - samples_b / samples_a))
