"""Helpers that compute bootstrap confidence intervals for benchmark cells"""

import numpy as np

RESAMPLES = 10_000
SEED = 20261101


def _generator():
    return np.random.default_rng(SEED)


def repetition_groups(frame, column="ready_seconds"):
    return [group[column].to_numpy(dtype=float) for _, group in frame.groupby("rep", sort=True)]


def _as_groups(values):
    groups = [np.atleast_1d(np.asarray(group, dtype=float)) for group in values]
    sums = np.array([group.sum() for group in groups])
    counts = np.array([len(group) for group in groups])
    return sums, counts


def pooled_mean(values):
    sums, counts = _as_groups(values)
    return float(sums.sum() / counts.sum())


def _resampled_means(values, generator):
    sums, counts = _as_groups(values)
    picks = generator.integers(0, len(sums), size=(RESAMPLES, len(sums)))
    return sums[picks].sum(axis=1) / counts[picks].sum(axis=1)


def _interval(samples):
    low, high = np.percentile(samples, [2.5, 97.5])
    return float(low), float(high)


def mean_interval(values):
    if len(values) < 2:
        return float("nan"), float("nan")
    return _interval(_resampled_means(values, _generator()))


def ratio_interval(numerator, denominator):
    generator = _generator()
    estimate = pooled_mean(numerator) / pooled_mean(denominator)
    samples = _resampled_means(numerator, generator) / _resampled_means(denominator, generator)
    return (estimate, *_interval(samples))


def difference_interval(first, second):
    generator = _generator()
    estimate = pooled_mean(first) - pooled_mean(second)
    samples = _resampled_means(first, generator) - _resampled_means(second, generator)
    return (estimate, *_interval(samples))


def removed_share_interval(cold_a, warm_a, cold_b, warm_b):
    generator = _generator()
    above_a = pooled_mean(cold_a) - pooled_mean(warm_a)
    above_b = pooled_mean(cold_b) - pooled_mean(warm_b)
    estimate = 1 - above_b / above_a
    samples_a = _resampled_means(cold_a, generator) - _resampled_means(warm_a, generator)
    samples_b = _resampled_means(cold_b, generator) - _resampled_means(warm_b, generator)
    return (estimate, *_interval(1 - samples_b / samples_a))
