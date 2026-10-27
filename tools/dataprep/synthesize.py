"""Synthesize vendors and the commercial fields ABO lacks (docs/DATA.md §4).

    uv run python -m dataprep.synthesize --seed 42

Reads data/products.raw.jsonl and writes data/vendors.jsonl, data/products.canonical.jsonl and
the hidden ground truth in data/hidden/ (item and vendor quality, used only by the event
simulator and never exported as features).
"""

from __future__ import annotations

import argparse
import json
import math
import unicodedata
from datetime import UTC, datetime, timedelta
from pathlib import Path

import numpy as np
import yaml

from dataprep import manifest

ROOT = Path(__file__).resolve().parents[2]
DATA = ROOT / "data"
PRICE_PARAMS = Path(__file__).with_name("price_params.yaml")

REGIONS = ["US", "CA", "GB", "IN", "AU", "DE", "FR", "JP", "MX", "AE"]
CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"


def make_ulid(ts_ms: int, rand_bytes: bytes) -> str:
    """Encodes a 48-bit timestamp and 80-bit randomness into a 26-char Crockford Base32 ULID."""
    val = (ts_ms << 80) | int.from_bytes(rand_bytes, "big")
    chars = []
    for _ in range(26):
        chars.append(CROCKFORD[val & 0x1F])
        val >>= 5
    return "".join(reversed(chars))


NAME_A = [
    "North",
    "Blue",
    "Prime",
    "Urban",
    "Golden",
    "Bright",
    "Summit",
    "Harbor",
    "Cedar",
    "Atlas",
    "Silver",
    "Rapid",
    "Coastal",
    "Maple",
    "Pioneer",
    "Lumen",
    "Granite",
    "Velvet",
    "Orbit",
    "Willow",
]
NAME_B = [
    "Trading",
    "Supply",
    "Goods",
    "Imports",
    "Wholesale",
    "Outfitters",
    "Market",
    "Depot",
    "Works",
    "Collective",
]


def slug(s: str) -> str:
    """Mirrors catalog.Slug in Go: strip accents, lower-case, non-alphanumeric runs to '-'."""
    folded = "".join(c for c in unicodedata.normalize("NFKD", s) if not unicodedata.combining(c))
    out, dash = [], False
    for c in unicodedata.normalize("NFC", folded).lower():
        if c.isalpha() or c.isdigit():
            out.append(c)
            dash = False
        elif not dash and out:
            out.append("-")
            dash = True
    return "".join(out).rstrip("-")


def zipf_weights(n: int, s: float) -> np.ndarray:
    w = 1.0 / np.arange(1, n + 1) ** s
    return w / w.sum()


def vendor_name(i: int) -> str:
    a, b = NAME_A[i % len(NAME_A)], NAME_B[i // len(NAME_A) % len(NAME_B)]
    return f"{a} {b} {i + 1}"


def make_vendors(rng: np.random.Generator, n: int) -> list[dict]:
    vendors = []
    for i in range(n):
        home = REGIONS[int(rng.integers(len(REGIONS)))]
        extra = rng.choice(
            [r for r in REGIONS if r != home], size=int(rng.integers(0, 5)), replace=False
        )
        vendors.append(
            {
                "vendor_id": f"v_{i + 1:03d}",
                "name": vendor_name(i),
                "ship_regions": sorted({home, *extra.tolist()}),
            }
        )
    return vendors


def vendor_profile(rng: np.random.Generator, vq: float) -> dict:
    """Observable vendor attributes, correlated with hidden vendor quality vq in [0, 1]."""
    return {
        "verified": bool(rng.random() < 0.3 + 0.6 * vq),
        "tier": "gold" if vq + rng.normal(0, 0.1) > 0.65 else "basic",
        "on_time_dispatch_rate": round(
            float(np.clip(0.75 + 0.24 * vq + rng.normal(0, 0.03), 0.5, 0.999)), 4
        ),
        "dispute_rate": round(float(np.clip(0.08 * (1 - vq) + rng.normal(0, 0.01), 0.001, 0.2)), 4),
        "response_hours": round(
            float(np.clip(rng.lognormal(math.log(2 + 22 * (1 - vq)), 0.3), 0.5, 72)), 1
        ),
    }


def price_minor(rng: np.random.Generator, params: dict, ptype: str) -> int:
    p = params["types"].get(ptype, params["default"])
    usd = p["median_usd"] * math.exp(p["sigma"] * rng.standard_normal())
    usd = min(max(usd, params["min_usd"]), params["max_usd"])
    return int(round(usd * 100))


def completeness(p: dict) -> float:
    """Mirrors Product.completeness in Go."""
    present = [
        p.get("description"),
        p.get("brand"),
        p.get("product_type"),
        p.get("attributes"),
        p.get("image_files"),
        p.get("ship_regions"),
        p.get("rating") is not None,
    ]
    return sum(1 for x in present if x) / len(present)


def synthesize(raw: list[dict], seed: int, n_vendors: int, ref: datetime, price_params: dict):
    streams = [np.random.default_rng(s) for s in np.random.SeedSequence(seed).spawn(5)]
    r_vendor, r_assign, r_quality, r_commerce, r_profile = streams

    vendors = make_vendors(r_vendor, n_vendors)
    assign = r_assign.choice(n_vendors, size=len(raw), p=zipf_weights(n_vendors, 1.1))
    q = r_quality.beta(2, 2, size=len(raw))

    vendor_q = []
    for i, v in enumerate(vendors):
        mine = q[assign == i]
        base = float(mine.mean()) if len(mine) else float(r_quality.beta(2, 2))
        vq = float(np.clip(0.7 * base + 0.3 * r_quality.beta(2, 2), 0, 1))
        vendor_q.append({"vendor_id": v["vendor_id"], "quality": round(vq, 6)})
        v.update(vendor_profile(r_profile, vq))

    products, item_q = [], []
    for p, vi, qi in zip(raw, assign, q, strict=True):
        v = vendors[vi]
        attrs = {}
        if p.get("color"):
            attrs["color"] = [p["color"]]
        if p.get("material"):
            attrs["material"] = [p["material"].lower()]
        if p.get("item_keywords"):
            attrs["item_keywords"] = p["item_keywords"]
        rating_count = (
            int(r_commerce.lognormal(math.log(30), 1.2)) if r_commerce.random() > 0.1 else 0
        )
        rating = round(float(np.clip(1 + 4 * qi + r_commerce.normal(0, 0.35), 1, 5)), 1)
        stock = (
            0
            if r_commerce.random() < 0.05
            else min(int(r_commerce.lognormal(math.log(40), 1.0)) + 1, 5000)
        )
        moq = 1 if r_commerce.random() < 0.7 else int(r_commerce.choice([10, 50, 100]))
        created = ref - timedelta(seconds=int(r_commerce.integers(0, 730 * 86400)))
        out = {
            "product_id": p["product_id"],
            "vendor_id": v["vendor_id"],
            "title": p["title"],
            "description": "\n".join(p.get("bullet_points") or []),
            "brand": p.get("brand") or "",
            "category_path": p["category_path"],
            "category_ids": list(dict.fromkeys(s for s in map(slug, p["category_path"]) if s)),
            "product_type": p["product_type"],
            "attributes": attrs,
            "price_minor": price_minor(r_commerce, price_params, p["product_type"]),
            "currency": "USD",
            "moq": moq,
            "stock": stock,
            "ship_regions": v["ship_regions"],
            "status": "active" if r_commerce.random() < 0.98 else "inactive",
            "rating": rating if rating_count > 0 else None,
            "rating_count": rating_count,
            "image_files": [f"{p['main_image_id']}.jpg"],
            "created_at": created.strftime("%Y-%m-%dT%H:%M:%SZ"),
        }
        out["listing_completeness"] = round(completeness(out), 6)
        products.append(out)
        item_q.append({"product_id": p["product_id"], "q": round(float(qi), 6)})
    return vendors, products, item_q, vendor_q


def synthesize_campaigns(
    products: list[dict],
    vendors: list[dict],
    seed: int,
    n_campaigns: int,
    ref: datetime,
) -> list[dict]:
    rng = np.random.default_rng(seed + 999)
    by_vendor: dict[str, list[dict]] = {}
    for p in products:
        if p.get("status") == "active":
            by_vendor.setdefault(p["vendor_id"], []).append(p)

    active_vendors = [v for v in vendors if v["vendor_id"] in by_vendor]
    if not active_vendors:
        return []

    n_target = min(n_campaigns, max(1, len(products) * 3))
    campaigns = []
    for i in range(n_target):
        v = active_vendors[i % len(active_vendors)]
        v_prods = by_vendor[v["vendor_id"]]
        k = min(len(v_prods), int(rng.integers(1, 6)))
        chosen_indices = rng.choice(len(v_prods), size=k, replace=False)
        chosen_prods = [v_prods[idx] for idx in chosen_indices]

        days_ago = int(rng.integers(10, 60))
        start_dt = ref - timedelta(days=days_ago)
        ts_ms = int(start_dt.timestamp() * 1000)

        camp_id = make_ulid(ts_ms, rng.bytes(10))
        creatives = [
            {
                "id": make_ulid(ts_ms, rng.bytes(10)),
                "campaign_id": camp_id,
                "product_id": p["product_id"],
            }
            for p in chosen_prods
        ]

        cats = set()
        words = set()
        for p in chosen_prods:
            for c in p.get("category_ids", []):
                cats.add(c)
            for w in p.get("title", "").split():
                clean_w = "".join(ch for ch in w.lower() if ch.isalnum())
                if len(clean_w) > 3:
                    words.add(clean_w)

        sorted_cats = sorted(cats)
        sorted_words = sorted(words)
        targeting = {}
        if rng.random() < 0.70:
            if sorted_cats:
                n_cats = min(len(sorted_cats), int(rng.integers(1, 3)))
                targeting["categories"] = sorted(
                    rng.choice(sorted_cats, size=n_cats, replace=False).tolist()
                )
            if sorted_words:
                n_words = min(len(sorted_words), int(rng.integers(1, 4)))
                targeting["keywords"] = sorted(
                    rng.choice(sorted_words, size=n_words, replace=False).tolist()
                )

        status = "active" if rng.random() < 0.95 else "paused"
        daily_budget = int(rng.choice([5000, 10000, 15000, 25000, 50000, 100000]))
        bid = int(rng.integers(25, 350))

        end_at = None
        if rng.random() < 0.10:
            end_dt = ref + timedelta(days=int(rng.integers(30, 180)))
            end_at = end_dt.strftime("%Y-%m-%dT%H:%M:%SZ")

        campaigns.append(
            {
                "id": camp_id,
                "vendor_id": v["vendor_id"],
                "status": status,
                "daily_budget_minor": daily_budget,
                "currency": "USD",
                "bid_minor": bid,
                "targeting": targeting,
                "start_at": start_dt.strftime("%Y-%m-%dT%H:%M:%SZ"),
                "end_at": end_at,
                "creatives": creatives,
            }
        )

    return campaigns


def write_jsonl(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=False, sort_keys=True) + "\n")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("--vendors", type=int, default=200)
    ap.add_argument("--campaigns", type=int, default=300)
    ap.add_argument(
        "--ref-date",
        default="2026-10-01",
        help="created_at is drawn from the 2 years before this date",
    )
    ap.add_argument("--data", type=Path, default=DATA)
    args = ap.parse_args()

    raw = [json.loads(line) for line in open(args.data / "products.raw.jsonl", encoding="utf-8")]
    ref = datetime.fromisoformat(args.ref_date).replace(tzinfo=UTC)
    params = yaml.safe_load(PRICE_PARAMS.read_text())
    vendors, products, item_q, vendor_q = synthesize(raw, args.seed, args.vendors, ref, params)
    campaigns = synthesize_campaigns(products, vendors, args.seed, args.campaigns, ref)

    out = {
        "vendors.jsonl": vendors,
        "products.canonical.jsonl": products,
        "campaigns.jsonl": campaigns,
        "hidden/item_quality.jsonl": item_q,
        "hidden/vendor_quality.jsonl": vendor_q,
    }
    for name, rows in out.items():
        write_jsonl(args.data / name, rows)
    manifest.update(
        args.data,
        "synthesize",
        {
            "seed": args.seed,
            "vendors": args.vendors,
            "campaigns": args.campaigns,
            "ref_date": args.ref_date,
            "price_params_sha256": manifest.sha256_file(PRICE_PARAMS),
        },
        [args.data / n for n in out],
    )
    active = sum(1 for p in products if p["status"] == "active")
    print(
        f"{len(products)} products ({active} active), {len(vendors)} vendors, "
        f"{len(campaigns)} campaigns"
    )


if __name__ == "__main__":
    main()
