"""End-to-end training on a small synthetic dataset in the `featurejob dataset` format."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import numpy as np
import onnxruntime as ort
import pandas as pd
import pytest

from training import export, train

FEATURES = [
    "rrf_score", "bm25_norm", "cosine_text", "cosine_image", "retriever_count", "policy_boost",
    "price_minor_log", "rating", "rating_count_log", "listing_completeness", "image_count",
    "age_days", "ctr_7d", "ctr_30d", "cvr_7d", "cvr_30d", "orders_30d_log", "vendor_score",
    "vendor_on_time_dispatch_rate", "vendor_dispute_rate", "vendor_response_hours",
    "vendor_verified", "query_category_match",
]  # fmt: skip


def synthetic(path: Path, requests: int = 600, per: int = 20) -> None:
    """Labels depend on rating and ctr_7d, which the baseline barely weighs."""
    rng = np.random.default_rng(1)
    rows = []
    for r in range(requests):
        split = "train" if r < requests * 0.8 else "valid" if r < requests * 0.9 else "test"
        x = rng.random((per, len(FEATURES)))
        x[:, FEATURES.index("policy_boost")] = 0
        utility = 2 * x[:, FEATURES.index("rating")] + 2 * x[:, FEATURES.index("ctr_7d")]
        label = np.clip(np.round(utility - 1.6 + rng.normal(0, 0.3, per)), 0, 3).astype(int)
        base = x[:, FEATURES.index("rrf_score")] + 0.1 * x[:, FEATURES.index("rating")]
        for i in range(per):
            rows.append(
                [
                    f"r{r:05d}",
                    1000 * r,
                    split,
                    f"p{r}_{i}",
                    i + 1,
                    label[i],
                    r % 10 == 0,
                    base[i],
                    base[i],
                    *x[i],
                ]
            )
    cols = [
        "request_id",
        "ts",
        "split",
        "product_id",
        "position",
        "label",
        "randomized",
        "baseline_prerank",
        "baseline_rank",
    ]
    pd.DataFrame(rows, columns=cols + FEATURES).to_csv(path, index=False)


def test_train_beats_baseline_and_exports(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    data = tmp_path / "ds.csv"
    synthetic(data)
    out, reports = tmp_path / "out", tmp_path / "reports"
    argv = ["train", "--data", str(data), "--out", str(out), "--reports", str(reports)]
    monkeypatch.setattr(sys, "argv", argv)
    train.main()

    for model_id, n in (("prerank-gbdt-v1", 15), ("rank-gbdt-v1", len(FEATURES) - 1)):
        manifest = json.loads((out / model_id / "features.json").read_text())
        assert manifest["id"] == model_id and len(manifest["features"]) == n
        assert "policy_boost" not in manifest["features"]
        assert manifest["metrics"]["test_ndcg10"] > manifest["metrics"]["baseline_ndcg10"]
        sess = ort.InferenceSession(str(out / model_id / "model.onnx"))
        assert [i.name for i in sess.get_inputs()] == [export.INPUT]
        scores = sess.run([export.OUTPUT], {export.INPUT: np.zeros((3, n), np.float32)})[0]
        assert scores.shape == (3,)
    report = next(reports.glob("*_ranking.md")).read_text()
    assert (
        "rank-gbdt-v1" in report and "95% CI" in report and "inverse propensity weighted" in report
    )


def test_per_request_metrics() -> None:
    df = pd.DataFrame({
        "request_id": ["a"] * 3 + ["b"] * 2,
        "product_id": ["x", "y", "z", "u", "v"],
        "position": [1, 2, 3, 1, 2],
        "label": [0, 3, 0, 0, 0],
        "score": [0.1, 0.9, 0.5, 1.0, 0.0],
    })  # fmt: skip
    per = train.per_request(df, "score").set_index("request_id")
    assert per.loc["a", "ndcg10"] == 1.0 and per.loc["a", "rr"] == 1.0
    assert per.loc["b", "ndcg10"] == 0.0 and per.loc["b", "rr"] == 0.0
    lo, hi = train.bootstrap_diff(np.array([1.0, 1.0]), np.array([0.0, 0.0]), seed=0)
    assert lo == hi == 1.0


def test_fit_eta_recovers_examination_curve() -> None:
    rng = np.random.default_rng(3)
    rows = []
    for r in range(3000):
        for pos in range(1, 21):
            clicked = rng.random() < 0.4 * pos**-0.7
            rows.append(
                {"request_id": f"r{r}", "position": pos, "label": int(clicked), "randomized": True}
            )
    eta = train.fit_eta(pd.DataFrame(rows))
    assert abs(eta - 0.7) < 0.05, eta


def test_ips_weights_deep_clicks() -> None:
    # Clicks at positions 1 and 4; a ranker that puts the deep click first wins once position
    # bias is corrected (gain at pos 4 weighs 4^0.7 ≈ 2.6).
    df = pd.DataFrame({
        "request_id": ["a"] * 4,
        "product_id": ["w", "x", "y", "z"],
        "position": [1, 2, 3, 4],
        "label": [1, 0, 0, 1],
        "logged": [4.0, 3.0, 2.0, 1.0],
        "deep_first": [3.0, 2.0, 1.0, 4.0],
    })  # fmt: skip
    plain_gap = (
        train.per_request(df, "deep_first").ndcg10[0] - train.per_request(df, "logged").ndcg10[0]
    )
    ips_logged = train.per_request(df, "logged", eta=0.7).ndcg10[0]
    ips_deep = train.per_request(df, "deep_first", eta=0.7).ndcg10[0]
    assert ips_deep == pytest.approx(1.0)
    assert ips_deep - ips_logged > plain_gap > 0
