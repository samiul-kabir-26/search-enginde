# PLAN.md — Implementation plan

This plan turns `PROJECT.md` and `docs/*.md` into ordered, checkable work. Milestones follow `PROJECT.md` §6:
finish each one fully (code, tests, lint, docs, and `make up && make seed` working) before starting the next.

**Legend:** `[x]` done · `[ ]` to do · **R** resolved decision (see §0) that a step implements.

---

## 0. Where we are

**M2 is done** (M1 and M0 too; M0's green CI run is still blocked by the GitHub account billing lock, not by the code).

M2 verification (2026-10-25, same laptop, everything running locally including Chrome and the IDE):

| Check | Result |
|---|---|
| `POST /v1/search` | Relevant results on the demo catalog, e.g. "office chair", "gold earrings", "red running shoes under $80" (price phrase → `price_minor ≤ 8000`); predicted categories from 1,336 centroids; `?explain=true` trace with per-stage provider, latency, counts, fallbacks and top-50 scores |
| Paging and facets | Cursor, page-number and offset paging read one snapshot (no overlap); disjunctive facets verified against OpenSearch |
| `make loadtest` | 50 RPS for 60 s with the snapshot cache flushed (88% of requests ran the full pipeline): **p95 95.9 ms** (target 150), max 168 ms, 0 errors, 0 degraded, no breaker trips, 1.4% of embedding calls over the S1 budget. Warm reruns: p95 63–74 ms. One of six runs, started right after a full `go test` build, hit a CPU stall that tripped breakers (3.6% degraded, still 0 errors): the laptop shares 12 cores between every service, Triton, k6, the IDE and a browser. |
| Auth | `POST /v1/auth/token` (demo API key from `make seed`) → EdDSA JWT verified by search-api through the JWKS |
| Tests | Golden explain-trace test, unit tests for every stage, integration tests (MySQL, OpenSearch, Qdrant, Redpanda); coverage pipeline 87.6%, stages 94.7%, tenant 80.5% (`make cover`) |

M1 verification (2026-10-25, 12-core CPU laptop, CPU Triton):

| Check | Result |
|---|---|
| `make data` | 10,000 products across 162 product types, 200 vendors, 11,200 images (10,000 main + 1,200 eval alternates); deterministic (pytest: same seed gives the same sha256); recorded in `data/MANIFEST.json` |
| `make models` / `make models-smoke` | ONNX parity cosine 1.000000 for `clip_vision`, `clip_text_encoder`, `bge_encoder`; all 9 Triton models READY; shapes, L2 norms and a text→image sanity check pass |
| `make seed` | `demo` tenant created with the default config (active v1), 10,000 images in S3, 10,000 events published in 7 s |
| `make run-indexer` | Indexed all 10,000 products in about 7 min from cold start: 0 invalid, 0 missing images |
| `make backfill` | Rebuilt v3 from the topic in **7 m 31 s**; expected = lexical = vector = 10,000; aliases swapped |
| Tests | Unit tests (`-race`), integration tests on testcontainers (OpenSearch, Qdrant, Redpanda), pytest, golangci-lint and Redocly all pass |

M0 verification:

| Check | Result |
|---|---|
| `make up` | MySQL, Redis, OpenSearch, Qdrant, Redpanda, ClickHouse and SeaweedFS (S3) all report healthy. Kafka topics and S3 buckets are created. |
| `make migrate` | 12 MySQL control-plane tables and the ClickHouse `events` table are created. A second run is a no-op. |
| `search-api` / `control-api` | `/healthz` and `/readyz` (MySQL + Redis checks) return OK, `/metrics` serves Prometheus output, JSON access logs carry trace IDs, shutdown is graceful. |
| `make lint`, `make test`, `make build` | Pass (golangci-lint v2 and Redocly OpenAPI lint). |
| `make gen` | buf generates the KServe v2 gRPC stubs, sqlc generates the tenant queries, openapi-typescript generates the TS client. |
| Python (`tools/.venv`) | torch 2.14.1 (CPU), transformers, LightGBM, onnx, onnxruntime 1.31 and onnxmltools import cleanly. |
| Web (`web/onboarding`) | Next.js 16 app: typecheck, lint and build all pass. |
| ONNX Runtime | `third_party/onnxruntime` 1.31.0, matching `yalue/onnxruntime_go` v1.38.0. |

### Decisions made during setup (review these)

| # | Decision | Why |
|---|---|---|
| D1 | Go module path is `github.com/zavisoft/search-engine` | Not given in the spec. Rename with one `go mod edit -module` plus a sed if needed. |
| D2 | Spec docs moved from the repo root to `docs/` | `PROJECT.md` refers to them as `docs/*.md`. |
| D3 | Dev object storage is **SeaweedFS 4.48** (`chrislusf/seaweedfs`), replacing MinIO built from source | MinIO upstream is archived with no published images; SeaweedFS is maintained, Apache-2.0, ships official images, uses about 65 MB RAM, and passed S3 auth, anonymous read-only on the images bucket, and idempotent bucket creation. RustFS 1.0 was only days old. Code uses only the S3 API. |
| D4 | MySQL is exposed on host port **3307** | A local MySQL already uses 3306. All host ports are configurable in `.env`. |
| D5 | Events topic is named `search.events.v1` | The spec names only `catalog.product.v1`. |
| D6 | Generated code lives in `internal/gen/inference` (KServe protos) and `internal/tenant/tenantdb` (sqlc) | These paths are not in the spec's layout. |
| D7 | Compose profiles: default = infra, plus `init`, `models` (Triton), `obs` (OTel, Jaeger, Prometheus, Grafana) and `apps` (Go services + nginx) | The Triton image is about 15 GB, so it is only needed from M1 (`make up-models`). |
| D8 | OpenSearch 3.9.0, Qdrant 1.20.0, Redpanda 26.2.6, ClickHouse 26.8 LTS, Redis 7.4.11, MySQL 8.4.13, Triton 26.09 | Latest stable releases on the spec's pinned lines. Every image and dependency is pinned exactly. |
| D9 | Python tooling uses `uv` (pinned 0.13.5, binary in `./bin`) with dependency groups; torch comes from the CPU wheel index | No pip or venv bootstrap exists on the host, and this keeps setup reproducible. |
| D10 | `GOTOOLCHAIN=go1.27.3` is pinned in `go.mod` and the Makefile | The system Go is 1.22. golangci-lint must be built with a Go version at least as new as the module's. |
| D12 | The indexer's event-order guard is OpenSearch `version_type=external_gte` on `source_version` | No extra version store: stale writes are rejected by the lexical index and skipped for Qdrant; equal versions re-apply, so replays are idempotent (`docs/DATA.md` §6). |
| D13 | `select.py` downloads only the selected images (~11K, ~85 MB) instead of the 3 GiB tarball (used when present) | The ABO bucket gives about 4 KB/s per connection from this network; parallel per-image fetches take about 6 min. |
| D14 | Selection keeps only English category trees (`/Categories/...`) and dedupes on main image too | Non-English marketplaces have localized category paths; ABO variants share photos, which would duplicate products and make image eval ambiguous. |
| D15 | Pooling and L2 normalization are inside the exported ONNX graphs; ensembles take `[N]` BYTES with `max_batch_size: 0`, ONNX models batch dynamically | Keeps the §1 tensor contract exactly while still batching on the encoders. |
| D16 | search-api's HTTP layer lives in `internal/searchapi`; the filter AST and feature catalog are `internal/pipeline/filter` and `internal/pipeline/feature` | Not in the spec layout; separate packages avoid import cycles (tenant config uses the filter AST). |
| D17 | `tenants.plan` is a foreign key to the new `plans` table (migration 000002, seeded with R3) | Plans are data the admin panel edits (R7). |
| D18 | Retrieval reads only ranking fields (OpenSearch doc values, trimmed Qdrant payload); display fields are fetched for the returned page with one `mget` | Loading `_source` for 500–1,000 hits made BM25 50–400 ms; doc values take 20–35 ms. |
| D19 | Lexical and vector retrievers return `k` (config) hits, not the page depth; depth is reached through the RRF union | Keeps S2 inside 80 ms. |
| D20 | Breakers trip after 10 consecutive failures or ≥ 50% of ≥ 20 calls; ORT text encoders run 2 instances × 2 threads | With 5 consecutive failures and 4 threads, CPU oversubscription under load opened the embedder breakers; tuned until 50 RPS ran clean. |
| D21 | The catalog topic is compacted; Qdrant `indexing_threshold` is 1,000 KB and payloads are `memory: cached` | Backfills read each product once; HNSW is built for small tenants (filtered top-500 search: 102 ms → 9 ms). |
| D23 | Default stage budgets move 40 ms from S5 to S2 (`s2_ms` 120, `s5_ms` 40; total still 300) | `default_linear` ranks in < 1 ms while S2 runs three retrievers; at 80 ms, CPU contention timed out vector retrieval. The demo tenant's stored v1 was updated in place (dev data). |
| D22 | The tenant attribute dictionary is a new optional `query.attributes` section; facets are a new optional `facets` section; `retrieval.ef` is optional | The spec named the features but not where their config lives. |
| D11 | ABO is **CC BY 4.0**, not CC BY-NC as `docs/DATA.md` said | The official page and README were checked on 2026-10-25. `docs/DATA.md` and `ATTRIBUTION.md` are corrected. It is still used for demo and eval data only. |

### Resolved questions (answered 2026-10-25)

The owner answered Q1–Q11. Where they delegated ("choose the best", "do what you recommend"), the choice below is the plan's.
Each design is written into the spec docs and OpenAPI files in the milestone that implements it, before the code.

**R1 — Catalog envelope, with tenant-defined custom fields** (M1, M2)
- Topic `catalog.product.v1`. Key `{tenant_id}:{product_id}`, so updates to one product stay in order. Headers: `tenant-id`, `event-id`, `op`, `schema-version`.
- Value: `{event_id, tenant_id, op: "upsert"|"delete", ts, source_version, product}`. `product` is the tenant's own JSON, mapped through `catalog.field_mapping`.
- Delete is `op: "delete"` (product holds only the ID). A Kafka tombstone (null value) for the same key is treated as a delete. The indexer drops any event whose `source_version` is ≤ the stored one, which makes replays and out-of-order delivery safe.
- **Mandatory canonical fields:** `product_id`, `title`, `category_path`, `price_minor`, `currency`. All other canonical fields are optional and have defaults (`status` = active; `vendor_id` = the tenant itself).
- **Custom fields:** `catalog.custom_fields: [{name, type, source (JSONPath), filterable, facetable, searchable (boost), sortable, required}]`. Types: `keyword`, `keyword[]`, `text`, `int`, `float`, `bool`, `date`. The plan caps how many a tenant may define.
- Custom fields are stored under `custom.{name}`: an explicit OpenSearch mapping per index version (never dynamic mapping) and a Qdrant payload, indexed when filterable. The filter registry merges canonical and custom fields, so filters, facets and pushdown work the same for both. CEL sees them as `item.custom.x`.
- Adding a custom field is an online mapping update. Changing a field's type, or making it filterable, requires a reindex (`v{n+1}` plus an alias swap).

**R2 — ABO download** (M1). Download into `data/raw/abo/` (git-ignored); there is plenty of disk space (284 GB free):

| File | URL | Size | Needed |
|---|---|---|---|
| `abo-listings.tar` | https://amazon-berkeley-objects.s3.amazonaws.com/archives/abo-listings.tar | 83 MiB | yes |
| `abo-images-small.tar` (256 px images plus `images/metadata/images.csv.gz`) | https://amazon-berkeley-objects.s3.amazonaws.com/archives/abo-images-small.tar | 3.0 GiB | yes |
| `LICENSE-CC-BY-4.0.txt`, `README.md` | https://amazon-berkeley-objects.s3.amazonaws.com/LICENSE-CC-BY-4.0.txt (and `/README.md`) | small | yes |
| `abo-images-original.tar`, `abo-spins.tar`, `abo-3dmodels.tar` | — | 110 / 40 / 154 GiB | **no** |

```bash
mkdir -p data/raw/abo && cd data/raw/abo
curl -fLO https://amazon-berkeley-objects.s3.amazonaws.com/archives/abo-listings.tar
curl -fLO https://amazon-berkeley-objects.s3.amazonaws.com/archives/abo-images-small.tar
curl -fLO https://amazon-berkeley-objects.s3.amazonaws.com/LICENSE-CC-BY-4.0.txt
curl -fLO https://amazon-berkeley-objects.s3.amazonaws.com/README.md
```

**R3 — Plans** (M2). Plans live in a MySQL `plans` table, editable from the platform admin panel (R7), and `tenants.plan` references it. Seed values:

| Plan | max `total_ms` | Search RPS / burst | Events RPS | BYO providers | Egress | Custom fields | Max page depth |
|---|---|---|---|---|---|---|---|
| `starter` | 300 | 20 / 40 | 200 | no | none (platform only) | 20 | 500 |
| `growth` | 600 | 100 / 200 | 1,000 | yes | any public host | 50 | 1,000 |
| `enterprise` | 1,000 | 500 / 1,000 | 5,000 | yes | any public host, plus an optional per-tenant allowlist | 100 | 1,000 |

The SSRF guard (private, loopback, link-local and metadata ranges) always applies, whatever the plan.

**R4 — Pagination and facets** (M2). There are three pagination modes on the same request, and a request uses exactly one:
1. `page.cursor`: opaque and the default, suited to infinite scroll.
2. `page.number` + `page.size`: numbered pages. The response adds `total` and `total_pages`.
3. `page.offset` + `page.size`: raw offset, for integrations and exports.
- All three read from a cached, ordered result snapshot (Redis `snap:{tid}:{id}`, 10 min, at most max-depth IDs), keyed by tenant, config version, normalized query, filters and user. Paging is therefore stable: no duplicates across pages, and sponsored slots stay put.
- Cursors are HMAC-signed `{snapshot, offset, config_version}`. A tampered or expired cursor returns 400 `invalid_cursor`.
- Depth: positions 1..`rank.keep` are fully ranked. Positions past that, up to the plan's max depth, follow the pre-rank and then RRF order of the filtered candidates. `total_matches` (approximate, from OpenSearch) is also returned.
- Facets are configured per tenant (`facets: [{field, size}]`) on filterable or facetable fields, canonical or custom. Defaults: `category_ids` (20), `brand` (20), `price_minor` buckets, `ship_regions` (10).
- Facet counts come from OpenSearch aggregations over the full filtered lexical match set. They are disjunctive: a selected facet doesn't shrink its own counts (`post_filter`).

**R5 — JWT issuance before control-api** (M2). Accepted: M2 ships the `auth` issuer, the JWKS and `POST /v1/auth/token` as a thin control-api slice for tenant `demo`.

**R6 — Category centroids** (M2, M3). Centroids are the mean of the normalized `text` vectors per category (leaf and ancestors), re-normalized.
- They are computed by the index job before its alias swap, and refreshed by `featurejob` (hourly) to absorb streaming upserts.
- They are stored as a versioned binary blob in S3: `models/centroids/{tenant}/v{n}.bin` plus a sha256 manifest.
- search-api loads them into memory per (tenant, index version) through the registry LRU and runs brute-force kNN. For 5,000 categories × 384 dims that takes under 1 ms, and needs no Redis or Qdrant round trip.

**R7 — Auth settings, controllable from a platform admin panel** (M4, M9)
- A new **platform admin** role (operator, not tenant): a `platform_admins` table, JWT scope `platform:admin`, and endpoints under `/v1/admin/*`. The first admin is bootstrapped from `PLATFORM_ADMIN_EMAIL` by `cmd/seed`. The UI lives at `/admin` in the web app.
- Settings are stored in a `platform_settings` table (typed JSON, audited with `updated_by`) and validated against bounds. Changes propagate over Redis pub/sub `platform.settings.changed`, with no restart.
- Defaults and bounds:
  - Passwords: argon2id at the OWASP baseline (m = 46 MiB, t = 1, p = 1); parameters live in the PHC string, so raising them rehashes on next login. Minimum length 12.
  - Email: Mailpit in dev (SMTP 1025, UI 8025) and SMTP in prod. Verification tokens last 24 h.
  - Data-plane tokens from API keys: 15 min (bounds 5–60 min).
  - Console session: 15 min access token plus a 7-day rotating refresh token in an httpOnly cookie (bounds 1–30 days).
  - Ed25519 signing keys rotate every 30 days (bounds 7–90). The previous key stays in the JWKS for 7 days. The admin panel also has "rotate now" and emergency revoke.

**R8 — Idempotency and shadow logs** (M4). Accepted:
- Idempotency keys: Redis `idem:{tid}:{key}` with a 24 h TTL. The stored response is replayed. Reusing a key with a different body returns 422.
- Shadow results: ClickHouse table `shadow_results` with request_id, tenant, active and shadow versions, top-k IDs from each, overlap@10, and latencies.

**R9 — Vendor score and online features** (M6)
- **`vendor_score` does not need a vector.** It is a scalar ranking feature (float in [0, 1]); vectors are only for semantic text and image matching. The tenant computes it in their own system, and we just store and read it.
- Two ways in:
  1. A `vendor` block in the product payload, via `catalog.vendor_mapping` (JSONPath).
  2. `PUT /v1/vendors` (bulk, scope `catalog:write`) or topic `catalog.vendor.v1`, for when vendor data changes independently of products. This avoids reindexing every product of a vendor.
  - An optional `{min, max}` in the mapping rescales the tenant's own range to [0, 1].
- Vendor data is kept in Redis only, not in the search indexes. S3 (CEL `item.vendor`) and S4–S6 read it with one pipelined fetch per request (at most 300 distinct vendors). Filter pushdown on vendor fields other than `vendor_id` is not supported in v1.
- Redis keys use a `{tid}` hash tag, so they are Redis-Cluster safe:

| Key | Fields | TTL |
|---|---|---|
| `f:{tid}:i:{product_id}` | ctr_7d, ctr_30d, cvr_7d, cvr_30d, orders_30d_log, updated_at | 48 h (featurejob refreshes ≤ 15 min; expired → declared defaults) |
| `f:{tid}:v:{vendor_id}` | score, on_time_dispatch_rate, dispute_rate, response_hours, verified | none (the tenant is the source of truth) |
| `f:{tid}:u:{user_id}` | top-N category affinities, price_band | 30 days |

- For the demo tenant, the synthetic vendor score comes from `vendors.jsonl` using a documented formula.

**R10 — Ads** (M8)
- Tenants set up campaigns. Pacing is accepted: probabilistic throttling against the hourly spend curve, with state in Redis.
- Default quality score: `quality = 0.6·relevance + 0.4·pCTR`.
  - relevance: the S5 score, min-max normalized within the request.
  - pCTR: position-debiased ad CTR, Bayesian-smoothed toward the tenant's average sponsored CTR (prior strength 100 impressions).
  - Tenants can tune the weights in `ads.quality`.
- GSP: `price_i = bid_{i+1}·q_{i+1}/q_i + 1 minor unit`, capped at `bid_i`, with a per-tenant reserve CPC. Charging is CPC, on a `click` event with `sponsored=true`.
- **The platform admin controls** (`platform_settings.ads`, overridable per plan):
  - which campaign fields tenants can see and edit (bid, budget, targeting dimensions);
  - what a sponsored result shows (label text, vendor name, disclosure link);
  - max ad slots per page, a minimum relevance floor, and reserve-price bounds.

**R11 — Model licenses** (M1). `bge-small-en-v1.5` is MIT, so it is kept. For images, the platform model `clip-vit-b32` now loads **`laion/CLIP-ViT-B-32-laion2B-s34B-b79K`** (MIT) instead of `openai/clip-vit-base-patch32`.
- Why: OpenAI's model card declares "any deployed use case… out of scope". LAION's checkpoint has the same ViT-B/32 architecture, 512 dims and `transformers` support, and better zero-shot accuracy, so no spec dims change.
- SigLIP 2 (Apache-2.0) was rejected for now: it has 768 dims and about 8× the CPU cost, which breaks the ≤ 10 min backfill target. It can be offered later as an optional platform model.

Follow-ups confirmed on 2026-10-25: R4's three modes are exactly cursor, page number and offset; D3 chose SeaweedFS (delegated).

---

## M0 — Scaffold, Compose, Makefile, CI, OpenAPI, migrations ✅

- [x] Repository layout per `PROJECT.md` §4, with a package `doc.go` recording each package's responsibility
- [x] `go.mod` (Go 1.27.3); pinned dev tools in `./bin` via `make tools`: golangci-lint, sqlc, migrate, buf, protoc-gen-go, protoc-gen-go-grpc
- [x] `internal/platform`: env config (fail fast, all errors at once), slog JSON logger, OTel (Prometheus metrics, optional OTLP traces), HTTP serve with graceful shutdown, `/healthz` and `/readyz`, access log, RFC 9457 problems, plus tests
- [x] `cmd/search-api` and `cmd/control-api` boot with health, readiness and metrics. `indexer`, `featurejob` and `seed` are stubs that exit with their milestone.
- [x] `deploy/docker-compose.yml` with healthchecks and one-shot init jobs (topics, buckets); Dockerfiles for the Go services, MinIO and Triton; nginx, OTel collector, Prometheus and Grafana configs
- [x] `.env.example` (twelve-factor) and `Makefile` targets: `up down migrate gen lint test test-integration seed eval loadtest`, plus `setup tools build run-* nuke`
- [x] Migrations framework (golang-migrate): MySQL `000001_init` (`docs/TENANT_CONFIG.md` §2) and ClickHouse `000001_events`
- [x] sqlc config and first tenant queries; KServe v2 proto vendored at v0.21.0 and generated
- [x] OpenAPI 3.1 skeletons `api/openapi/search.yaml` (full schemas) and `control.yaml` (all paths; detailed bodies are filled per milestone), linted with Redocly
- [x] `tools/` Python project (`pyproject.toml`, `uv.lock`), `tools/dataprep/ATTRIBUTION.md`
- [x] `web/onboarding` Next.js 16 + TypeScript strict + Tailwind 4, typed client from `control.yaml`, Playwright config
- [x] CI workflow `.github/workflows/ci.yml`: Go lint/test/build plus generated-code drift check, integration, Compose health, Python ruff, web build
- [x] Pushed to `github.com/samiul-kabir-26/search-enginde` (public)
- [ ] **CI green on GitHub.** Blocked by the GitHub account billing lock; after that is fixed, run `gh run rerun 40089879724`.

---

## M1 — Data prep, canonical schema, Triton models, indexer, seed ✅

**Done when:** about 10K products are indexed into OpenSearch and Qdrant for tenant `demo`, and the counts are verified.

**Phase 1: data** (`tools/dataprep`, Python)
- [x] ABO metadata downloaded by `make data` (`download.py`, parallel ranges); real field names recorded in `docs/DATA.md` §2 (`node[].node_name`, list-valued `product_type`)
- [x] `select.py --seed 42`: filters (English category tree), dedup (item, title+brand, main image), stratification (≤400 per type, drop types with <20, equal quota), 10K target preferring items with alternates, 1,200 eval alternates → `data/products.raw.jsonl`, `data/eval/image_eval_seed.jsonl`, `data/images/`
- [x] `synthesize.py`: 200 Zipf vendors (`vendors.jsonl`), lognormal prices (`price_params.yaml`), moq, stock (5% zero), ship_regions, status, rating ~ hidden q, created_at, listing_completeness → `products.canonical.jsonl`; hidden quality in `data/hidden/`
- [x] `MANIFEST.json` with params, counts and sha256 per file; `make data`; pytest for determinism (same seed gives same sha256)

**Phase 2: canonical schema** (`internal/catalog`)
- [x] `Product` struct with the filterable-field registry, validation of mandatory fields, and the UUIDv5 point ID (**R1**)
- [x] Custom fields: `catalog.custom_fields` schema and validation, type coercion, `custom.{name}` storage, registry merge (**R1**); spec added to `docs/TENANT_CONFIG.md` and `docs/DATA.md`
- [x] JSONPath field mapping with `github.com/theory/jsonpath` (RFC 9535, `[*]` verified in tests), plus `Mapper.Preview` for M4
- [x] `catalog.product.v1` envelope: encode, decode, header checks, tombstones (**R1**)

**Phase 3: models** (`tools/models`, `deploy/triton`)
- [x] `export.py`: CLIP vision and text towers (`laion/CLIP-ViT-B-32-laion2B-s34B-b79K`, **R11**) and BGE-small → ONNX with dynamic axes; parity cosine ≥ 0.999 on 100 samples (measured 1.000000)
- [x] Triton repository: Python preprocessing models plus `clip_image`, `clip_text` and `bge_text` ensembles; CPU instance groups; dynamic batching (`max_queue_delay_microseconds` 2000)
- [x] `make up-models` healthy; `make models-smoke` checks every ensemble's shape, L2 norm and a semantic sanity case

**Phase 4: providers and binaries** (Go)
- [x] `providers/embedder/kservev2`: gRPC client, tensor names per `docs/MODELS.md` §1, batch splitting, raw and typed outputs, `INVALID_ARG` → `pipeline.ErrInvalidInput`
- [x] `providers/lexical/opensearch`: `mapping.json` plus custom fields, versioned indexes, alias swap, bulk upsert and delete with `external_gte` versioning (**D12**), health
- [x] `providers/vectorstore/qdrant`: named vectors `text`/`image`, HNSW m=16 / ef_construct=128, payload indexes on every filterable field, alias swap, upsert and delete
- [x] `cmd/indexer`: consumer group with manual commits and retry, per-poll dedup and concurrent chunks, first-use v1 creation, `-backfill` with count verification and alias swap; corrupt images isolated
- [x] `cmd/seed`: upload images to S3, create the `demo` tenant (tenant row, default draft, active v1, onboarding state), publish products as events in the storefront shape of the default mapping
- [x] Integration tests (testcontainers): OpenSearch, Qdrant, Redpanda
- [x] Verify the count match and a full backfill in ≤ 10 min on CPU Triton (7 m 31 s)

Carried forward: Triton runs at about 50% CPU during backfill, so ORT thread and instance tuning is left for M10 load work. Image URLs are fetched with a plain size- and time-limited HTTP client; M5 moves this to `security.EgressClient` with the platform image host allowed.

New deps: `twmb/franz-go`, `opensearch-project/opensearch-go/v4`, `qdrant/go-client`, `aws-sdk-go-v2`, `theory/jsonpath`, `oklog/ulid/v2`, `golang.org/x/sync`, `testcontainers-go` (OpenSearch, Qdrant, Redpanda modules).

---

## M2 — Text search: S1–S3, default_linear, explain, JWT, rate limit ✅

**Done when:** text search returns ranked results with a per-stage trace at `?explain=true`, and p95 ≤ 150 ms locally at 50 RPS. **Met: p95 95.9 ms cache-cold.**

- [x] `internal/tenant`: typed config sections, `DisallowUnknownFields`, `Validate(ctx, deps) []FieldError` (every rule without live deps, all errors reported), schema-version migration hook, sqlc store, TTL cache and resolver (plan + compiled always_filters)
- [x] `internal/auth`: EdDSA verify, JWKS fetch/cache/refresh on unknown kid, tenant from `tid`, scope middleware; control-api slice: JWKS + `POST /v1/auth/token` (API key → 15 min JWT), demo API key written by `make seed` (**R5**)
- [x] `internal/pipeline`: consumer-side interfaces, `Query`/`Hit`/`Candidate`/`Trace`, stage budget `min(stage, remaining)`, weighted RRF (k=60), explain trace (top 50), stage/request/empty metrics and spans
- [x] `internal/resilience`: `Call(ctx, key, primary, fallback)` with deadline and fallback reserve, `sony/gobreaker/v2` keyed by tenant+stage+provider, `search_stage_fallback_total`, `provider_breaker_state`
- [x] `FilterExpr` AST (`internal/pipeline/filter`): parse, validate and normalize (operators per type, depth ≤ 3, filterable fields incl. custom), translated to OpenSearch `bool` and the Qdrant payload filter; always pushed down
- [x] S1 text: NFKC, lowercase, whitespace collapse; price phrases → filter; attribute dictionary (tenant-extensible, **D22**); category kNN vote over centroids loaded from S3 (**R6**); text and text→image embeddings
- [x] Centroid builder (`internal/catalog/centroids`): built by every backfill and by `featurejob centroids`; `centroids/{tenant}/v{n}.bin` + manifest (**R6**)
- [x] S2 retrievers: `lexical_bm25` (field boosts, disjunctive facets), `vector_text`, `vector_text_to_image`, `category_popular`; errgroup fan-out; partial failure tolerated; all failing returns `degraded=true`, never 5xx
- [x] S3: `always_filters` pushdown (CEL rules come in M7)
- [x] S4/S5 `providers/scorer/defaultlinear` with normalized features, keep/cut preserving deep-page order; S6 passthrough with depth cut
- [x] `POST /v1/search`: strict validation, problem+json errors, configurable disjunctive facets, three pagination modes over a Redis snapshot with HMAC cursors and plan page depth (**R4**); `search.yaml` updated first
- [x] `plans` table migration and seed (**D17**); per-tenant GCRA rate limit from the plan (`go-redis/redis_rate`, **R3**)
- [x] Golden test for the explain trace; unit tests for every stage; coverage ≥ 80% on pipeline, stages and tenant
- [x] k6 `loadtest/search_text.js` at 50 RPS: p95 ≤ 150 ms (95.9 ms cache-cold, 63–74 ms warm)

Carried forward: load and isolation testing on dedicated hardware (M10), where breaker thresholds can be revisited with real contention data; config activation pub/sub (M4) replaces the 30 s resolver TTL; the registry's factories, LRU and BYO providers come in M4–M5; ONNX/KServe scorers in M7.

---

## M3 — Image search and similar items

**Done when:** image-eval recall@10 is reported (`docs/DATA.md` §7), and p95 ≤ 250 ms excluding upload.

- [ ] `internal/imaging`: ≤ 10 MB, magic-byte sniffing (JPEG, PNG, WebP via `x/image/webp`), `DecodeConfig` ≤ 4096², crop box, JPEG re-encode (strips EXIF)
- [ ] S1 image: CLIP image embedding; category = majority vote over the top-50 image neighbors
- [ ] `POST /v1/search/image` multipart (image, crop, q, filters, user, page, explain); `vector_image` plus text retrievers fused with RRF when `q` is present; `predicted_categories`
- [ ] `GET /v1/products/{id}/similar`: anchor's stored vectors → `vector_image` + `vector_text`, excluding the anchor
- [ ] `tools/eval`: build `data/eval/image_queries.jsonl` (alternate images); recall@10, recall@50, MRR report
- [ ] Unit tests for imaging (malformed, oversized, polyglot files) and image-query p95 check

---

## M4 — control-api: sign-up, config lifecycle, propagation

**Done when:** activating a new version changes search behavior within 5 s, without a restart.

- [ ] Auth: sign-up (tenant, owner, default draft, onboarding state), login (argon2id), email verification (Mailpit in Compose), users and roles, API keys (shown once, hashed), `POST /v1/auth/token`, session refresh rotation, Ed25519 key rotation and JWKS (**R7**)
- [ ] Platform admin: `platform_admins` and `platform_settings` tables, `platform:admin` scope, `/v1/admin/*` (settings with bounds, plans CRUD, key rotate/revoke, tenant list), `platform.settings.changed` pub/sub, bootstrap admin in `cmd/seed` (**R3**, **R7**)
- [ ] Idempotency-Key middleware: Redis `idem:{tid}:{key}`, 24 h, replay on match, 422 on body mismatch (**R8**)
- [ ] Config: draft GET/PUT, validate (returns every error), publish (immutable, monotonic version), activate, shadow (0–20%), diff
- [ ] Propagation: Redis pub/sub `tenant.config.activated` → search-api invalidation; 30 s reconcile loop; old-version provider drain
- [ ] `internal/registry`: `(kind, type)` factories, `Deps`, LRU per `(tenant, version, stage)` with close on eviction or supersede
- [ ] Shadow execution on sampled traffic (results logged to ClickHouse `shadow_results`, never returned; **R8**)
- [ ] Onboarding: state machine, `PUT /onboarding/steps/{step}` validation per `docs/ONBOARDING.md` §4, engine-explainer endpoints
- [ ] Catalog mapping preview, catalog upload presign, index jobs (build `v{n+1}`, alias swap, progress)
- [ ] Playground search (draft or version, explain on)
- [ ] Fill in `control.yaml` request/response schemas for every endpoint touched; regenerate the TS client
- [ ] Integration test: activate v2 → behavior changes within 5 s

---

## M5 — BYO providers, egress security, secrets, resilience

**Done when:** in a chaos test, a BYO endpoint that fails or hangs falls back to the default within the stage budget.

- [ ] `internal/security`: EgressClient (https only, resolve on every dial, `net.Dialer.Control` rejects RFC 1918, loopback, link-local, metadata, ::1 and fc00::/7), per-plan allowlist
- [ ] Indexer image fetches (`indexer.HTTPFetcher`) go through EgressClient, with the platform's own S3 image host explicitly allowed
- [ ] SecretStore interface: AES-256-GCM envelope encryption in MySQL (dev) and a Vault implementation; `POST /secrets`; never logged or returned
- [ ] Vector adapters: Qdrant (BYO), pgvector (`pgx/v5`, SQL WHERE pushdown), Milvus (Go client v2); `payload_mapping`
- [ ] Embedders: KServe v2 (gRPC plus REST fallback), `openai_embeddings` (client-side L2 normalization)
- [ ] Connections CRUD and tester (health, collection exists, dims/metric, sample latency); model register and validate (probe inference)
- [ ] Validation rules 3–6 with live probes; per-tenant bulkhead semaphores
- [ ] Chaos tests: failing, hanging and slow BYO endpoints; tenant isolation (no p95 impact on other tenants)

---

## M6 — Events, ClickHouse, features

**Done when:** online item and user features are readable in a request, with freshness ≤ 15 min.

- [ ] `POST /v1/events`: validation (order fields), ≤ 500 per batch, dedup by `event_id`, franz-go producer to `search.events.v1`
- [ ] search-api emits async `impression` events
- [ ] ClickHouse migration: Kafka engine table plus materialized view into `events`
- [ ] `tools/dataprep/simulate.py`: users (affinity, price band, region), sessions, hidden utility, 10% randomized positions, about 2M impressions → `data/events/`; `cmd/seed --events`
- [ ] `cmd/featurejob`: position-debiased CTR/CVR (7d/30d), orders, user affinity and price band → Redis keys and TTLs per **R9**; schedule ≤ 15 min; hourly centroid refresh (**R6**)
- [ ] Vendor data from the tenant: `catalog.vendor_mapping` in product payloads, `PUT /v1/vendors` (bulk) and topic `catalog.vendor.v1` → `f:{tid}:v:{vendor_id}`, with optional range normalization (**R9**)
- [ ] `providers/features`: Redis FeatureReader used by S4–S6

---

## M7 — Learned ranking and CEL policies

**Done when:** nDCG@10 on the held-out set beats `default_linear`.

- [ ] `tools/training`: ClickHouse dataset (graded labels order=3, cart=2, click=1), time split 80/10/10, LightGBM lambdarank prerank (≤ 15 features, ≤ 200 trees) and rank → ONNX via onnxmltools plus `.features.json`; metrics report
- [ ] Feature builder for the catalog in `docs/MODELS.md` §4 (ordered schema, declared defaults)
- [ ] `providers/scorer/onnx`: `onnxruntime_go`, one session per `(tenant, version)`, intra-op thread limit; ≤ 5 ms for 2000 candidates
- [ ] `providers/scorer/kservev2`: remote rank through Triton's ONNX backend (`rank-gbdt-v1`)
- [ ] ONNX upload flow: presign, ≤ 200 MB, sha256, dry-run against the declared schema
- [ ] `providers/policy`: cel-go restricted env (`item`, `query`), compile at validation, `cel.CostLimit`, ≤ 50 rules / ≤ 2 KB; boosts clamped to [-1, 1] → `policy_boost`
- [ ] Ranking eval: learned model vs `default_linear` on the held-out split

---

## M8 — Personalization, diversity, ads

**Done when:** sponsored slots are honored, and organic ranking is unaffected when ads are disabled (golden test).

- [ ] Personalization: `rank + α·affinity + β·price_band_match`; BYO re-ranker (KServe v2) replaces it
- [ ] Diversity: greedy `max_per_vendor` per window
- [ ] `internal/ads`: campaigns and creatives CRUD, eligibility, relevance floor, quality = relevance + smoothed pCTR, GSP with reserve CPC, probabilistic pacing in Redis, dedup against organic, `sponsored=true` (**R10**)
- [ ] Admin ad controls (`platform_settings.ads`, per-plan overrides): tenant-editable campaign fields, sponsored display fields, max slots, minimum floor, reserve bounds (**R10**)
- [ ] `data/campaigns.jsonl` (300) seeded; ads reports from ClickHouse
- [ ] Golden test: ads disabled gives organic output identical to the pre-ads baseline

---

## M9 — Onboarding wizard

**Done when:** a new tenant goes from sign-up to a live search playground end to end.

- [ ] Session as an httpOnly secure cookie; all control-api calls through Route Handlers and Server Actions
- [ ] `/onboarding/[step]` with a step guard; Server Components for data, client components for interactive pieces
- [ ] Components: `Stepper`, `EngineDiagram` (accessible, mobile-first SVG), `StagePanel`, `ProviderChooser`, `ConnectionForm`, `ModelRegistrationForm`, `JsonPathMapper`, `CelEditor`, `WeightSliders`, `SlotPicker`, `IndexProgress`, `PlaygroundSearch`, `ExplainTrace`
- [ ] Server `errors[].path` mapped to fields; focus management; `aria-live` progress
- [ ] `/admin` (platform admin): settings, plans, signing keys, ad controls, tenants (**R7**, **R10**)
- [ ] Playwright E2E per `docs/ONBOARDING.md` §6 plus an admin settings flow (`npx playwright install chromium` first)

---

## M10 — Eval, load tests, dashboards, runbook

**Done when:** reports are committed under `tools/eval/reports/` and `loadtest/reports/`.

- [ ] `make eval`: text (nDCG@10, recall@100, MRR), image (recall@10/50, MRR) and ranking suites → `tools/eval/reports/{date}_{version}.md`
- [ ] k6 scenarios: text, image, similar, BYO remote rank, plus an isolation test (one slow tenant) → `loadtest/reports/`
- [ ] Grafana dashboards (provisioned JSON) for the request/stage latency, fallback and breaker metrics in `docs/ARCHITECTURE.md` §10
- [ ] `docs/RUNBOOK.md`: start-up, common failures, breaker states, re-index, key rotation, rollback by activation

---

## Cross-cutting rules (every milestone)

- `context.Context` first on every I/O path; no package-level mutable state; interfaces live in `internal/pipeline`
- `tenant.ID` is required on every tenant-scoped query; the data plane never accepts `tenant_id` from a request body
- Every tenant-endpoint call goes through `security.EgressClient` and `resilience.Call`
- Update `api/openapi/*.yaml` **before** handlers; run `make gen` and commit the generated code (CI checks for drift)
- Pin every new dependency and image exactly; integration tests with testcontainers; coverage ≥ 80% on pipeline, stages, tenant and security

## Quick reference

```bash
make setup          # Go tools, Python venv, web deps, ONNX Runtime (first time)
make up-models && make migrate   # infra + Triton
make data && make models         # dataset (ABO subset) and ONNX exports, once
make seed                        # demo tenant, images, catalog events
make run-indexer                 # streaming indexer (or: make backfill TENANT=demo)
make run-control-api             # :8081  JWKS + token exchange
make run-search-api              # :8080  POST /v1/search
make token                       # JWT for the demo API key
make loadtest                    # k6, 50 RPS
make lint test
make up-obs         # + Jaeger :16686, Prometheus :9090, Grafana :3001
make down           # or `make nuke` to drop volumes
```
