"""Select the deterministic ABO product subset (docs/DATA.md §3).

    uv run python -m dataprep.select --seed 42

Writes data/products.raw.jsonl, data/eval/image_eval_seed.jsonl and data/images/*.jpg (main
images, plus one alternate image per eval product). Images come from
data/raw/abo/abo-images-small.tar when present, otherwise they are downloaded individually.
"""

from __future__ import annotations

import argparse
import csv
import gzip
import json
import random
import re
import tarfile
import unicodedata
from collections import defaultdict
from pathlib import Path

from dataprep import download, manifest

ROOT = Path(__file__).resolve().parents[2]
DATA = ROOT / "data"

# ABO's English marketplaces label the category tree root "Categories"; other marketplaces
# use localized roots, which would give non-English category paths.
CATEGORY_ROOT = "Categories"


def localized(values: list[dict] | None, key: str = "value") -> str | None:
    """Returns the en_US value, falling back to any en_* value."""
    if not values:
        return None
    for want in ("en_US", "en_"):
        for v in values:
            if v.get("language_tag", "").startswith(want) and v.get(key) not in (None, ""):
                return str(v[key]).strip()
    return None


def localized_all(values: list[dict] | None, limit: int) -> list[str]:
    out: list[str] = []
    for v in values or []:
        if v.get("language_tag", "").startswith("en_"):
            s = str(v.get("value", "")).strip()
            if s and s not in out:
                out.append(s)
    return out[:limit]


def category_path(record: dict) -> list[str] | None:
    for node in record.get("node") or []:
        parts = [p.strip() for p in (node.get("node_name") or "").split("/") if p.strip()]
        if len(parts) >= 2 and parts[0] == CATEGORY_ROOT:
            return parts[1:]
    return None


def color(record: dict) -> str | None:
    for v in record.get("color") or []:
        if v.get("language_tag", "").startswith("en_"):
            std = v.get("standardized_values") or []
            return (std[0] if std else v.get("value", "")).strip().lower() or None
    return None


def normalize_key(title: str, brand: str | None) -> str:
    t = unicodedata.normalize("NFKC", title).lower()
    t = re.sub(r"[^\w]+", " ", t).strip()
    return f"{t}|{(brand or '').strip().lower()}"


def parse(record: dict, image_paths: dict[str, str]) -> dict | None:
    """Maps one ABO listing to a raw product, or None when it fails the §3.1 filters."""
    title = localized(record.get("item_name"))
    main = record.get("main_image_id")
    path = category_path(record)
    if not title or not main or main not in image_paths or not path:
        return None
    types = record.get("product_type") or []
    ptype = types[0].get("value") if types else None
    if not ptype:
        return None
    others = [i for i in record.get("other_image_id") or [] if i in image_paths]
    return {
        "item_id": record["item_id"],
        "product_id": f"abo:{record['item_id']}",
        "domain": record.get("domain_name"),
        "title": title,
        "brand": localized(record.get("brand")),
        "product_type": ptype,
        "category_path": path,
        "color": color(record),
        "material": localized(record.get("material")),
        "item_keywords": localized_all(record.get("item_keywords"), 12),
        "bullet_points": localized_all(record.get("bullet_point"), 8),
        "main_image_id": main,
        "other_image_ids": others,
    }


def load_image_paths(raw: Path) -> dict[str, str]:
    with gzip.open(raw / download.IMAGES_CSV, "rt", newline="") as f:
        return {row["image_id"]: row["path"] for row in csv.DictReader(f)}


def load_candidates(raw: Path, image_paths: dict[str, str]) -> list[dict]:
    """Parses every listing and deduplicates on item_id, normalized title plus brand, and main
    image (ABO size/colour variants often share one photo).

    Among duplicates, items with alternates and the amazon.com listing win, so the outcome
    does not depend on file order.
    """
    parsed = []
    for f in sorted((raw / "listings" / "metadata").glob("listings_*.json.gz")):
        with gzip.open(f, "rt", encoding="utf-8") as lines:
            for line in lines:
                p = parse(json.loads(line), image_paths)
                if p:
                    parsed.append(p)

    def rank(p: dict) -> tuple:
        return (
            not p["other_image_ids"],
            p["domain"] != "amazon.com",
            p["item_id"],
            p["domain"] or "",
        )

    parsed.sort(key=rank)
    seen: set[str] = set()
    out = []
    for p in parsed:
        keys = {
            "item:" + p["item_id"],
            "title:" + normalize_key(p["title"], p["brand"]),
            "image:" + p["main_image_id"],
        }
        if keys & seen:
            continue
        seen |= keys
        out.append(p)
    out.sort(key=lambda p: p["item_id"])
    return out


def stratify(
    cands: list[dict], rng: random.Random, target: int, cap: int, min_type: int
) -> list[dict]:
    """Caps each product_type at `cap`, drops types under `min_type`, then takes an equal
    per-type quota (water-filling) so that exactly `target` items remain. Items with an
    alternate image are preferred within each type."""
    by_type: dict[str, list[dict]] = defaultdict(list)
    for p in cands:
        by_type[p["product_type"]].append(p)
    pools = {}
    for t in sorted(by_type):
        items = by_type[t]
        if len(items) < min_type:
            continue
        rng.shuffle(items)
        items.sort(key=lambda p: not p["other_image_ids"])
        pools[t] = items[:cap]
    available = sum(len(v) for v in pools.values())
    if available < target:
        raise SystemExit(f"only {available} items available after stratification, want {target}")

    quota = 0
    while sum(min(len(v), quota + 1) for v in pools.values()) <= target:
        quota += 1
    picked = {t: v[:quota] for t, v in pools.items()}
    short = target - sum(len(v) for v in picked.values())
    for t in sorted(
        (t for t, v in pools.items() if len(v) > quota), key=lambda t: (-len(pools[t]), t)
    ):
        if short == 0:
            break
        picked[t].append(pools[t][quota])
        short -= 1
    out = [p for v in picked.values() for p in v]
    out.sort(key=lambda p: p["product_id"])
    return out


def materialize_images(raw: Path, image_paths: dict[str, str], ids: list[str], out: Path) -> None:
    """Makes out/ hold exactly the images in ids (stale files from earlier runs are removed)."""
    out.mkdir(parents=True, exist_ok=True)
    keep = {f"{i}.jpg" for i in ids}
    for f in out.glob("*.jpg"):
        if f.name not in keep:
            f.unlink()
    missing = [i for i in ids if not (out / f"{i}.jpg").exists()]
    tar_path = raw / "abo-images-small.tar"
    if missing and tar_path.exists():
        wanted = {f"images/small/{image_paths[i]}": i for i in missing}
        with tarfile.open(tar_path) as t:
            for m in t:
                if m.name in wanted:
                    data = t.extractfile(m).read()
                    (out / f"{wanted[m.name]}.jpg").write_bytes(data)
        missing = [i for i in missing if not (out / f"{i}.jpg").exists()]
    if missing:
        print(f"downloading {len(missing)} images from the ABO bucket")
        download.fetch_small_images([(image_paths[i], out / f"{i}.jpg") for i in missing])


def write_jsonl(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=False, sort_keys=True) + "\n")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("--target", type=int, default=10_000)
    ap.add_argument("--cap", type=int, default=400)
    ap.add_argument("--min-type", type=int, default=20)
    ap.add_argument(
        "--eval-images",
        type=int,
        default=1_200,
        help="products whose first alternate image is kept for image eval",
    )
    ap.add_argument("--raw", type=Path, default=DATA / "raw" / "abo")
    ap.add_argument("--out", type=Path, default=DATA)
    ap.add_argument("--no-images", action="store_true", help="skip image download (tests)")
    args = ap.parse_args()

    if not args.no_images:
        download.ensure_metadata(args.raw)
    rng = random.Random(args.seed)
    image_paths = load_image_paths(args.raw)
    cands = load_candidates(args.raw, image_paths)
    products = stratify(cands, rng, args.target, args.cap, args.min_type)

    # An eval query image must identify one product: never another product's main image and
    # never shared between eval products.
    mains = {p["main_image_id"] for p in products}
    alt_owners: dict[str, int] = defaultdict(int)
    for p in products:
        for i in p["other_image_ids"]:
            alt_owners[i] += 1
    for p in products:
        p["eval_alternates"] = [
            i for i in p["other_image_ids"] if i not in mains and alt_owners[i] == 1
        ]
    with_alt = [p for p in products if p["eval_alternates"]]
    eval_rows = [
        {"product_id": p["product_id"], "image_id": p["eval_alternates"][0]}
        for p in sorted(
            rng.sample(with_alt, min(args.eval_images, len(with_alt))),
            key=lambda p: p["product_id"],
        )
    ]

    for p in products:
        del p["eval_alternates"]
    write_jsonl(args.out / "products.raw.jsonl", products)
    write_jsonl(args.out / "eval" / "image_eval_seed.jsonl", eval_rows)
    if not args.no_images:
        materialize_images(
            args.raw, image_paths, [p["main_image_id"] for p in products], args.out / "images"
        )
        materialize_images(
            args.raw, image_paths, [r["image_id"] for r in eval_rows], args.out / "images" / "alt"
        )

    params = {k: v for k, v in vars(args).items() if k not in ("raw", "out", "no_images")}
    files = [args.out / "products.raw.jsonl", args.out / "eval" / "image_eval_seed.jsonl"]
    if not args.no_images:
        files.append(args.out / "images")
    manifest.update(args.out, "select", params, files)

    types = {p["product_type"] for p in products}
    print(
        f"candidates {len(cands)} -> selected {len(products)} across {len(types)} product types; "
        f"{len(with_alt)} with alternates; {len(eval_rows)} eval images"
    )


if __name__ == "__main__":
    main()
