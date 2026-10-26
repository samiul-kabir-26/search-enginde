"""Synthetic users and interaction events simulator (docs/DATA.md §5).

    uv run python -m dataprep.simulate --seed 42

Reads data/products.canonical.jsonl and data/hidden/item_quality.jsonl, and writes:
- data/users.jsonl (hidden ground truth: affinities, price band, region; never used as features)
- data/events/part-*.jsonl.gz (~2M impressions plus clicks, carts and orders, in the
  POST /v1/events schema; impressions of the 10% randomized sessions carry "randomized": true)
- data/simulation_params.json
and records the results in data/MANIFEST.json. Event IDs come from the seeded RNG, so the same
seed produces byte-identical files. Timestamps end at --ref-date; `seed --events` shifts them to
the present.
"""

from __future__ import annotations

import argparse
import gzip
import io
import json
import math
import re
import uuid
from collections import defaultdict
from datetime import datetime, timedelta
from pathlib import Path

import numpy as np

from dataprep import manifest

ROOT = Path(__file__).resolve().parents[2]
DATA = ROOT / "data"

REGIONS = ["US", "CA", "GB", "IN", "AU", "DE", "FR", "JP", "MX", "AE"]
PRICE_BANDS = [
    ("budget", 0, 2000),
    ("economy", 2000, 5000),
    ("mid", 5000, 10000),
    ("premium", 10000, 25000),
    ("luxury", 25000, 100000000),
]
PRICE_BAND_WEIGHTS = [0.30, 0.35, 0.20, 0.10, 0.05]


def tokenize(text: str) -> list[str]:
    return [w.lower() for w in re.findall(r"\w+", text) if len(w) > 1]


def build_bm25_index(products: list[dict]):
    """Builds a lightweight in-memory BM25 index over products for candidate retrieval."""
    n_docs = len(products)
    doc_lens = []
    inv_index = defaultdict(list)
    doc_terms = []

    for i, p in enumerate(products):
        parts = [p.get("title", ""), p.get("brand", ""), p.get("product_type", "")]
        for vals in p.get("attributes", {}).values():
            if isinstance(vals, list):
                parts.extend(str(v) for v in vals)
        tokens = tokenize(" ".join(parts))
        doc_lens.append(len(tokens))
        tf = defaultdict(int)
        for t in tokens:
            tf[t] += 1
        doc_terms.append(tf)
        for t in tf:
            inv_index[t].append(i)

    avgdl = sum(doc_lens) / max(n_docs, 1)
    idf = {}
    for term, doc_list in inv_index.items():
        df = len(doc_list)
        idf[term] = math.log((n_docs - df + 0.5) / (df + 0.5) + 1.0)

    return {
        "inv_index": inv_index,
        "idf": idf,
        "doc_lens": doc_lens,
        "doc_terms": doc_terms,
        "avgdl": avgdl,
        "n_docs": n_docs,
    }


def bm25_search(
    index: dict, query_terms: list[str], top_k: int = 50, k1: float = 1.2, b: float = 0.75
) -> list[tuple[int, float]]:
    scores = defaultdict(float)
    inv = index["inv_index"]
    idf = index["idf"]
    doc_lens = index["doc_lens"]
    doc_terms = index["doc_terms"]
    avgdl = index["avgdl"]

    for term in query_terms:
        if term not in inv:
            continue
        term_idf = idf[term]
        for doc_id in inv[term]:
            tf = doc_terms[doc_id][term]
            doc_len = doc_lens[doc_id]
            denom = tf + k1 * (1.0 - b + b * (doc_len / avgdl))
            scores[doc_id] += term_idf * (tf * (k1 + 1.0)) / max(denom, 1e-6)

    if not scores:
        return []
    ranked = sorted(scores.items(), key=lambda kv: kv[1], reverse=True)
    return ranked[:top_k]


def get_price_band(price_minor: int) -> str:
    for name, low, high in PRICE_BANDS:
        if low <= price_minor <= high:
            return name
    return "mid"


def price_match_score(prod_price: int, user_band: str) -> float:
    prod_band = get_price_band(prod_price)
    if prod_band == user_band:
        return 1.0
    names = [b[0] for b in PRICE_BANDS]
    idx_p = names.index(prod_band) if prod_band in names else 2
    idx_u = names.index(user_band) if user_band in names else 2
    dist = abs(idx_p - idx_u)
    return max(0.0, 1.0 - 0.3 * dist)


def generate_users(rng: np.random.Generator, n_users: int, top_categories: list[str]) -> list[dict]:
    users = []
    n_cats = len(top_categories)
    band_names = [b[0] for b in PRICE_BANDS]

    for i in range(n_users):
        k = int(rng.integers(2, 6))
        chosen_cat_indices = rng.choice(n_cats, size=min(k, n_cats), replace=False)
        dirichlet_weights = rng.dirichlet(np.ones(len(chosen_cat_indices)) * 0.5)

        affinities = {}
        for cat_idx, w in zip(chosen_cat_indices, dirichlet_weights, strict=True):
            affinities[top_categories[cat_idx]] = round(float(w), 4)

        band = str(rng.choice(band_names, p=PRICE_BAND_WEIGHTS))
        region = str(rng.choice(REGIONS))
        users.append(
            {
                "user_id": f"u_{i + 1:05d}",
                "category_affinities": affinities,
                "price_band": band,
                "region": region,
            }
        )
    return users


def event_id(rng: np.random.Generator) -> str:
    """A UUIDv4 drawn from the seeded RNG (uuid.uuid4 would break determinism)."""
    return str(uuid.UUID(bytes=rng.bytes(16), version=4))


def iso(dt: datetime) -> str:
    return dt.strftime("%Y-%m-%dT%H:%M:%S.") + f"{dt.microsecond // 1000:03d}Z"


def sigmoid(x: float) -> float:
    return 1.0 / (1.0 + math.exp(-max(min(x, 15.0), -15.0)))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seed", type=int, default=42, help="RNG seed (default 42)")
    parser.add_argument("--data", type=Path, default=DATA, help="dataset dir")
    parser.add_argument("--users", type=int, default=5000, help="number of users")
    parser.add_argument(
        "--sessions", type=int, default=40000, help="number of sessions (~2M impressions)"
    )
    parser.add_argument("--parts", type=int, default=8, help="number of gzip output parts")
    parser.add_argument("--ref-date", default="2026-10-25T12:00:00Z", help="reference anchor date")
    args = parser.parse_args()

    rng = np.random.default_rng(args.seed)

    # 1. Load canonical products and item qualities
    prod_path = args.data / "products.canonical.jsonl"
    if not prod_path.exists():
        raise SystemExit(f"error: {prod_path} does not exist (run `make data` first)")

    products = []
    with open(prod_path) as f:
        for line in f:
            if line.strip():
                products.append(json.loads(line))

    # Item quality lookup
    quality_path = args.data / "hidden" / "item_quality.jsonl"
    item_quality = {}
    if quality_path.exists():
        with open(quality_path) as f:
            for line in f:
                if line.strip():
                    r = json.loads(line)
                    item_quality[r["product_id"]] = float(r.get("quality", r.get("q", 0.5)))
    else:
        for p in products:
            item_quality[p["product_id"]] = float(rng.beta(2, 2))

    # Collect top categories
    top_categories_set = set()
    category_to_prods = defaultdict(list)
    for idx, p in enumerate(products):
        cp = p.get("category_path")
        if cp and len(cp) > 0:
            top_cat = cp[0]
            top_categories_set.add(top_cat)
            category_to_prods[top_cat].append(idx)
        for cid in p.get("category_ids", []):
            category_to_prods[cid].append(idx)

    top_categories = sorted(top_categories_set)
    if not top_categories:
        top_categories = ["General"]

    # 2. Build index and query templates
    print("Building BM25 index over products...")
    bm25 = build_bm25_index(products)

    colors = sorted(
        {
            c
            for p in products
            for c in p.get("attributes", {}).get("color", [])
            if isinstance(c, str)
        }
    )
    materials = sorted(
        {
            m
            for p in products
            for m in p.get("attributes", {}).get("material", [])
            if isinstance(m, str)
        }
    )
    brands = sorted({p["brand"] for p in products if p.get("brand")})
    types = sorted({p["product_type"] for p in products if p.get("product_type")})

    # Generate synthetic users
    print(f"Generating {args.users} users...")
    users = generate_users(rng, args.users, top_categories)
    users_file = args.data / "users.jsonl"
    with open(users_file, "w") as f:
        for u in users:
            f.write(json.dumps(u) + "\n")

    # 3. Simulate sessions
    print(f"Simulating {args.sessions} sessions across {args.parts} partitions...")
    events_dir = args.data / "events"
    events_dir.mkdir(parents=True, exist_ok=True)

    ref_dt = datetime.fromisoformat(args.ref_date.replace("Z", "+00:00"))
    sessions_per_part = math.ceil(args.sessions / args.parts)

    part_writers = []
    part_files = []
    for part_idx in range(args.parts):
        pf = events_dir / f"part-{part_idx:05d}.jsonl.gz"
        part_files.append(pf)
        # mtime=0 keeps the gzip header, and so the file hash, identical across runs.
        part_writers.append(io.TextIOWrapper(gzip.GzipFile(pf, "wb", mtime=0), encoding="utf-8"))

    total_impressions = 0
    total_clicks = 0
    total_carts = 0
    total_orders = 0

    try:
        for s_idx in range(args.sessions):
            user = users[int(rng.integers(len(users)))]
            uid = user["user_id"]
            sid = f"s_{s_idx + 1:07d}"
            req_id = f"01J{s_idx + 1:023d}"
            part_target = part_writers[min(s_idx // sessions_per_part, args.parts - 1)]

            # Timestamp spread over past 30 days
            seconds_ago = float(rng.uniform(0, 30 * 86400))
            session_time = ref_dt - timedelta(seconds=seconds_ago)
            ts_iso = iso(session_time)

            is_browse = rng.random() < 0.30
            candidate_indices = []

            if is_browse:
                # Browse top category from user affinity or random
                user_affs = list(user["category_affinities"].keys())
                cat = (
                    user_affs[int(rng.integers(len(user_affs)))] if user_affs else top_categories[0]
                )
                pool = category_to_prods.get(cat, [])
                if len(pool) >= 50:
                    chosen = rng.choice(pool, size=50, replace=False)
                    candidate_indices = [(int(idx), 0.8) for idx in chosen]
                elif pool:
                    candidate_indices = [(int(idx), 0.8) for idx in pool]

            if len(candidate_indices) < 50:
                # Text query
                tmpl = int(rng.integers(4))
                if tmpl == 0 and colors and types:
                    q = f"{rng.choice(colors)} {rng.choice(types)}"
                elif tmpl == 1 and brands and types:
                    q = f"{rng.choice(brands)} {rng.choice(types)}"
                elif tmpl == 2 and materials and types:
                    q = f"{rng.choice(materials)} {rng.choice(types)}"
                elif types:
                    q = str(rng.choice(types))
                else:
                    q = "goods"

                tokens = tokenize(q)
                hits = bm25_search(bm25, tokens, top_k=50)
                max_bm25 = hits[0][1] if hits else 1.0
                candidate_indices = [(idx, score / max(max_bm25, 1e-6)) for idx, score in hits]

            # Fallback if too few candidates
            if len(candidate_indices) < 50:
                needed = 50 - len(candidate_indices)
                used = {idx for idx, _ in candidate_indices}
                avail = [i for i in range(len(products)) if i not in used]
                extra = rng.choice(avail, size=min(needed, len(avail)), replace=False)
                candidate_indices.extend((int(idx), 0.2) for idx in extra)

            # 10% randomized positions for propensity estimation (docs/DATA.md §5)
            is_randomized = rng.random() < 0.10
            if is_randomized:
                rng.shuffle(candidate_indices)

            # Examination and actions
            session_events = []
            for pos_0, (doc_idx, relevance) in enumerate(candidate_indices[:50]):
                pos = pos_0 + 1
                prod = products[doc_idx]
                pid = prod["product_id"]
                q_val = item_quality.get(pid, 0.5)

                top_cat = prod.get("category_path", [""])[0]
                affinity = user["category_affinities"].get(top_cat, 0.0)
                price_match = price_match_score(prod.get("price_minor", 0), user["price_band"])

                # Impression event (always generated for all 50 items)
                impression = {
                    "event_id": event_id(rng),
                    "type": "impression",
                    "ts": ts_iso,
                    "request_id": req_id,
                    "product_id": pid,
                    "position": pos,
                    "user_id": uid,
                    "session_id": sid,
                    "sponsored": False,
                }
                if is_randomized:
                    impression["randomized"] = True
                session_events.append(impression)
                total_impressions += 1
                # Actions follow the impression by a few seconds each.
                action_time = session_time + timedelta(seconds=float(rng.uniform(2, 60)))

                # Examination
                p_exam = 1.0 / (pos**0.7)
                if rng.random() >= p_exam:
                    continue

                # Click probability: σ(a·relevance + b·q + c·affinity + d·price_match − e)
                logit = 2.0 * relevance + 2.0 * q_val + 1.5 * affinity + 1.0 * price_match - 2.0
                if rng.random() < sigmoid(logit):
                    session_events.append(
                        {
                            "event_id": event_id(rng),
                            "type": "click",
                            "ts": iso(action_time),
                            "request_id": req_id,
                            "product_id": pid,
                            "position": pos,
                            "user_id": uid,
                            "session_id": sid,
                            "sponsored": False,
                        }
                    )
                    total_clicks += 1

                    # Add to cart
                    cart_logit = 1.5 * q_val + 1.0 * price_match - 1.5
                    if rng.random() < sigmoid(cart_logit):
                        session_events.append(
                            {
                                "event_id": event_id(rng),
                                "type": "add_to_cart",
                                "ts": iso(action_time + timedelta(seconds=30)),
                                "request_id": req_id,
                                "product_id": pid,
                                "position": pos,
                                "user_id": uid,
                                "session_id": sid,
                                "sponsored": False,
                            }
                        )
                        total_carts += 1

                        # Order
                        order_logit = 1.5 * q_val + 1.0 * price_match - 1.0
                        if rng.random() < sigmoid(order_logit):
                            qty = int(rng.integers(1, 4))
                            price = prod.get("price_minor", 2000)
                            session_events.append(
                                {
                                    "event_id": event_id(rng),
                                    "type": "order",
                                    "ts": iso(action_time + timedelta(seconds=120)),
                                    "request_id": req_id,
                                    "product_id": pid,
                                    "position": pos,
                                    "user_id": uid,
                                    "session_id": sid,
                                    "sponsored": False,
                                    "order_id": f"o_{sid}_{pos}",
                                    "quantity": qty,
                                    "value_minor": qty * price,
                                }
                            )
                            total_orders += 1

            for ev in session_events:
                part_target.write(json.dumps(ev) + "\n")

    finally:
        for w in part_writers:
            w.close()

    # Write simulation parameters
    sim_params = {
        "seed": args.seed,
        "users": args.users,
        "sessions": args.sessions,
        "parts": args.parts,
        "ref_date": args.ref_date,
        "impressions": total_impressions,
        "clicks": total_clicks,
        "add_to_cart": total_carts,
        "orders": total_orders,
        "randomized_share": 0.10,
        "exam_decay": 0.7,
        "click_weights": {
            "relevance": 2.0,
            "quality": 2.0,
            "affinity": 1.5,
            "price_match": 1.0,
            "intercept": -2.0,
        },
    }
    sim_file = args.data / "simulation_params.json"
    sim_file.write_text(json.dumps(sim_params, indent=2) + "\n")

    # Update manifest
    manifest.update(
        args.data,
        "simulate",
        sim_params,
        [users_file, sim_file, *part_files],
    )

    print("Simulation completed:")
    print(f"  Impressions: {total_impressions:,}")
    print(f"  Clicks:      {total_clicks:,} (CTR {total_clicks / max(total_impressions, 1):.2%})")
    print(f"  Carts:       {total_carts:,}")
    print(f"  Orders:      {total_orders:,} (CVR {total_orders / max(total_clicks, 1):.2%})")


if __name__ == "__main__":
    main()
