"""Determinism and invariants of select + synthesize on a small synthetic ABO fixture."""

from __future__ import annotations

import gzip
import hashlib
import json
import random
import subprocess
import sys
from datetime import datetime, timedelta
from pathlib import Path

import pytest

from dataprep import manifest
from dataprep.select import normalize_key, parse
from dataprep.synthesize import slug

TOOLS = Path(__file__).resolve().parents[1]
TYPES = {"SHOES": 60, "CHAIR": 45, "LAMP": 30, "RARE": 5}


def listing(i: int, ptype: str, rng: random.Random) -> dict:
    rec = {
        "item_id": f"B{i:09d}",
        "domain_name": "amazon.com",
        "item_name": [{"language_tag": "en_US", "value": f"{ptype.title()} model {i}"}],
        "brand": [{"language_tag": "en_US", "value": f"Brand{i % 7}"}],
        "product_type": [{"value": ptype}],
        "node": [{"node_id": 1, "node_name": f"/Categories/Home/{ptype.title()}s"}],
        "main_image_id": f"img{i}",
        "color": [{"language_tag": "en_US", "value": "Navy", "standardized_values": ["Blue"]}],
        "item_keywords": [
            {"language_tag": "en_US", "value": "k1"},
            {"language_tag": "en_GB", "value": "k2"},
        ],
        "bullet_point": [{"language_tag": "en_US", "value": "Sturdy"}],
    }
    if rng.random() < 0.7:
        rec["other_image_id"] = [f"alt{i}"]
    return rec


@pytest.fixture
def raw_dir(tmp_path: Path) -> Path:
    rng = random.Random(7)
    raw = tmp_path / "raw"
    meta = raw / "listings" / "metadata"
    meta.mkdir(parents=True)
    records, i = [], 0
    for ptype, n in TYPES.items():
        for _ in range(n):
            records.append(listing(i, ptype, rng))
            i += 1
    records.append({**records[0], "domain_name": "amazon.in"})  # same item_id, other market
    records.append(
        {
            **listing(i, "SHOES", rng),
            "item_name": records[1]["item_name"],
            "brand": records[1]["brand"],
        }
    )
    records.append(
        {
            **listing(i + 1, "SHOES", rng),
            "node": [{"node_id": 2, "node_name": "/Kategorien/Schuhe"}],
        }
    )
    with gzip.open(meta / "listings_0.json.gz", "wt") as f:
        for r in records:
            f.write(json.dumps(r) + "\n")
    csv = raw / "images" / "metadata" / "images.csv.gz"
    csv.parent.mkdir(parents=True)
    with gzip.open(csv, "wt") as f:
        f.write("image_id,height,width,path\n")
        for j in range(i + 2):
            f.write(f"img{j},256,256,aa/img{j}.jpg\nalt{j},256,256,aa/alt{j}.jpg\n")
    return raw


def run(module: str, *args: str) -> None:
    subprocess.run(
        [sys.executable, "-m", module, *args], cwd=TOOLS, check=True, capture_output=True
    )


def build(raw: Path, out: Path) -> None:
    run(
        "dataprep.select",
        "--seed",
        "42",
        "--target",
        "100",
        "--cap",
        "40",
        "--min-type",
        "20",
        "--eval-images",
        "10",
        "--raw",
        str(raw),
        "--out",
        str(out),
        "--no-images",
    )
    run("dataprep.synthesize", "--seed", "42", "--vendors", "12", "--data", str(out))


def test_same_seed_same_bytes(raw_dir: Path, tmp_path: Path) -> None:
    a, b = tmp_path / "a", tmp_path / "b"
    build(raw_dir, a)
    build(raw_dir, b)
    for name in [
        "products.raw.jsonl",
        "products.canonical.jsonl",
        "vendors.jsonl",
        "campaigns.jsonl",
        "hidden/item_quality.jsonl",
        "eval/image_eval_seed.jsonl",
    ]:
        assert manifest.sha256_file(a / name) == manifest.sha256_file(b / name), name
    m = json.loads((a / "MANIFEST.json").read_text())
    assert m["files"]["products.canonical.jsonl"]["rows"] == 100
    assert m["steps"]["select"]["seed"] == 42


def test_selection_invariants(raw_dir: Path, tmp_path: Path) -> None:
    out = tmp_path / "out"
    build(raw_dir, out)
    raw = [json.loads(line) for line in open(out / "products.raw.jsonl")]
    types = {p["product_type"] for p in raw}
    assert len(raw) == 100
    assert "RARE" not in types, "types under --min-type are dropped"
    assert max(sum(p["product_type"] == t for p in raw) for t in types) <= 40
    assert len({p["item_id"] for p in raw}) == 100
    assert len({normalize_key(p["title"], p["brand"]) for p in raw}) == 100
    assert all(p["category_path"][0] == "Home" for p in raw), "non-English trees are skipped"
    assert all(p["item_keywords"] == ["k1", "k2"] for p in raw)

    canon = [json.loads(line) for line in open(out / "products.canonical.jsonl")]
    vendors = {v["vendor_id"] for v in map(json.loads, open(out / "vendors.jsonl"))}
    main_image = {p["product_id"]: p["main_image_id"] for p in raw}
    for p in canon:
        assert p["vendor_id"] in vendors
        assert p["price_minor"] >= 99 and p["currency"] == "USD"
        assert p["moq"] in (1, 10, 50, 100)
        assert p["status"] in ("active", "inactive")
        assert p["image_files"] == [main_image[p["product_id"]] + ".jpg"]
        assert (p["rating"] is None) == (p["rating_count"] == 0)
        assert p["category_ids"] == ["home", slug(p["category_path"][1])]
        assert "q" not in p, "hidden quality must never be exported with products"


def test_parse_and_slug() -> None:
    rec = listing(1, "SHOES", random.Random(0))
    p = parse(rec, {"img1": "aa/img1.jpg", "alt1": "aa/alt1.jpg"})
    assert p["color"] == "blue" and p["product_id"] == "abo:B000000001"
    assert parse({**rec, "main_image_id": "nope"}, {"img1": "x"}) is None
    assert slug("Bags & Wallets") == "bags-wallets"
    assert slug("  Café Crème ") == "cafe-creme"
    assert slug("Électronique/TV 4K") == "electronique-tv-4k"


def run_simulate(data: Path) -> None:
    cmd = [
        sys.executable,
        "-m",
        "dataprep.simulate",
        "--seed",
        "42",
        "--data",
        str(data),
        "--users",
        "50",
        "--sessions",
        "100",
        "--parts",
        "2",
    ]
    subprocess.run(cmd, check=True, cwd=TOOLS)


def test_simulate_invariants(raw_dir: Path, tmp_path: Path) -> None:
    out = tmp_path / "out"
    build(raw_dir, out)
    run_simulate(out)

    users = [json.loads(line) for line in open(out / "users.jsonl")]
    assert len(users) == 50
    assert all("user_id" in u and "price_band" in u and "category_affinities" in u for u in users)

    all_events = []
    for part in sorted((out / "events").glob("part-*.jsonl.gz")):
        all_events += [json.loads(line) for line in gzip.open(part, "rt")]

    impressions = [e for e in all_events if e["type"] == "impression"]
    assert len(impressions) == 100 * 50
    assert all(1 <= e["position"] <= 50 for e in impressions)
    assert len({e["event_id"] for e in all_events}) == len(all_events)

    # Randomized sessions mark all their impressions and nothing else.
    randomized = {e["request_id"] for e in impressions if e.get("randomized")}
    assert 0 < len(randomized) < 30
    assert all(e.get("randomized") for e in impressions if e["request_id"] in randomized)
    assert not any(e.get("randomized") for e in all_events if e["type"] != "impression")

    # The request log has one record per session, with the query that produced its impressions.
    requests = [json.loads(line) for line in gzip.open(out / "events" / "requests.jsonl.gz", "rt")]
    assert len(requests) == 100
    assert {r["request_id"] for r in requests} == {e["request_id"] for e in impressions}
    assert {r["type"] for r in requests} <= {"text", "browse"}
    assert all(r["query"] for r in requests)

    clicks = [e for e in all_events if e["type"] == "click"]
    carts = [e for e in all_events if e["type"] == "add_to_cart"]
    orders = [e for e in all_events if e["type"] == "order"]
    assert len(clicks) > 0
    assert len(orders) <= len(carts) <= len(clicks)
    for o in orders:
        assert "order_id" in o and o["quantity"] >= 1 and o["value_minor"] > 0
    # Events end at ref_date (actions trail their impression by at most ~3 min); seed shifts
    # them so ref_date lands on the present.
    ref = datetime.fromisoformat(
        json.loads((out / "simulation_params.json").read_text())["ref_date"]
    )
    latest = max(datetime.fromisoformat(e["ts"]) for e in all_events)
    assert ref - timedelta(days=30) <= latest <= ref + timedelta(minutes=3)


def test_simulate_is_deterministic(raw_dir: Path, tmp_path: Path) -> None:
    hashes = []
    for name in ("a", "b"):
        out = tmp_path / name
        build(raw_dir, out)
        run_simulate(out)
        files = sorted((out / "events").glob("*.gz")) + [
            out / "users.jsonl",
            out / "simulation_params.json",
        ]
        hashes.append([hashlib.sha256(f.read_bytes()).hexdigest() for f in files])
    assert hashes[0] == hashes[1]
