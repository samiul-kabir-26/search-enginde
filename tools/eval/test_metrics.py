import json
from pathlib import Path

import pytest

from eval import image, metrics


def test_metrics():
    ranked = ["a", "b", "c", "d"]
    assert metrics.hit_at(ranked, "c", 2) == 0 and metrics.hit_at(ranked, "c", 3) == 1
    assert metrics.reciprocal_rank(ranked, "b") == 0.5 and metrics.reciprocal_rank(ranked, "z") == 0
    assert metrics.recall_at(ranked, {"a", "d", "z"}, 2) == pytest.approx(1 / 3)
    assert metrics.recall_at(ranked, set(), 2) == 0
    assert metrics.ndcg_at(["x", "a"], {"a": 3}, 10) == pytest.approx(1 / 1.584962500721156)
    assert metrics.ndcg_at(["a"], {"a": 3, "b": 1}, 1) == 1
    assert metrics.percentile([5, 1, 3, 2, 4], 50) == 3 and metrics.percentile([], 95) == 0


def test_build_queries_and_summary(tmp_path: Path):
    (tmp_path / "eval").mkdir()
    rows = [
        {"product_id": "p1", "brand": "B", "product_type": "T", "status": "active", "stock": 3},
        {"product_id": "p2", "brand": "B", "product_type": "T", "status": "active", "stock": 1},
        {"product_id": "p3", "brand": "B", "product_type": "T", "status": "inactive", "stock": 1},
        {"product_id": "p4", "brand": "C", "product_type": "T", "status": "active", "stock": 0},
    ]
    (tmp_path / "products.canonical.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows))
    seeds = [{"product_id": "p1", "image_id": "i1"}, {"product_id": "p4", "image_id": "i4"}]
    (tmp_path / "eval" / "image_eval_seed.jsonl").write_text(
        "".join(json.dumps(s) + "\n" for s in seeds)
    )

    qs = image.build_queries(tmp_path)
    assert len(qs) == 1, "queries for products hidden by always_filters are skipped"
    assert qs[0].grades == {"p1": 3, "p2": 1}, "hidden products never count as relevant"
    assert (tmp_path / "eval" / "image_queries.jsonl").exists()

    results = [
        image.Result(qs[0], ranked=["p2", "p1"], latency_ms=40),
        image.Result(qs[0], ranked=["x"], latency_ms=60),
    ]
    s = image.summarize(results)
    assert (
        s["exact_recall@10"] == 0.5
        and s["mrr"] == pytest.approx(0.25)
        and s["latency_p95_ms"] == 60
    )
    assert "Recall@10" in image.report_markdown(s, results, 1, 4)
