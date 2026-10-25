"""Ranking metrics for the offline eval suites."""

from __future__ import annotations

import math


def recall_at(ranked: list[str], relevant: set[str], k: int) -> float:
    """Share of the relevant set found in the top k."""
    if not relevant:
        return 0.0
    return len(set(ranked[:k]) & relevant) / len(relevant)


def hit_at(ranked: list[str], target: str, k: int) -> float:
    """1 when target is in the top k, else 0."""
    return 1.0 if target in ranked[:k] else 0.0


def reciprocal_rank(ranked: list[str], target: str) -> float:
    """1/rank of target, or 0 when it was not returned."""
    try:
        return 1.0 / (ranked.index(target) + 1)
    except ValueError:
        return 0.0


def ndcg_at(ranked: list[str], grades: dict[str, int], k: int) -> float:
    """nDCG@k with gain 2^grade - 1."""

    def dcg(gs: list[int]) -> float:
        return sum((2**g - 1) / math.log2(i + 2) for i, g in enumerate(gs))

    ideal = dcg(sorted(grades.values(), reverse=True)[:k])
    if ideal == 0:
        return 0.0
    return dcg([grades.get(p, 0) for p in ranked[:k]]) / ideal


def percentile(xs: list[float], p: float) -> float:
    """Nearest-rank percentile (p in [0, 100])."""
    if not xs:
        return 0.0
    s = sorted(xs)
    return s[min(len(s) - 1, max(0, math.ceil(p / 100 * len(s)) - 1))]
