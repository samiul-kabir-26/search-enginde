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

The demo tenant's vendor score (PLAN.md R9) is computed by `seed` from these fields and published with them to `catalog.vendor.v1`: `score = 0.40·on_time + 0.25·(1 − min(dispute/0.1, 1)) + 0.15·(1 − min(hours/48, 1)) + 0.20·verified`, in [0, 1]. Real tenants send their own score; the platform only stores it.

## 5. Synthetic users and events (`tools/dataprep/simulate.py`)

The goal is a learnable ranking signal, not noise.

**Hidden ground truth (never exported to features).**
- Per item: quality `q ~ Beta(2,2)` (`data/hidden/item_quality.jsonl`).
- Per user: a category affinity vector (Dirichlet over 2–5 top-level categories), a price band, and a region (`data/users.jsonl`). featurejob never reads this file; it derives user features from events.

**Sessions.**
- 40,000 sessions over 5,000 users, spread over the 30 days before `--ref-date`, 50 impressions each (2M impressions).
- Sample a user and a query. 70% of queries are text from templates over the catalog (`{color|brand|material} {product_type}` or a product type); 30% browse one of the user's affinity categories.
- Retrieve the top 50 by a naive in-memory BM25 baseline. Positions are shuffled for 10% of sessions, and those impressions carry `"randomized": true`; featurejob estimates the position propensity from them.

**Examination and actions.**
- Examination probability: `p_exam(pos) = 1 / pos^0.7`.
- Click probability: `σ(a·relevance + b·q + c·affinity + d·price_match − e)`.
- Cart and order follow on click with their own logistic models.

**Outputs.**
- Events are written as gzipped JSONL in the `/v1/events` schema (`data/events/part-*.jsonl.gz`); clicks follow their impression by 2–60 s, carts and orders shortly after.
- Event IDs are drawn from the seeded RNG and the gzip headers carry no timestamp, so the same seed gives byte-identical files (pytest checks this). The full run takes about 2.5 minutes.
- `data/events/requests.jsonl.gz` is the matching request log: one record per session with its text query (or the browsed category, type `browse`), in the `search.requests.v1` format search-api writes.
- The generator parameters, including the hidden ones, are stored in `data/simulation_params.json` for reproducibility.
- `make seed-events` (`seed -events`) publishes the events to `search.events.v1` and the request log to `search.requests.v1`, shifting every timestamp so `--ref-date` lands on the current time; the 7- and 30-day feature windows are then populated whenever the stack is seeded. ClickHouse ingests them through the Kafka engine, and `make features` (or the scheduled featurejob) turns them into online features.

## 6. Vectorization and indexing

**Pipeline.** `seed` publishes `catalog.product.v1` events. `indexer` then:
1. maps each product to the canonical schema;
2. embeds it via the tenant's configured embedders;
3. upserts to the vector store (the platform Qdrant collection, or the tenant's BYO store) and OpenSearch.

**Catalog event envelope (`catalog.product.v1`).**
- Key `{tenant_id}:{product_id}`, so one product's changes share a partition and stay ordered. Headers: `tenant-id`, `event-id`, `op`, `schema-version` (`1`); when present they must match the body.
- Value: `{event_id, tenant_id, product_id, op: "upsert"|"delete", ts, source_version, product}`. `product` is the tenant's own JSON, mapped through `catalog.field_mapping` and `catalog.custom_fields`; its mapped `product_id` must equal the envelope's.
- A Kafka tombstone (null value) for a key is a delete.
- `source_version` orders changes to one product. OpenSearch is the arbiter: writes use `version_type=external_gte`, so an older version is rejected as stale (and then skipped for Qdrant too) while an equal version re-applies, making replays idempotent. `source_version: 0` (and tombstones) apply unconditionally.
- Events that fail mapping or validation are logged and counted as invalid, never retried; store or model failures retry the batch with backoff (at-least-once).

**Topic retention.** `catalog.product.v1` is compacted (`cleanup.policy=compact`), so a backfill reads roughly one event per product rather than the whole history.

**Vendor events (`catalog.vendor.v1`, PLAN.md R9).** Vendor data that changes independently of products: key `{tenant_id}:{vendor_id}`, headers `tenant-id`, `event-id`, `schema-version`, value `{event_id, tenant_id, ts, vendor: {vendor_id, score?, on_time_dispatch_rate?, dispute_rate?, response_hours?, verified?}}`. The topic is compacted and updates are partial, so producers should send every field. The indexer's consumer group reads it next to the catalog topic and writes `f:{tid}:v:{vendor_id}` in Redis; undecodable or out-of-range events are counted invalid and skipped, a Redis failure retries the batch. Vendor fields mapped from product payloads (`catalog.vendor_mapping`) are written by the same indexer, for products it actually applies; a Redis failure there is logged and does not stall catalog indexing.

**Category centroids.** Every backfill builds per-category centroids of the new version's `text` vectors (mean over a category's products, leaf and ancestors, re-normalized, at least 3 products) and stores them in S3 at `centroids/{tenant}/v{n}.bin` with a sha256 manifest before promoting the version. `featurejob centroids -tenant <slug>` (`make centroids`) refreshes them for the active version.

**Index versions.** The indexer creates and promotes `v1` on a tenant's first event. `indexer -backfill -tenant <slug>` rebuilds into `v{n+1}` from the start of the topic, checks that both stores hold exactly the live products, swaps both aliases, and drops versions older than the previous one.

**Index jobs and uploads (M4).** control-api queues a job in `index_jobs`: `POST /index/jobs`, or the onboarding `index_build` step. Any indexer replica claims the oldest queued job with `SELECT … FOR UPDATE SKIP LOCKED`, then:
1. For an upload job, it reads the tenant's JSONL from the uploads bucket (`uploads/{tenant}/{upload_id}.jsonl`, written through a 15-minute presigned PUT). Each line is one product in the tenant's own shape. Lines are published to `catalog.product.v1` as upserts in batches of 500, with the publish time as `source_version`, so an upload supersedes older data for the same products. Each line is first mapped with the job version's `catalog.field_mapping`; lines that fail mapping or validation are counted as failed and not published.
2. It backfills `v{n+1}` with the job's config version (same procedure as `-backfill`), recording progress in `total`, `done` and `failed`.
3. It verifies counts, swaps both aliases, and marks the job `succeeded` with its `index_version`, or `failed` with the error.

Measured locally: 200 uploaded products for a new tenant were indexed and searchable in about 65 s, most of it the backfill's catch-up wait.

The data-prep scripts do not compute embeddings, so indexing and query use the same embedding path.

**Text embedding input:** `"{title}. {brand}. {category leaf}. {attributes k:v joined}"`, truncated per tokenizer limits by the model server.

**Image embedding input.** The main image. Alternate images are not indexed in v1; they are reserved for eval.

**Qdrant collection `t_{tenant}_products_v{n}`:**
- Named vectors: `text` (size 384, Cosine) and `image` (size 512, Cosine).
- HNSW: `m=16`, `ef_construct=128`. Search `ef` comes from config (default 128). The optimizer's `indexing_threshold` is 1,000 KB, so HNSW is built even for small tenants (Qdrant's 10 MB default leaves a 10K-product collection on brute force, where filtered top-500 searches take ~100 ms instead of ~9 ms). Payloads use `memory: cached`.
- Payload indexes: `vendor_id`, `category_ids`, `brand`, `status`, `stock` (integer), `price_minor` (integer), `ship_regions`, `moq`.
- Point ID: UUIDv5 of `product_id`; `product_id` is also stored in the payload.

**BYO vector stores (M5).** A tenant with a BYO vector store (Qdrant, pgvector or Milvus; `docs/TENANT_CONFIG.md` §4) creates the collection or table itself; the indexer embeds products with the configured embedders (platform or BYO) and writes them into it through the declared vector names and `payload_mapping`.
- BYO stores have no versions: streaming writes and backfills go to the same collection in place (the lexical index is still versioned and swapped). After a backfill the indexer deletes the products the store holds that are no longer live, then checks that the store's count equals the live products before promoting the lexical version. Centroids are built from the BYO store's `text` vectors.
- Only fields the store holds are written: any field in Qdrant, the table's columns in pgvector, the schema fields (plus the dynamic field when enabled) in Milvus. Point IDs in Qdrant are the UUIDv5 of `product_id`, as on the platform. A product without an image vector is written without one (Qdrant), with NULL (pgvector) or with a zero vector (Milvus fields that are not nullable).
- A failing BYO provider affects only its tenant: the consumer retries that tenant's batch 3 times and then skips its events (logged; an index job restores them) instead of stalling every tenant.
- Catalog image URLs are tenant data: they are fetched through the egress client (https, public addresses only), except the platform's own S3 image host.

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
  users.jsonl                # hidden user ground truth (simulate.py)
  campaigns.jsonl
  events/part-*.jsonl.gz     # simulated interaction events (simulate.py)
  events/requests.jsonl.gz   # their request log (simulate.py)
  training/{tenant}.csv.gz   # learning-to-rank dataset (featurejob dataset, make train)
  images/{image_id}.jpg
  eval/image_queries.jsonl
  eval/text_queries.jsonl
  simulation_params.json
  MANIFEST.json              # counts, seeds, sha256 per file
```

`data/` is git-ignored. `make data` builds it from the downloaded archives.
