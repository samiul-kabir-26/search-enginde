"""Text eval (docs/DATA.md §7): template queries over catalog attributes with graded labels.

Templates, each seeded from a retrievable product so every query has at least one exact match:

- "{color} {material} {type}"
- "{brand} {type}"
- "{type} under {price}"
- "{color} {type}"

A product's grade is 3 when it matches every constraint of the query and 1 when it has the
query's product type plus a partial match (for two-constraint templates, the type alone).
Products the default always_filters hide (inactive or out of stock) are never relevant.

Metrics: nDCG@10 over the grades, recall@100 of the exact matches (capped at 100, so a query
with more than 100 exact matches can still reach 1; the result list is S6's output, which keeps
rank.keep candidates in ranked order and the rest in retrieval order, so it measures retrieval
at that depth), and MRR of the first exact match.
"""

from __future__ import annotations

import json
import time
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np

from eval import metrics
from eval.client import DATA, Client
from eval.image import retrievable

PAGE = 100
TEMPLATES = ("color_material_type", "brand_type", "type_under_price", "color_type")
PRICE_STEPS = [10, 25, 50, 100, 200, 500, 1000, 2500]


@dataclass
class Query:
    query_id: str
    template: str
    text: str
    constraints: dict
    grades: dict[str, int]


@dataclass
class Result:
    query: Query
    ranked: list[str] = field(default_factory=list)
    latency_ms: float = 0.0
    degraded: bool = False


def type_text(product_type: str) -> str:
    """STRING_LIGHT -> "string light"."""
    return product_type.replace("_", " ").lower().strip()


def first(p: dict, attr: str) -> str:
    vals = (p.get("attributes") or {}).get(attr) or []
    for v in vals:
        v = str(v).strip()
        # One or two plain words read like a shopper's term; codes and long phrases do not.
        if v and len(v) <= 20 and len(v.split()) <= 2 and v.replace(" ", "").isalpha():
            return v.lower()
    return ""


def has(p: dict, attr: str, value: str) -> bool:
    return any(value in str(v).lower() for v in (p.get("attributes") or {}).get(attr) or [])


def matches(p: dict, c: dict) -> dict[str, bool]:
    """Which of a query's constraints a product meets."""
    out = {"type": p.get("product_type") == c["type"]}
    if "color" in c:
        out["color"] = has(p, "color", c["color"])
    if "material" in c:
        out["material"] = has(p, "material", c["material"])
    if "brand" in c:
        out["brand"] = (p.get("brand") or "").lower() == c["brand"].lower()
    if "max_price_minor" in c:
        out["price"] = p.get("price_minor", 0) <= c["max_price_minor"]
    return out


def grade(p: dict, c: dict) -> int:
    m = matches(p, c)
    if all(m.values()):
        return 3
    others = [v for k, v in m.items() if k != "type"]
    if m["type"] and (len(others) == 1 or any(others)):
        return 1
    return 0


def build_queries(data: Path = DATA, n: int = 400, seed: int = 42) -> list[Query]:
    """Writes data/eval/text_queries.jsonl (deterministic for a seed and catalog)."""
    products = []
    with open(data / "products.canonical.jsonl", encoding="utf-8") as f:
        for line in f:
            p = json.loads(line)
            if retrievable(p) and p.get("product_type"):
                products.append(p)
    by_type: dict[str, list[dict]] = defaultdict(list)
    for p in products:
        by_type[p["product_type"]].append(p)

    rng = np.random.default_rng(seed)
    queries: list[Query] = []
    seen: set[str] = set()
    attempts = 0
    while len(queries) < n and attempts < n * 50:
        attempts += 1
        template = TEMPLATES[len(queries) % len(TEMPLATES)]
        p = products[int(rng.integers(len(products)))]
        t = p["product_type"]
        c: dict = {"type": t}
        if template == "color_material_type":
            color, material = first(p, "color"), first(p, "material")
            if not color or not material:
                continue
            c |= {"color": color, "material": material}
            text = f"{color} {material} {type_text(t)}"
        elif template == "brand_type":
            if not p.get("brand"):
                continue
            c["brand"] = p["brand"]
            text = f"{p['brand']} {type_text(t)}"
        elif template == "type_under_price":
            dollars = next((s for s in PRICE_STEPS if s * 100 >= p["price_minor"]), None)
            if dollars is None:
                continue
            c["max_price_minor"] = dollars * 100
            text = f"{type_text(t)} under ${dollars}"
        else:
            color = first(p, "color")
            if not color:
                continue
            c["color"] = color
            text = f"{color} {type_text(t)}"
        if text in seen:
            continue
        seen.add(text)
        grades = {q["product_id"]: g for q in by_type[t] if (g := grade(q, c)) > 0}
        queries.append(Query(f"txt-{len(queries) + 1:04d}", template, text, c, grades))

    with open(data / "eval" / "text_queries.jsonl", "w", encoding="utf-8") as f:
        for q in queries:
            f.write(json.dumps(q.__dict__, sort_keys=True) + "\n")
    return queries


def run(client: Client, queries: list[Query], workers: int = 4) -> list[Result]:
    def one(q: Query) -> Result:
        t0 = time.perf_counter()
        body = client.text_search(q.text, PAGE)
        r = Result(q, latency_ms=(time.perf_counter() - t0) * 1000)
        r.ranked = [x["product_id"] for x in body["results"]]
        r.degraded = body["degraded"]
        return r

    with ThreadPoolExecutor(workers) as pool:
        return list(pool.map(one, queries))


def exact(q: Query) -> set[str]:
    return {p for p, g in q.grades.items() if g == 3}


def summarize(results: list[Result]) -> dict:
    def block(rs: list[Result]) -> dict:
        n = len(rs)
        if n == 0:
            return {"queries": 0}

        def mean(f):
            return sum(f(r) for r in rs) / n

        def mrr(r: Result) -> float:
            want = exact(r.query)
            return next((1.0 / (i + 1) for i, p in enumerate(r.ranked) if p in want), 0.0)

        return {
            "queries": n,
            "ndcg@10": mean(lambda r: metrics.ndcg_at(r.ranked, r.query.grades, 10)),
            "recall@100": mean(lambda r: metrics.capped_recall_at(r.ranked, exact(r.query), 100)),
            "mrr": mean(mrr),
            "zero_results": sum(1 for r in rs if not r.ranked),
            "degraded": sum(1 for r in rs if r.degraded),
        }

    lat = [r.latency_ms for r in results]
    out = block(results) | {
        "latency_p50_ms": metrics.percentile(lat, 50),
        "latency_p95_ms": metrics.percentile(lat, 95),
        "latency_max_ms": max(lat) if lat else 0.0,
    }
    out["by_template"] = {
        t: block([r for r in results if r.query.template == t]) for t in TEMPLATES
    }
    return out


def report_markdown(summary: dict, results: list[Result], config_version: int, workers: int) -> str:
    worst = sorted(results, key=lambda r: metrics.ndcg_at(r.ranked, r.query.grades, 10))[:5]
    lines = [
        "## Text search",
        "",
        f"{summary['queries']} template queries over catalog attributes (graded: all "
        f"constraints 3, type plus a partial match 1), page size {PAGE}, {workers} concurrent "
        "clients, "
        f"config v{config_version}.",
        "",
        "| Metric | Value |",
        "|---|---|",
        f"| **nDCG@10** | **{summary['ndcg@10']:.3f}** |",
        f"| Recall@100 (exact matches, capped at 100) | {summary['recall@100']:.3f} |",
        f"| MRR (first exact match) | {summary['mrr']:.3f} |",
        f"| Zero-result queries | {summary['zero_results']} |",
        f"| Degraded responses | {summary['degraded']} |",
        f"| Latency p50 / p95 / max (client) | {summary['latency_p50_ms']:.0f} / "
        f"{summary['latency_p95_ms']:.0f} / {summary['latency_max_ms']:.0f} ms |",
        "",
        "| Template | Queries | nDCG@10 | Recall@100 | MRR |",
        "|---|---|---|---|---|",
    ]
    for t, b in summary["by_template"].items():
        if b["queries"]:
            lines.append(
                f"| `{t}` | {b['queries']} | {b['ndcg@10']:.3f} | {b['recall@100']:.3f} "
                f"| {b['mrr']:.3f} |"
            )
    lines += ["", "Lowest nDCG@10:", ""]
    for r in worst:
        lines.append(
            f'- "{r.query.text}" ({len(exact(r.query))} exact matches): top 3 {r.ranked[:3]}'
        )
    lines.append("")
    return "\n".join(lines)
