"""Image eval (docs/DATA.md §7): each eval product's alternate image is the query.

The relevant set is the product itself (grade 3) plus products with the same brand and
product_type (grade 1). Products the tenant's always_filters hide (inactive or out of stock)
cannot be retrieved, so queries for them are skipped and they never count as relevant.
"""

from __future__ import annotations

import json
import time
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path

from eval import metrics
from eval.client import DATA, Client

PAGE = 50


@dataclass
class Query:
    query_id: str
    product_id: str
    image_file: str
    grades: dict[str, int]


@dataclass
class Result:
    query: Query
    ranked: list[str] = field(default_factory=list)
    latency_ms: float = 0.0
    degraded: bool = False
    categories: list[dict] = field(default_factory=list)


def retrievable(p: dict) -> bool:
    return p["status"] == "active" and p["stock"] > 0


def build_queries(data: Path = DATA) -> list[Query]:
    """Writes data/eval/image_queries.jsonl from the eval seed and the canonical catalog."""
    products = {}
    with open(data / "products.canonical.jsonl", encoding="utf-8") as f:
        for line in f:
            p = json.loads(line)
            products[p["product_id"]] = p
    groups: dict[tuple[str, str], list[str]] = defaultdict(list)
    for p in products.values():
        if retrievable(p) and p["brand"]:
            groups[(p["brand"], p["product_type"])].append(p["product_id"])

    queries = []
    with open(data / "eval" / "image_eval_seed.jsonl", encoding="utf-8") as f:
        for line in f:
            seed = json.loads(line)
            p = products.get(seed["product_id"])
            if p is None or not retrievable(p):
                continue
            grades = {pid: 1 for pid in groups.get((p["brand"], p["product_type"]), [])}
            grades[p["product_id"]] = 3
            queries.append(
                Query(
                    f"img-{len(queries) + 1:04d}",
                    p["product_id"],
                    f"images/alt/{seed['image_id']}.jpg",
                    grades,
                )
            )
    with open(data / "eval" / "image_queries.jsonl", "w", encoding="utf-8") as f:
        for q in queries:
            f.write(json.dumps(q.__dict__, sort_keys=True) + "\n")
    return queries


def run(client: Client, queries: list[Query], workers: int = 4, data: Path = DATA) -> list[Result]:
    def one(q: Query) -> Result:
        image = (data / q.image_file).read_bytes()
        t0 = time.perf_counter()
        body = client.image_search(image, PAGE)
        r = Result(q, latency_ms=(time.perf_counter() - t0) * 1000)
        r.ranked = [x["product_id"] for x in body["results"]]
        r.degraded = body["degraded"]
        r.categories = body.get("predicted_categories", [])
        return r

    with ThreadPoolExecutor(workers) as pool:
        return list(pool.map(one, queries))


def summarize(results: list[Result]) -> dict:
    n = len(results)
    if n == 0:
        return {"queries": 0}

    def mean(f):
        return sum(f(r) for r in results) / n

    lat = [r.latency_ms for r in results]
    return {
        "queries": n,
        "exact_recall@10": mean(lambda r: metrics.hit_at(r.ranked, r.query.product_id, 10)),
        "exact_recall@50": mean(lambda r: metrics.hit_at(r.ranked, r.query.product_id, 50)),
        "mrr": mean(lambda r: metrics.reciprocal_rank(r.ranked, r.query.product_id)),
        "graded_recall@10": mean(lambda r: metrics.recall_at(r.ranked, set(r.query.grades), 10)),
        "graded_recall@50": mean(lambda r: metrics.recall_at(r.ranked, set(r.query.grades), 50)),
        "ndcg@10": mean(lambda r: metrics.ndcg_at(r.ranked, r.query.grades, 10)),
        "with_categories": mean(lambda r: 1.0 if r.categories else 0.0),
        "degraded": sum(1 for r in results if r.degraded),
        "latency_p50_ms": metrics.percentile(lat, 50),
        "latency_p95_ms": metrics.percentile(lat, 95),
        "latency_max_ms": max(lat),
    }


def report_markdown(summary: dict, results: list[Result], config_version: int, workers: int) -> str:
    misses = [r for r in results if r.query.product_id not in r.ranked[:10]][:5]
    lines = [
        "## Image search",
        "",
        f"{summary['queries']} queries (alternate catalog images of retrievable products), "
        f"page size {PAGE}, {workers} concurrent clients, config v{config_version}.",
        "",
        "| Metric | Value |",
        "|---|---|",
        f"| **Recall@10** (the product itself in the top 10) "
        f"| **{summary['exact_recall@10']:.3f}** |",
        f"| Recall@50 (the product itself in the top 50) | {summary['exact_recall@50']:.3f} |",
        f"| MRR (of the product itself) | {summary['mrr']:.3f} |",
        f"| Graded-set recall@10 (product + same brand and type) "
        f"| {summary['graded_recall@10']:.3f} |",
        f"| Graded-set recall@50 | {summary['graded_recall@50']:.3f} |",
        f"| nDCG@10 (grades 3 and 1) | {summary['ndcg@10']:.3f} |",
        f"| Queries with predicted categories | {summary['with_categories']:.1%} |",
        f"| Degraded responses | {summary['degraded']} |",
        f"| Latency p50 / p95 / max (client, local upload) | {summary['latency_p50_ms']:.0f} / "
        f"{summary['latency_p95_ms']:.0f} / {summary['latency_max_ms']:.0f} ms |",
        "",
    ]
    if misses:
        lines += ["Examples outside the top 10:", ""]
        for r in misses:
            lines.append(f"- `{r.query.product_id}` ({r.query.image_file}): top 3 {r.ranked[:3]}")
        lines.append("")
    return "\n".join(lines)
