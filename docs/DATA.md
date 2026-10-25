# DATA.md — Datasets, Vector Database, Synthetic Data, Evaluation

## 1. What is needed

| Asset | Size | Source |
|---|---|---|
| Product catalog with images | ~10,000 products, ~10,000 main images plus alternates | Amazon Berkeley Objects (ABO) subset |
| Vendors | 200 | Synthetic |
| Buyers | 5,000 | Synthetic |
| Interaction events | ~2M impressions, plus clicks, carts, orders, RFQs | Synthetic, generated from a hidden utility model |
| Sponsored campaigns | 300 | Synthetic |
| Text eval queries | ~1,000 with graded labels | Derived from catalog attributes |
| Image eval queries | ~1,000 | ABO alternate images of indexed products |
| Lexical index | 1 index per tenant | OpenSearch |
| Vector database | 1 collection per tenant, named vectors `text` (384) and `image` (512) | Qdrant |

## 2. Product source: ABO

ABO (Amazon Berkeley Objects) provides product listings with multilingual metadata and catalog images.

**License.** CC BY 4.0 (relicensed from CC BY-NC 4.0; confirmed on the official page and README on 2026-10-25). Attribution is required. It is still demo data only: it is never used as production tenant data. Keep the attribution file in `tools/dataprep/ATTRIBUTION.md`.

**Downloads.** From the official ABO bucket (`https://amazon-berkeley-objects.s3.amazonaws.com/index.html`), done by `make data` (`tools/dataprep/download.py`):
- `archives/abo-listings.tar` (83 MiB): `listings/metadata/listings_{0-9a-f}.json.gz`, gzipped JSON lines;
- `images/metadata/images.csv.gz` (6 MiB): `image_id,height,width,path`;
- only the selected small images, fetched one by one from `images/small/{path}`. The 3 GiB `archives/abo-images-small.tar` is optional: `select.py` reads from it when it is present in `data/raw/abo/`, otherwise ~11K images (~85 MB) are downloaded in parallel. The bucket is slow per connection, so large files are fetched as parallel byte ranges.

**Verified field names** (inspected 2026-10-25; 147,702 listings). Every listing has `item_id`, `item_name`, `product_type`, `country`, `marketplace` and `domain_name`; most have `brand`, `main_image_id`, `node`, `other_image_id`, `bullet_point`, `item_keywords` and `color`; about a third have `material`.
- Localized fields are lists of `{language_tag, value}`; `color` items also carry `standardized_values`.
- `product_type` is a list `[{value}]`, not a string.
- `node` is a list of `{node_id, node_name}`, where `node_name` is the category path as a string such as `/Categories/Furniture/Living Room Furniture`. Non-English marketplaces use localized roots (`/Categorías/...`).
- A listing is unique per (`item_id`, `domain_name`); the same item can appear on several marketplaces.
- ABO does not provide price, stock or seller; those are synthesized (§4).

## 3. Subset selection (`tools/dataprep/select.py`)

Run deterministically with `--seed 42`.

1. Keep items with an `en_US` (fallback `en_*`) `item_name`, a `main_image_id` present in `images.csv.gz`, a `product_type`, and a `node` path under the English root `/Categories/` (the root segment is dropped from `category_path`).
2. Deduplicate on `item_id`, on normalized title plus brand, and on `main_image_id` (size and colour variants often share one photo). Among duplicates, items with alternates and `amazon.com` listings win.
3. Stratify by `product_type`, capping each type at 400 items. Drop types with fewer than 20 items.
4. Target 10,000 items: every remaining type gets the same quota (water-filling), and items with at least one `other_image_id` are preferred within a type because image eval needs them (§7).
5. Choose 1,200 eval products and one alternate image each. An eval image is never another product's main image and never shared between eval products.
6. Write `data/products.raw.jsonl` and `data/eval/image_eval_seed.jsonl`; `data/images/` holds exactly the main images and `data/images/alt/` the eval images (uploaded to the S3 bucket `catalog-images` at seed time).
7. Record parameters, row counts and sha256 for every output in `data/MANIFEST.json`.

On the 2026-10-25 data this selects 10,000 of 91,352 candidates across 162 product types.

## 4. Canonical product schema (`internal/catalog`)

| Field | Type | Filterable | Notes |
|---|---|---|---|
| `product_id` | string | yes | `abo:{item_id}` |
| `tenant_id` | string | yes (implicit) | |
| `vendor_id` | string | yes | Synthetic assignment, Zipf-distributed (a few large vendors) |
| `title` | text | no | |
| `description` | text | no | Joined `bullet_point` values |
| `brand` | keyword | yes | |
| `category_path` | keyword[] | yes | From `node`, root→leaf |
| `category_ids` | keyword[] | yes | Slugified path segments |
| `product_type` | keyword | yes | |
| `attributes` | object | selected keys | color, material, plus `item_keywords` |
| `price_minor` | int64 | yes | Synthetic: lognormal per `product_type` (params in `tools/dataprep/price_params.yaml`) |
| `currency` | keyword | yes | `USD` |
| `moq` | int32 | yes | Synthetic B2B minimum order quantity: 1 for 70%, else 10/50/100 |
| `stock` | int32 | yes | Synthetic; 5% set to 0 |
| `ship_regions` | keyword[] | yes | Synthetic from the vendor profile |
| `status` | keyword | yes | `active` (98%), `inactive` |
| `rating` | float | yes | Synthetic, correlated with the hidden quality q |
| `rating_count` | int32 | no | |
| `image_urls` | keyword[] | no | S3 URLs (public images bucket) |
| `listing_completeness` | float | no | Computed: share of populated optional fields |
| `created_at` | timestamp | yes | Synthetic, within the last 2 years |

Vendors (`data/vendors.jsonl`) have: `vendor_id`, `name`, `verified`, `tier` (`basic`/`gold`), `on_time_dispatch_rate`, `dispute_rate`, `response_hours`, `ship_regions`. These values are drawn so that vendor score correlates with hidden quality.

## 5. Synthetic users and events (`tools/dataprep/simulate.py`)

The goal is a learnable ranking signal, not noise.

**Hidden ground truth (never exported to features).**
- Per item: quality `q ~ Beta(2,2)`.
- Per user: a category affinity vector (Dirichlet over top-level categories), a price band, and a region.

**Sessions.**
- Sample a user and a query. 70% of queries are text from the eval-style query generator (§7); 30% are category browse.
- Retrieve the top 50 by a naive BM25 baseline. Positions are randomized for 10% of sessions; these are used for propensity estimation.

**Examination and actions.**
- Examination probability: `p_exam(pos) = 1 / pos^0.7`.
- Click probability: `σ(a·relevance + b·q + c·affinity + d·price_match − e)`.
- Cart and order follow on click with their own logistic models.

**Outputs.**
- Events are written as JSONL in the `/v1/events` schema and published to Kafka by `cmd/seed --events`.
- Store the generator parameters in `data/simulation_params.json` for reproducibility.

## 6. Vectorization and indexing

**Pipeline.** `seed` publishes `catalog.product.v1` events. `indexer` then:
1. maps each product to the canonical schema;
2. embeds it via the tenant's configured embedders;
3. upserts to Qdrant and OpenSearch.

**Catalog event envelope (`catalog.product.v1`).**
- Key `{tenant_id}:{product_id}`, so one product's changes share a partition and stay ordered. Headers: `tenant-id`, `event-id`, `op`, `schema-version` (`1`); when present they must match the body.
- Value: `{event_id, tenant_id, product_id, op: "upsert"|"delete", ts, source_version, product}`. `product` is the tenant's own JSON, mapped through `catalog.field_mapping` and `catalog.custom_fields`; its mapped `product_id` must equal the envelope's.
- A Kafka tombstone (null value) for a key is a delete.
- `source_version` orders changes to one product. OpenSearch is the arbiter: writes use `version_type=external_gte`, so an older version is rejected as stale (and then skipped for Qdrant too) while an equal version re-applies, making replays idempotent. `source_version: 0` (and tombstones) apply unconditionally.
- Events that fail mapping or validation are logged and counted as invalid, never retried; store or model failures retry the batch with backoff (at-least-once).

**Topic retention.** `catalog.product.v1` is compacted (`cleanup.policy=compact`), so a backfill reads roughly one event per product rather than the whole history.

**Category centroids.** Every backfill builds per-category centroids of the new version's `text` vectors (mean over a category's products, leaf and ancestors, re-normalized, at least 3 products) and stores them in S3 at `centroids/{tenant}/v{n}.bin` with a sha256 manifest before promoting the version. `featurejob centroids -tenant <slug>` (`make centroids`) refreshes them for the active version.

**Index versions.** The indexer creates and promotes `v1` on a tenant's first event. `indexer -backfill -tenant <slug>` rebuilds into `v{n+1}` from the start of the topic, checks that both stores hold exactly the live products, swaps both aliases, and drops versions older than the previous one.

The data-prep scripts do not compute embeddings, so indexing and query use the same embedding path.

**Text embedding input:** `"{title}. {brand}. {category leaf}. {attributes k:v joined}"`, truncated per tokenizer limits by the model server.

**Image embedding input.** The main image. Alternate images are not indexed in v1; they are reserved for eval.

**Qdrant collection `t_{tenant}_products_v{n}`:**
- Named vectors: `text` (size 384, Cosine) and `image` (size 512, Cosine).
- HNSW: `m=16`, `ef_construct=128`. Search `ef` comes from config (default 128). The optimizer's `indexing_threshold` is 1,000 KB, so HNSW is built even for small tenants (Qdrant's 10 MB default leaves a 10K-product collection on brute force, where filtered top-500 searches take ~100 ms instead of ~9 ms). Payloads use `memory: cached`.
- Payload indexes: `vendor_id`, `category_ids`, `brand`, `status`, `stock` (integer), `price_minor` (integer), `ship_regions`, `moq`.
- Point ID: UUIDv5 of `product_id`; `product_id` is also stored in the payload.

**OpenSearch index `t_{tenant}_products_v{n}`:**
- `title`, `description` and `attributes.*` are text fields with the English analyzer plus a `.keyword` subfield where needed.
- Filterable fields are `keyword`/numeric.
- BM25 is the default similarity.
- Mapping lives in `internal/providers/lexical/opensearch/mapping.json`.

**Throughput target.** Full 10K backfill in ≤ 10 minutes on a dev laptop (CPU Triton).

## 7. Evaluation sets (`tools/eval`)

**Image eval** (`data/eval/image_queries.jsonl`):
- For each sampled indexed product with an `other_image_id`, the alternate image is the query.
- The relevant set is the product itself (grade 3) plus products with the same brand and product_type (grade 1).
- Metrics: recall@10, recall@50, MRR.
- As implemented (`tools/eval/image.py`, `make eval`): queries whose product the default always_filters hide (inactive or out of stock) are skipped, and hidden products never count as relevant. The headline recall@k and MRR use the product itself; graded-set recall and nDCG@10 use the full graded set. Reports are written to `tools/eval/reports/`.

**Text eval** (`data/eval/text_queries.jsonl`):
- Queries are generated from templates over catalog attributes, for example `"{color} {material} {product_type}"`, `"{brand} {product_type}"` and `"{product_type} under {price}"`.
- Labels are graded by attribute match: all constraints match = 3, product_type plus partial match = 1.
- Metrics: nDCG@10, recall@100 (retrieval-only), MRR.

**Ranking eval.** A held-out time split of synthetic sessions; metric is nDCG@10 against the hidden-utility labels.

`make eval` runs all suites against a running stack and writes `tools/eval/reports/{date}_{config_version}.md`.

## 8. Files produced by `tools/dataprep`

```
data/
  products.raw.jsonl
  products.canonical.jsonl
  vendors.jsonl
  users.jsonl
  campaigns.jsonl
  events/part-*.jsonl.gz
  images/{image_id}.jpg
  eval/image_queries.jsonl
  eval/text_queries.jsonl
  simulation_params.json
  MANIFEST.json              # counts, seeds, sha256 per file
```

`data/` is git-ignored. `make data` builds it from the downloaded archives.
