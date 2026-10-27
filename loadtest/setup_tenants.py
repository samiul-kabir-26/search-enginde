"""Creates the load-test tenants for loadtest/byo_rank.js and loadtest/isolation.js.

    tools/.venv/bin/python loadtest/setup_tenants.py      (or: make loadtest-setup)

Two tenants on the growth plan, each with 500 demo products (images from `make seed`), the
platform defaults, and a BYO kserve_v2 rank model:
- lt-byo:  Triton's rank_gbdt_v1 at http://localhost:8000, registered as the tenant's own model
- lt-slow: loadtest/slow_kserve.py at http://127.0.0.1:9555 (started separately; its delay is
  raised for the isolation test)

control-api and search-api must run with EGRESS_TRUSTED_HOSTS=localhost,127.0.0.1 so tenant
endpoints on this machine pass the egress policy (the same client, bulkhead and breakers as a
public endpoint). API keys are written to loadtest/.tenants.json (git-ignored). Re-running reuses
tenants that already exist and are live.
"""

from __future__ import annotations

import json
import os
import re
import time
from pathlib import Path

import requests

ROOT = Path(__file__).resolve().parents[1]
OUT = Path(__file__).resolve().parent / ".tenants.json"
CONTROL = os.environ.get("CONTROL_API_URL", "http://localhost:8081").rstrip("/")
MAILPIT = os.environ.get("MAILPIT_URL", "http://localhost:8025").rstrip("/")
S3_PUBLIC = os.environ.get("S3_PUBLIC_URL", "http://localhost:9100").rstrip("/")
BUCKET = os.environ.get("S3_BUCKET_IMAGES", "catalog-images")
PASSWORD = "load-test-password-1"
PRODUCTS = 500

TENANTS = {
    "lt-byo": {"model": "rank_gbdt_v1", "endpoint": "http://localhost:8000"},
    "lt-slow": {"model": "slow_ranker", "endpoint": "http://127.0.0.1:9555"},
}


def must(r: requests.Response, what: str) -> dict:
    if not r.ok:
        raise SystemExit(f"{what}: HTTP {r.status_code} {r.text[:500]}")
    return r.json() if r.content else {}


def catalog() -> str:
    """500 active demo products with images, in the demo storefront's JSON shape."""
    lines = []
    with open(ROOT / "data" / "products.canonical.jsonl", encoding="utf-8") as f:
        for line in f:
            p = json.loads(line)
            if p["status"] != "active" or not p.get("image_files"):
                continue
            lines.append(
                json.dumps(
                    {
                        "id": p["product_id"],
                        "name": p["title"],
                        "description": p.get("description"),
                        "category": {"path": p["category_path"]},
                        "brand": p.get("brand"),
                        "product_type": p.get("product_type"),
                        "price": {"amount_minor": p["price_minor"], "currency": p["currency"]},
                        "images": [
                            {"url": f"{S3_PUBLIC}/{BUCKET}/demo/{x}"} for x in p["image_files"]
                        ],
                        "vendor": {"id": p["vendor_id"]},
                        "attributes": p.get("attributes"),
                        "inventory": {"stock": p.get("stock")},
                        "shipping": {"regions": p.get("ship_regions")},
                        "status": p["status"],
                        "reviews": {"rating": p.get("rating"), "count": p.get("rating_count")},
                        "created_at": p.get("created_at"),
                    }
                )
            )
            if len(lines) == PRODUCTS:
                break
    return "\n".join(lines) + "\n"


def verify_email(email: str) -> None:
    for _ in range(30):
        res = requests.get(
            f"{MAILPIT}/api/v1/search", params={"query": f'to:"{email}"'}, timeout=10
        ).json()
        for m in res.get("messages") or []:
            msg = requests.get(f"{MAILPIT}/api/v1/message/{m['ID']}", timeout=10).json()
            link = re.search(
                r"verify-email\?token=([A-Za-z0-9_\-.%]+)",
                msg.get("Text", "") + msg.get("HTML", ""),
            )
            if link:
                must(
                    requests.post(
                        f"{CONTROL}/v1/auth/verify-email", json={"token": link.group(1)}, timeout=10
                    ),
                    "verify",
                )
                return
        time.sleep(1)
    raise SystemExit(f"no verification email for {email}")


def login(slug: str, email: str) -> tuple[str, str]:
    s = must(
        requests.post(
            f"{CONTROL}/v1/auth/login",
            json={"tenant": slug, "email": email, "password": PASSWORD},
            timeout=10,
        ),
        "login",
    )
    return s["tenant_id"], s["access_token"]


def setup(slug: str, spec: dict, rank_features: list[str], jsonl: str) -> dict:
    email = f"owner@{slug}.test"
    r = requests.post(
        f"{CONTROL}/v1/signup",
        json={
            "company_name": f"Load test {slug}",
            "slug": slug,
            "region": "us",
            "plan": "growth",
            "email": email,
            "password": PASSWORD,
        },
        timeout=30,
    )
    if r.status_code == 201:
        verify_email(email)
    elif r.status_code != 409:
        must(r, "signup")
    tid, token = login(slug, email)  # a fresh token carries the verified-email claim
    H = {"Authorization": f"Bearer {token}"}
    base = f"{CONTROL}/v1/tenants/{tid}"
    state = must(requests.get(f"{base}/onboarding", headers=H, timeout=10), "state")
    if state["current_step"] == "done":
        print(f"{slug}: already live")
        return {"tenant_id": tid}

    def step(name: str, body: dict | None = None, query: str = "") -> dict:
        return must(
            requests.put(
                f"{base}/onboarding/steps/{name}{query}", headers=H, json=body or {}, timeout=120
            ),
            f"{slug} step {name}",
        )

    step("engine_tour")
    draft = must(requests.get(f"{base}/config/draft", headers=H, timeout=10), "draft")["document"]
    samples = [json.loads(x) for x in jsonl.splitlines()[:20]]
    step(
        "catalog",
        {
            "ingestion": "upload",
            "field_mapping": draft["catalog"]["field_mapping"],
            "samples": samples,
        },
    )
    step("retrieval", query="?use_defaults=true")

    up = must(
        requests.post(f"{base}/catalog/uploads", headers=H, json={"format": "jsonl"}, timeout=10),
        "presign",
    )
    must(requests.put(up["url"], headers=up["headers"], data=jsonl.encode(), timeout=60), "upload")

    model = must(
        requests.post(
            f"{base}/models",
            headers=H | {"Idempotency-Key": f"{slug}-rank"},
            timeout=30,
            json={
                "kind": "rank",
                "type": "kserve_v2",
                "name": spec["model"],
                "version": "",
                "endpoint": spec["endpoint"],
                "input_schema": {"features": rank_features},
            },
        ),
        "register model",
    )
    v = must(
        requests.post(f"{base}/models/{model['id']}/validate", headers=H, timeout=60),
        "validate model",
    )
    if not v["ok"]:
        raise SystemExit(f"{slug}: model validation failed: {v['checks']}")
    step("ranking", {"mode": "byo", "rank_model_id": model["id"]})

    job = step("index_build", {"upload_id": up["upload_id"]})["data"]["job"]
    while job["status"] not in ("succeeded", "failed"):
        time.sleep(2)
        job = must(requests.get(f"{base}/index/jobs/{job['id']}", headers=H, timeout=10), "job")
    if job["status"] != "succeeded":
        raise SystemExit(f"{slug}: index build failed: {job.get('error')}")
    print(f"{slug}: indexed {job['done']} products")
    step("playground")
    live = step("go_live", {"mode": "activate"})["data"]
    return {"tenant_id": tid, "api_key": live["api_key"]}


def main() -> None:
    known = json.loads(OUT.read_text()) if OUT.exists() else {}
    platform = must(requests.get(f"{CONTROL}/v1/platform/models", timeout=10), "platform models")[
        "items"
    ]
    features = next(m["features"] for m in platform if m["id"] == "rank-gbdt-v1")
    jsonl = catalog()
    for slug, spec in TENANTS.items():
        got = setup(slug, spec, features, jsonl)
        known[slug] = known.get(slug, {}) | {k: v for k, v in got.items() if v}
        if not known[slug].get("api_key"):
            raise SystemExit(
                f"{slug} is live but its API key is not in {OUT}; create one in the console"
            )
    OUT.write_text(json.dumps(known, indent=2) + "\n")
    print(f"wrote {OUT}")


if __name__ == "__main__":
    main()
