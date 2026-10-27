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


def test_capped_recall_and_gain_ndcg():
    ranked = ["a", "b", "c"]
    # 2 of 3 relevant found in the top 2, out of at most 2.
    assert metrics.capped_recall_at(ranked, {"a", "b", "z"}, 2) == 1
    assert metrics.capped_recall_at(ranked, {"c", "z"}, 2) == 0
    gains = {"a": 0.2, "b": 0.9, "c": 0.5}
    assert metrics.gain_ndcg_at(["b", "c", "a"], gains, 3, ["a", "b", "c"]) == pytest.approx(1)
    assert metrics.gain_ndcg_at(["a", "c", "b"], gains, 3, ["a", "b", "c"]) < 1
    assert metrics.gain_ndcg_at(["x"], {}, 3, ["x"]) == 0


def test_text_grades_and_queries(tmp_path: Path):
    from eval import text

    bag = {
        "product_type": "HANDBAG",
        "brand": "Acme",
        "price_minor": 4000,
        "attributes": {"color": ["Red"], "material": ["Leather"]},
    }
    c = {"type": "HANDBAG", "color": "red", "material": "leather"}
    assert text.grade(bag, c) == 3
    assert text.grade(bag | {"attributes": {"color": ["Red"]}}, c) == 1  # type + one of two
    assert text.grade(bag | {"attributes": {}}, c) == 0  # type alone is not partial here
    assert text.grade(bag | {"product_type": "WALLET"}, c) == 0
    cheap = {"type": "HANDBAG", "max_price_minor": 5000}
    assert text.grade(bag, cheap) == 3
    assert text.grade(bag | {"price_minor": 9000}, cheap) == 1  # two constraints: type alone
    assert text.type_text("STRING_LIGHT") == "string light"

    (tmp_path / "eval").mkdir()
    rows = [
        {
            "product_id": f"p{i}",
            "status": "active",
            "stock": 1,
            "product_type": "HANDBAG",
            "brand": "Acme",
            "price_minor": 1000 * (i + 1),
            "attributes": {"color": ["red" if i % 2 else "blue"], "material": ["leather"]},
        }
        for i in range(6)
    ] + [
        {
            "product_id": "hidden",
            "status": "inactive",
            "stock": 1,
            "product_type": "HANDBAG",
            "brand": "Acme",
            "price_minor": 100,
            "attributes": {"color": ["red"]},
        }
    ]
    (tmp_path / "products.canonical.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows))
    qs = text.build_queries(tmp_path, n=8, seed=1)
    assert qs == text.build_queries(tmp_path, n=8, seed=1)  # deterministic
    assert len({q.text for q in qs}) == len(qs)
    for q in qs:
        assert "hidden" not in q.grades
        assert any(g == 3 for g in q.grades.values())
