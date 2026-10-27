"""Ranking eval (docs/DATA.md §7): held-out synthetic sessions scored by the hidden utility.

The simulator (tools/dataprep/simulate.py) decides clicks from a hidden utility per (query,
user, product):

    utility = σ(2·relevance + 2·quality + 1.5·affinity + 1·price_match − 2)

with relevance the query's BM25 score normalized by its best match, quality the hidden item
quality, affinity the user's weight for the product's top category, and price_match the
distance between the product's and the user's price bands. This suite replays the text
requests of the held-out time split (the latest 10% of sessions) through search-api with the
session's user, and scores each top 10 by that utility: nDCG@10 with real-valued gains, the
ideal being the best 10 of the pool (the engine's top 50 plus the simulator's BM25 top 50,
retrievable products only). The logged order (what the simulated shoppers saw: BM25) is scored
the same way for comparison. Utility uses hidden data the engine never sees.
"""

from __future__ import annotations

import gzip
import json
import math
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path

from dataprep.simulate import bm25_search, build_bm25_index, price_match_score, tokenize
from eval import metrics
from eval.client import DATA, Client
from eval.image import retrievable

POOL = 50
# The held-out share of sessions, by time (the M7 training split uses the same 80/10/10).
HELD_OUT = 0.10
# Click-model weights; data/simulation_params.json records the ones the dataset was made with.
WEIGHTS = {"relevance": 2.0, "quality": 2.0, "affinity": 1.5, "price_match": 1.0, "intercept": -2.0}


@dataclass
class Session:
    request_id: str
    ts: str
    query: str
    user_id: str


@dataclass
class Result:
    session: Session
    ranked: list[str] = field(default_factory=list)
    logged: list[str] = field(default_factory=list)
    gains: dict[str, float] = field(default_factory=dict)
    pool: list[str] = field(default_factory=list)
    latency_ms: float = 0.0
    degraded: bool = False


class Utility:
    """The simulator's hidden utility for any (query, user, product)."""

    def __init__(self, data: Path = DATA):
        self.products = []
        with open(data / "products.canonical.jsonl", encoding="utf-8") as f:
            for line in f:
                if line.strip():
                    self.products.append(json.loads(line))
        self.index = {p["product_id"]: i for i, p in enumerate(self.products)}
        self.bm25 = build_bm25_index(self.products)
        self.quality = {}
        with open(data / "hidden" / "item_quality.jsonl", encoding="utf-8") as f:
            for line in f:
                r = json.loads(line)
                self.quality[r["product_id"]] = float(r.get("quality", r.get("q", 0.5)))
        self.users = {}
        with open(data / "users.jsonl", encoding="utf-8") as f:
            for line in f:
                u = json.loads(line)
                self.users[u["user_id"]] = u
        params = data / "simulation_params.json"
        self.weights = WEIGHTS | (
            json.loads(params.read_text()).get("click_weights", {}) if params.exists() else {}
        )

    def logged(self, query: str) -> list[tuple[str, float]]:
        """The simulator's candidates for a text query: BM25 top 50, relevance normalized."""
        hits = bm25_search(self.bm25, tokenize(query), top_k=len(self.products))
        if not hits:
            return []
        best = max(hits[0][1], 1e-6)
        return [(self.products[i]["product_id"], s / best) for i, s in hits]

    def gains(self, query: str, user_id: str, product_ids: list[str]) -> dict[str, float]:
        rel = dict(self.logged(query))
        user = self.users.get(user_id, {"category_affinities": {}, "price_band": "mid"})
        w = self.weights
        out = {}
        for pid in product_ids:
            i = self.index.get(pid)
            if i is None:
                continue
            p = self.products[i]
            top_cat = (p.get("category_path") or [""])[0]
            logit = (
                w["relevance"] * rel.get(pid, 0.0)
                + w["quality"] * self.quality.get(pid, 0.5)
                + w["affinity"] * user["category_affinities"].get(top_cat, 0.0)
                + w["price_match"] * price_match_score(p.get("price_minor", 0), user["price_band"])
                + w["intercept"]
            )
            out[pid] = 1.0 / (1.0 + math.exp(-logit))
        return out

    def retrievable(self, pid: str) -> bool:
        i = self.index.get(pid)
        return i is not None and retrievable(self.products[i])


def held_out_sessions(data: Path = DATA, limit: int = 0) -> list[Session]:
    rows = []
    with gzip.open(data / "events" / "requests.jsonl.gz", "rt", encoding="utf-8") as f:
        for line in f:
            r = json.loads(line)
            rows.append(r)
    rows.sort(key=lambda r: r["ts"])
    tail = rows[int(len(rows) * (1 - HELD_OUT)) :]
    out = [
        Session(r["request_id"], r["ts"], r["query"], r["user_id"])
        for r in tail
        if r["type"] == "text" and r["query"]
    ]
    return out[:limit] if limit else out


def run(client: Client, util: Utility, sessions: list[Session], workers: int = 4) -> list[Result]:
    def one(s: Session) -> Result:
        t0 = time.perf_counter()
        body = client.text_search(s.query, POOL, user_id=s.user_id)
        r = Result(s, latency_ms=(time.perf_counter() - t0) * 1000, degraded=body["degraded"])
        r.ranked = [x["product_id"] for x in body["results"]]
        r.logged = [pid for pid, _ in util.logged(s.query)[:POOL] if util.retrievable(pid)]
        r.pool = list(dict.fromkeys(r.ranked + r.logged))
        r.gains = util.gains(s.query, s.user_id, r.pool)
        return r

    with ThreadPoolExecutor(workers) as pool:
        return list(pool.map(one, sessions))


def summarize(results: list[Result]) -> dict:
    n = len(results)
    if n == 0:
        return {"queries": 0}

    def mean(f):
        return sum(f(r) for r in results) / n

    def util_at(ranked: list[str], gains: dict[str, float]) -> float:
        top = ranked[:10]
        return sum(gains.get(p, 0.0) for p in top) / 10

    lat = [r.latency_ms for r in results]
    return {
        "queries": n,
        "engine_ndcg@10": mean(lambda r: metrics.gain_ndcg_at(r.ranked, r.gains, 10, r.pool)),
        "logged_ndcg@10": mean(lambda r: metrics.gain_ndcg_at(r.logged, r.gains, 10, r.pool)),
        "engine_utility@10": mean(lambda r: util_at(r.ranked, r.gains)),
        "logged_utility@10": mean(lambda r: util_at(r.logged, r.gains)),
        "zero_results": sum(1 for r in results if not r.ranked),
        "degraded": sum(1 for r in results if r.degraded),
        "latency_p50_ms": metrics.percentile(lat, 50),
        "latency_p95_ms": metrics.percentile(lat, 95),
    }


def report_markdown(summary: dict, config_version: int, workers: int) -> str:
    s, v = summary, config_version
    d = s["engine_ndcg@10"] - s["logged_ndcg@10"]
    return "\n".join(
        [
            "## Ranking (held-out sessions, hidden utility)",
            "",
            f"{s['queries']} text sessions from the latest {HELD_OUT:.0%} of the simulated "
            f"traffic, replayed with their users through config v{v} ({workers} concurrent "
            "clients). Gains are the simulator's hidden click utility (relevance, item quality, "
            "the user's category affinity and price band); the ideal is the best 10 of the "
            f"engine's and the logged top {POOL}.",
            "",
            "| Ranker | nDCG@10 (utility) | Mean utility of the top 10 |",
            "|---|---|---|",
            f"| **Engine (config v{v})** | **{s['engine_ndcg@10']:.4f}** "
            f"| {s['engine_utility@10']:.4f} |",
            f"| Logged order (simulator BM25) | {s['logged_ndcg@10']:.4f} "
            f"| {s['logged_utility@10']:.4f} |",
            "",
            f"Δ nDCG@10 engine vs logged: {d:+.4f}. Zero-result sessions: {s['zero_results']}; "
            f"degraded responses: {s['degraded']}; latency p50 / p95 "
            f"{s['latency_p50_ms']:.0f} / {s['latency_p95_ms']:.0f} ms.",
            "",
            "The model comparison on the offline test split (learned vs `default_linear`) is in "
            "`tools/eval/reports/2026-10-26_ranking.md`.",
            "",
        ]
    )
