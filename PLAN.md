# PLAN.md — Implementation plan

This plan turns `PROJECT.md` and `docs/*.md` into ordered, checkable work. Milestones follow `PROJECT.md` §6:
finish each one fully (code, tests, lint, docs, and `make up && make seed` working) before starting the next.

**Legend:** `[x]` done · `[ ]` to do · **R** resolved decision (see §0) that a step implements.

---

## 0. Where we are

**M9 is done** (M0–M8 too; M0's green CI run is still blocked by the GitHub account billing lock, not by the code).

M9 verification (2026-10-27, same laptop, dev stack with Triton; control-api, search-api and indexer from `make run-*`):

| Check | Result |
|---|---|
| Acceptance: sign-up to live playground | Playwright `e2e/onboarding.spec.ts` (docs/ONBOARDING.md §6): signs up a tenant, opens a stage in the engine tour, uploads 100 products as JSONL (storefront shape, demo images), keeps the mapping and retrieval defaults, adds a CEL filter and tests it on the samples (keeps 20 of 20), publishes and builds (index job polled to `succeeded`), runs a text and an image search with results and the explain trace, verifies the email through Mailpit and activates version 1 (API key shown once). Passes twice in a row (37 s, 25 s) |
| Guard and errors | A step ahead of `current_step` redirects to it; a taken slug comes back as a 409 at `/slug` and marks that input `aria-invalid`, keeping what was typed |
| Admin flow | `e2e/admin.spec.ts`: admin login, an out-of-range password length is refused at its field, valid settings and ad controls save and persist across reload (then restored); plans, signing keys and tenants render |
| Policy test endpoint | New `POST /v1/tenants/{tid}/policy/test` (OpenAPI first, TS client regenerated): each rule compiles alone and runs on the samples mapped with the draft's mapping, as S3 would; integration subtest covers keep, boost, unmappable samples and compile errors |
| Static checks | `next build` (Cache Components, partial prerendering), `tsc`, ESLint clean; Go build/vet (both tag sets), golangci-lint 0 issues, unit tests (`-race`) pass; Redocly clean for both specs |
| Found while testing | The access token's `ev` claim is stale after the user verifies, so go-live refreshes the session and retries once; the first searches after a search-api restart can time out (D38), visible in the explain trace, so the E2E retries them |

M8 verification (2026-10-27, same laptop, dev stack):

| Check | Result |
|---|---|
| Golden test & organic preservation | `TestExplainTraceGolden` matches `testdata/explain_trace.golden.json` byte-for-byte (unchanged by M8); `TestAdsGoldenAndOrganicPreservation` checks the organic order is identical with ads disabled, and the sponsored slot, organic dedup and order with ads enabled. `TestBlendHonorsEverySlot` covers several slots (fixed 2026-10-27: removing a later winner's organic copy used to shift an earlier ad out of its slot) |
| Personalization & BYO re-ranker | S6 adds `α·affinity + β·price_band_match` to the S5 score, or a BYO `kserve_v2` re-ranker (breaker, fallback) replaces it; both re-order only the candidates S5 ranked, so the tail keeps its pre-rank and RRF order (R4; fixed 2026-10-27, they used to sort the whole list across score scales) |
| Diversity | Greedy `max_per_vendor` per sliding window of `window` results, keeping score order among compliant items (fixed 2026-10-27: the window counted one result too many, and the pass copied candidates quadratically) |
| Ads auction & pacing | `internal/ads`: eligible ads are ranked candidates (S5/S6) matching targeting, above `relevance_floor` (S5 score min-max normalized among them), admitted by pacing (once per campaign per request); quality `0.6·rel + 0.4·pCTR`; GSP `min(bid, max(reserve, ⌈b₂q₂/q₁⌉ + 1))` with the tenant's `reserve_cpc_minor`; pacing throttles probabilistically against a uniform hourly spend curve in Redis. Sponsored clicks are charged once per placement at the recorded clearing price, which becomes the click's `value_minor` (fixed 2026-10-27: resent clicks were charged again, a client-sent value was kept, and shadow runs overwrote live clearing prices) |
| Admin ad controls | `platform_settings.ads` in `api/openapi/control.yaml` and control-api settings; config validation enforces `max_slots`, `min_relevance_floor` and the `reserve_bounds` on `reserve_cpc_minor`; bids below the minimum reserve are refused (enforcement added 2026-10-27) |
| Dataset & seeding | Deterministic `data/campaigns.jsonl` (300 campaigns with vendor targeting and creatives) from `tools/dataprep/synthesize.py` (byte-identical across runs), seeded into MySQL `ad_campaigns` and `ad_creatives` by `cmd/seed` |
| ClickHouse ads reports | `GET /v1/tenants/{tid}/ads/reports?from&to`: sponsored impressions, clicks, spend and CTR per campaign, attributed through each product's creative across all campaign statuses (fixed 2026-10-27: every product with events used to appear as its own "campaign") |
| Tests & lint | Unit (`-race`) and integration (`-p 1 -race`, 36 packages) pass after the 2026-10-27 fixes (before them `TestControlPlane` and `TestLearnedScorersAndPolicies` failed on M8 changes, and `TestBYOControlPlane` had failed since M7); `golangci-lint` 0 issues; Redocly clean for both specs; TS client in sync; pytest and ruff clean |

M7 verification (2026-10-26, same laptop, dev stack with Triton):

| Check | Result |
|---|---|
| Offline eval (nDCG@10) | Held-out test set (2,000 requests, graded: order 3, cart 2, click 1): **`rank-gbdt-v1` nDCG@10 0.3583 vs `default_linear` 0.2708 (+0.0875 gain)**, MRR 0.7006 vs 0.5293; **`prerank-gbdt-v1` nDCG@10 0.3348 vs `default_linear` 0.2563 (+0.0785 gain)**, MRR 0.6607 vs 0.5231. Position-debiased (IPW): `rank-gbdt-v1` +0.0243, `prerank-gbdt-v1` +0.0313. Report: `tools/eval/reports/2026-10-26_ranking.md` |
| Latency / stage budgets | `prerank-gbdt-v1` capped at 120 trees (115 trees selected) takes **3.90 ms** for 2,000 candidates on 1 ORT thread, fitting the 5 ms stage budget (**D69**); `rank-gbdt-v1` (329 trees) takes **1.6 ms** for 50 candidates in-process and ~2 ms via Triton remote KServe v2 |
| Feature catalog & replay | Dataset builder `featurejob dataset` replayed 1,000,000 impressions / 20,000 requests through Go serving S1/S2 and S4 feature builder (**D68**), training period online features aggregated as of training cutoff to eliminate leak; LightGBM lambdarank models exported to ONNX via onnxmltools with float32 inputs matching catalog schema |
| Scorer providers | In-process ORT scorer (`providers/scorer/onnx`) with thread tuning (`ONNX_INTRA_OP_THREADS`) and shared session cache; remote KServe v2 scorer (`providers/scorer/kservev2`) supporting Triton `rank_gbdt_v1` with raw and typed tensor formats; registry dynamic factory in `registry/scorers.go` |
| Model upload & dry-run | Presigned upload URL (`POST /v1/tenants/{tid}/models/uploads`), registration (`POST /models`) with ONNX dry-run against declared feature schema, sha256 validation; Triton model repository sync in `cmd/featurejob publish` (**D70**) |
| CEL policy engine | `providers/policy` with cel-go restricted env (`item`, `query`), cost limit (2000), AST validation; S3 filter stage evaluates rules concurrently across CPUs with deadline checks and fails open (**D63**–**D65**); `policy_boost` clamped to [-1, 1] applied additively to model output (**D66**) |
| Live search & explain trace | Query "office chair" with `explain: true` verified S3 filter evaluating CEL policy (3.8 ms), S4 prerank using `platform:prerank-gbdt-v1` (1.7 ms), and S5 rank using `platform:rank-gbdt-v1` (1.6 ms) with 22 features and `policy_boost` applied |
| Tests & coverage | Go unit tests (`-race`) and integration tests (`-p 1 -race`) pass (the featurejob integration test's `events.NewProducer` call was fixed on 2026-10-27; it did not compile before); pytest clean (11 passed); `make cover` **86.4%** (pipeline 88.0%, stages 94.7%, tenant 83.7%, security 80.8%); `make lint` 0 issues; Redocly clean for `search.yaml` and `control.yaml`; web app typecheck and lint pass |

M6 verification (2026-10-26, same laptop, dev stack with Triton):

| Check | Result |
|---|---|
| Acceptance: features readable in a request | `searchapi` TestOnlineFeaturesReadableInSearch: item, vendor and user features stored in Redis appear in the S4 explain trace and move a product from 3rd to 1st; Redis down → 200 with the declared defaults and `online_features:<reason>` in the trace. Live: after `make seed-events` + one featurejob run, all 50 traced candidates of `office chair` for `u_01317` carry `ctr_*`, `cvr_*`, `orders_30d_log`, `vendor_*` and both user features, and the top 5 changed |
| Freshness | Live: 40 clicks and 10 orders via `POST /v1/events` (202) were in ClickHouse after **2.3 s** and in Redis after **40.7 s** with the scheduler at 1 min (`orders_30d_log` 0 → 0.2398 = ln 11/10). At the default 10 min interval the worst case is ≈ 10 min plus a 2 s run, inside 15 min. Integration test (real Redpanda + ClickHouse): 9,137 events → Redis features in **8.8 s** |
| Kafka → ClickHouse | Migration 000003 (Kafka engine table + materialized view): 2,416,684 simulated events published by `seed -events` in 14.8 s and fully ingested 16 s later; `FINAL` collapses a resent event; 1,005 impressions emitted by search-api for warm-up searches arrived alongside (none `randomized`) |
| featurejob | One run for the demo tenant: **10,000 items and 4,999 users in 2.1 s**; propensity **η = 0.698** from 22,469 randomized clicks (the simulator's hidden value is 0.7; the integration test estimates 0.672 from 1,121 clicks); scheduled mode covers all 5 live tenants plus the hourly centroid refresh |
| S4 cost | 25 live queries, ~710 candidates each: S4 (feature read + scoring) **p50 4.9 ms, p95 6.6 ms** of its 15 ms budget, 0 fallbacks; before any features existed S4 took 2.9 ms |
| Vendors | `seed` published 200 vendors to `catalog.vendor.v1`; the indexer stored them in Redis 0.26 s later (no TTL, score per the DATA §4 formula, e.g. v_001 = 0.7613). `PUT /v1/vendors` partial updates keep unsent fields (test); payload `vendor_mapping` writes only for non-stale products and never fails indexing (test) |
| Simulator | `make data` step `simulate`: 2,000,000 impressions, 233,247 clicks, 112,904 carts, 70,533 orders, 10% randomized sessions, 2 m 22 s; two runs give byte-identical files (sha256 of every part, `users.jsonl` and params; pytest too) |
| User features vs hidden truth | Learned from events only: a user's hidden top category is among their learned top 20 for 9% of users (a random category: 1%), and 37% of learned price bands fall in the hidden band (5 bands). The simulator draws text queries independently of the user, so the personalization signal is real but weak (see M8) |
| Tests | Unit (`-race`) and integration (`-p 1 -race`, 30 packages) pass, including the new Kafka → ClickHouse → featurejob → Redis test. `make cover` **87.0%** (pipeline, stages, tenant, security). `make lint` clean; both OpenAPI files pass Redocly; TS client regenerated and the web app typechecks; pytest and ruff clean |
| Fix found live | The indexer hung at shutdown (pre-existing since M1): `Run` returned on cancellation after a poll without `AllowRebalance`, and with `BlockRebalanceOnPoll` leaving the group waits for it. `Consumer.Close` now releases it first; the integration test `TestConsumerClosesAfterCancel` hangs without the fix and passes with it |
| Environment note | The laptop had rebooted: the first searches timed out in S2 until Qdrant's vectors paged back in (M3 note), and one query during the bulk ClickHouse ingest still timed out on the vector retrievers. Linting with `--build-tags integration` reports the same 3 `contextcheck` findings in M4's control-plane harness as before |

M5 verification (2026-10-25, same laptop):

| Check | Result |
|---|---|
| Acceptance: chaos test | `internal/e2e` TestBYOChaos: a tenant with a BYO OpenAI-compatible embedder and a real BYO Qdrant behind a TCP fault proxy, default budgets (S1 40, S2 120, total 300 ms). Request p95: embedder fails **1.8 ms**, hangs **42.9 ms**, slow (250 ms) **44.5 ms**; store refuses **0.9 ms**, hangs **122.7 ms**, slow **123.3 ms**. Every request returned results (platform lexical), none degraded, each stage inside its budget; breakers opened after 10 failures and later fallbacks are immediate. Under `-race`: ≤ 128.9 ms |
| Isolation | Tenant B's p95 while tenant A sends steady traffic (8 clients, 100 RPS each): **0.24 ms with A healthy, 0.24 ms with A's embedder and store hung** (3.8 vs 3.7 ms under `-race`); A's bulkhead peaked at its limit of 8 calls in flight |
| Live (dev stack) | A new growth tenant configured only through control-api: secret, BYO `kserve_v2` model (Triton `bge_text`; gRPC fails on its HTTP port, so REST was detected and recorded), BYO pgvector connection (test: health, collection, vectors, fields, sample query); the draft validated live; 200 uploaded products → index job **succeeded in 16 s**, 200/200 rows with text and image vectors in the tenant's own table; activated. Search p50 **24 ms** (S1 `byo:kserve_v2:bge_text` 15 ms). With the pgvector container **paused**: 30/30 searches returned results, p50 23 ms, max 150 ms (S2 timeouts, then `breaker_open`); unpaused, vector retrieval came back without fallback. The demo tenant (platform providers) was unaffected; no secret values in any log |
| BYO adapters | One conformance suite (writes, filtered kNN with pushdown including arrays, `ne` and custom fields, get, count, scroll, describe, delete) passes on real Qdrant 1.20, pgvector 0.8.7 (PostgreSQL 17) and Milvus 2.6.25 |
| Security | Egress tests: private, loopback, link-local, metadata, CGNAT and IPv6 ranges; a host name resolving to loopback is refused at dial time; no redirects, no plain http, allowlist; the metadata address is rejected at registration (integration). Envelope encryption: a ciphertext moved to another tenant does not decrypt. Vault KV v2 against Vault 2.1.2 |
| Control plane | Integration tests: secrets (stored encrypted, never returned), models (validation, dimension mismatch, gRPC→REST detection), connections (egress, TLS, test checks, status), BYO draft validated live and published, live-connection update tested before saving, in-use delete refused, starter plan refused, tenant isolation |
| Tests | Unit (`-race`) and integration (`-p 1 -race`) pass. `make cover` **86.8%** (pipeline 88.0, stages 94.4, tenant 83.7, security 80.8). `make lint` clean; `control.yaml` passes Redocly; TS client regenerated and the web app typechecks |
| Environment note | The Milvus test container needs about 1.5 GB of memory. Linting with `--build-tags integration` (not part of `make lint` or CI) still reports 3 `contextcheck` findings in M4's control-plane test harness |

M4 verification (2026-10-25, same laptop, against the running stack):

| Check | Result |
|---|---|
| Acceptance: activation → search changes | e2e test (`internal/e2e`, real MySQL + Redis, control-api and search-api in one process): **v2 served 5–11 ms after the activate call**. Live: v{n} with a brand-only always_filter served **226–279 ms** after activation, which includes the new version's first full pipeline run; no restart |
| Sign-up and auth (live) | Sign-up 201 with the refresh cookie; an Idempotency-Key replay returns the same tenant; activation before email verification is refused (403); the verification mail arrives in Mailpit and its token verifies; refresh rotation returns `email_verified=true`; platform admin login from `make seed`; first signing key generated at control-api start |
| Config lifecycle (live) | Draft edit, playground search on the draft (trace included, only the filtered brand returned), publish, activate, roll back by re-activating v1, draft restored |
| Index job from an upload (live) | New tenant: presigned JSONL upload of 200 products, publish, job claimed by the indexer: **succeeded in 63–69 s**, 200/200, index v1; activated; a new API key's search returns only uploaded products |
| Shadow (live) | Demo v2 as shadow at 20%: 100 fresh queries wrote **19–20 comparison rows** to ClickHouse `shadow_results` (overlap@10, both latencies). One run made while the integration suite was starting its containers had every shadow run degraded (budget timeouts on the loaded laptop); the rerun had 0 degraded |
| Isolation | Another tenant's token gets 404 on `/v1/tenants/{tid}/...` (no tenant probing); role scopes enforced (integration test) |
| Tests | Integration tests for every control-plane flow (sign-up, login, refresh reuse detection, verification, users and roles, API keys and token exchange, config lifecycle, idempotency, onboarding through go-live, admin settings/plans/key rotate and revoke, playground proxy, uploads), all passing with `-race`. `make cover` 80.9% (pipeline, stages, tenant); controlplane 68%, security 87%, registry 61% on their own tests. `make lint` clean; `control.yaml` passes Redocly; TS client regenerated and the web app typechecks |
| Environment note | `make test-integration` starts every package's testcontainers at once; with the whole stack running and swap full, MySQL and Redpanda containers missed their start deadlines (6 packages). The same packages pass when run one at a time (`go test -p 1 -tags integration`). The first queries right after a search-api restart can also blow the stage budgets while connections open (**D38**) |

M3 verification (2026-10-25, same laptop):

| Check | Result |
|---|---|
| `make eval` (image suite) | 1,119 queries: **recall@10 0.583**, recall@50 0.727, MRR 0.388 (product itself); graded-set recall@10 0.149, nDCG@10 0.411; categories predicted for 91%; 0 degraded. Raw image kNN without ranking scores recall@10 0.535 on a 400-query sample, so ranking helps; the remaining misses are mostly alternate photos showing a different view. Report: `tools/eval/reports/2026-10-25_v1.md` |
| Image latency | p95 166 ms with 4 concurrent clients (target 250 ms); single requests 56–84 ms end to end with the vectors warm |
| Similar items | 22 ms; anchor excluded, always_filters applied |
| Uploads | Magic-byte sniffing, header dimension check, crop, downscale and EXIF-stripping re-encode; polyglot, truncated, oversized and decompression-bomb cases rejected (tests) |
| Environment note | The laptop swaps (3.6 of 4 GB swap in use with every service, Triton, an IDE and a browser); Qdrant's image vectors were paged out after an hour unused and the first image queries took up to 2.8 s until warm |

M2 verification:

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
| D24 | `budgets.image` overrides stage budgets for image and similar queries (default `s1_ms` 130, total 300 to fit every plan) | Image embedding on CPU (~35–60 ms plus the category vote) cannot fit the 40 ms text S1. |
| D25 | Image queries without `q` return no facets; `q` adds the text retrievers and facets | Facets come from the lexical match set (R4); an image-only query has none. |
| D26 | The image category vote falls back up the category path when no leaf reaches 15% | ABO's 1,336 fine leaf categories made leaf-only votes empty for most queries (91% now get categories). |
| D27 | Similar items returns at most `size` results without snapshot or cursor | The spec defines only `size`; similar lists are short and cheap (22 ms). |
| D22 | The tenant attribute dictionary is a new optional `query.attributes` section; facets are a new optional `facets` section; `retrieval.ef` is optional | The spec named the features but not where their config lives. |
| D28 | JWT signing keys live in MySQL `signing_keys`, private keys sealed with AES-256-GCM under `SECRETS_MASTER_KEY` (kid as AAD); rotation and revocation hold a MySQL advisory lock; `JWT_SIGNING_KEY`/`JWT_TTL` env vars are gone | Every control-api replica signs with the same active key, rotation needs no redeploy, and the admin can revoke a key (**R7**) |
| D29 | Console sessions: 15 min access token in the body plus an opaque refresh token (sha256 stored) in the `se_refresh` cookie, rotated on every use; reusing a rotated token revokes the family | Standard rotation with theft detection; the cookie is HttpOnly and SameSite=Strict, `Secure` outside dev |
| D30 | Activation (config activate and onboarding go-live) requires a verified email; platform admins are exempt | Stops unverified sign-ups from going live; R7 asked for verification but not where it gates |
| D31 | A token for another tenant gets 404, not 403, on `/v1/tenants/{tid}/...` | Tenant IDs cannot be probed |
| D32 | The playground calls search-api through a control-api proxy with a one-minute `search:playground` token; only that scope makes search-api honour `X-Config-Version` (version or `draft`), and playground runs force explain and skip snapshots and shadow | Keeps draft and version pinning out of the public data-plane contract; the console never holds a search token |
| D33 | Shadow runs only on fresh pipeline runs (not snapshot hits), on a bounded worker pool (`SHADOW_WORKERS`, default 8) that drops instead of queueing; rows are batched to ClickHouse (1 s or 500 rows) and the top 10 of each side is stored | Shadow load stays proportional to real pipeline work and can never slow or fail the primary request (**R8**) |
| D34 | `TENANT_CONFIG_TTL` default 30 s → 10 min: Redis pub/sub is the primary invalidation, the 30 s reconcile against MySQL is the fallback, the TTL is only a backstop | M2's short TTL was the stop-gap that M4's propagation replaces; fewer MySQL reads per instance |
| D35 | Mailpit v1.31.6 in Compose (SMTP 1025, UI 8025) for verification mail | Dev SMTP with an HTTP API the tests and smoke scripts read |
| D36 | Index jobs are queued in `index_jobs` by control-api and run by the indexer (`FOR UPDATE SKIP LOCKED`); uploads are JSONL in the tenant's own shape, published as catalog events and then backfilled into `v{n+1}` | One indexing path for streamed and uploaded data, and the indexer already owns embedding and alias swaps |
| D37 | Onboarding accepts only what exists today: BYO embedders and stores return `byo_unavailable` until M5, learned and BYO ranking `not_available` until M7, CEL rules are saved with a `deferred` warning until M7; retrieval presets are balanced, keyword, semantic and visual | Steps validate against real capabilities instead of accepting config that cannot run |
| D38 | A degraded result with no items is not snapshotted, and an activation prebuilds the new version's providers | Found in the live smoke test: the first queries after a search-api restart timed out while connections opened, and the empty degraded result was then served from the snapshot for 30 s. A startup warm-up is carried forward |
| D39 | The Milvus adapter speaks Milvus's gRPC API (`milvus-proto/go-api/v2` v2.6.17) instead of the Go client v2 | The client v2 imports Milvus's server `pkg` (72 modules, including an embedded etcd 3.5) that does not compile against otelgrpc v0.70, which our OTel exporters require through grpc-gateway. The proto module needs only grpc and protobuf and is the same API the client wraps; the test container uses Milvus's standalone_embed recipe |
| D40 | BYO vector stores are written by the indexer, in place: no `v{n}` versions or alias swap; a backfill re-upserts every live product, deletes the ones no longer live, and requires an exact count before promoting the lexical version | Owner's choice (2026-10-25): one indexing path for every store. The tenant creates the collection or table; the connection test checks it |
| D41 | A failing BYO embedder or store degrades to the platform: its vector retrievers drop out and the lexical index carries the request; when `lexical_bm25` is not in the mix, a BYO vector retriever falls back to one platform BM25 search per request, fused under `lexical_bm25` | Owner's choice (2026-10-25) over dual-indexing every BYO tenant on platform vectors |
| D42 | BYO lexical stores are not supported in v1 (`byo_unavailable`) | `PROJECT.md` lists only vector adapters for v1 |
| D43 | BYO factories do no network I/O; a provider that cannot be built (missing, unvalidated, bad secret) fails each call and is rebuilt after 30 s; BYO providers are rebuilt every 5 min | A hung endpoint must never block outside a stage budget, and a BYO problem must never 503 the request; the refresh picks up connection edits and secret rotations |
| D44 | Bulkhead: 32 concurrent BYO calls per tenant per search-api (`BYO_MAX_CONCURRENCY`); a full bulkhead refuses at once (`bulkhead_full`) | Queueing would spend the stage budget waiting; refusing lets the fallback run |
| D45 | The plan allowlist applies to tenant-registered endpoints; catalog image URLs get the SSRF guard and https but not the allowlist; the platform's own hosts are trusted via `EGRESS_TRUSTED_HOSTS` (and the indexer's S3 hosts) | Image URLs are catalog data on tenants' CDNs; starter plans cannot register endpoints at all (`byo_allowed` false) |
| D46 | KServe REST is chosen at model validation (gRPC first, REST when gRPC fails), not per call; images over REST use the binary tensor data extension, text JSON strings | Per-call fallback would double latency on a hung endpoint; binary images are not valid JSON strings |
| D47 | Embedder schemas are `input_schema {modality: text\|image}` and `output_schema {dim}`; `tenant_models` gains `settings` (transport, max batch, text-to-image), `last_error`, `updated_at` (migration 000004); connection `settings` are `{username, database}` | The spec named the columns but not their contents for embedders |
| D48 | Updating a connection that serves the active or shadow version runs the connection test with the new settings and saves only if it passes; deleting a connection referenced by the draft or a non-retired version is 409 | A credential rotation or typo must not break live search |
| D49 | The indexer retries a BYO tenant's failing batch 3 times, then skips that tenant's events in the batch (logged; an index job restores them) | Before M5 a failing batch blocked every tenant; a tenant's own endpoint must not stall platform indexing |
| D50 | Rules 3–5 probe live on draft save, validate and publish (5 s per probe); successful probes are cached 30 s, failures never | Wizard edits would otherwise call tenant endpoints on every save |
| D51 | Milvus searches use Bounded consistency; gets and counts use Strong | Search latency; writes become searchable within Milvus's graceful time (~5 s) |
| D52 | Vault is used through its HTTP KV v2 API (no SDK); secret refs are ULIDs and the Vault path includes the tenant | Two calls are needed; refs are validated before use in a path |
| D53 | Events carry an optional `randomized` flag (impressions only), stored in a new ClickHouse column | featurejob must know which impressions were shown in a randomized order to estimate position propensities (docs/DATA.md §5, MODELS §4); the event schema had no marker. search-api does not run exploration slots yet, so only tenants (and the simulator) send it |
| D54 | Propensity `p(pos) = pos^-η`, η fitted from randomized traffic (≥ 200 clicks, ≥ 5 positions, clamped to [0, 2]), kept in `f:{tid}:propensity`; previous estimate, then η = 0, as fallbacks. CTR = clicks per examination with a 20-examination prior toward the tenant mean; CVR = orders per click with a 10-click prior | The power law fits the examination curve with one parameter and stays smooth at deep positions; shrinkage keeps items with a few impressions from being ranked by noise. A click implies examination, so CVR needs no position correction. On the synthetic data η = 0.70 (hidden 0.7) |
| D55 | search-api emits one impression per returned result (every page, image search, similar items) with `TryProduce`: a 50,000-record buffer and 30 s delivery timeout, drops counted in `events_dropped_total`; Kafka is not in `/readyz` | Impressions must never slow or fail a search, and a Kafka outage must not take search out of rotation. Tenants send clicks, carts and orders referencing `request_id` |
| D56 | `event_id` dedup happens within a batch at ingest and across batches in ClickHouse (`ReplacingMergeTree`, queries use `FINAL`); no Redis seen-set | Retries of a batch are safe without a per-event Redis write; every featurejob query already reads `FINAL` |
| D57 | User features come from events only: interactions weighted order 3, cart 2, click 1 over 30 days; `category_affinity` = share of weight per category ID (top 20); price band = weighted 20th–80th percentile of interacted prices in minor units (≥ 3 products), stored as `price_lo`/`price_hi`; categories and prices are read from the tenant's lexical index | The simulator's `users.jsonl` is hidden ground truth and must not leak into features (the earlier draft read it). Minor-unit bands work for any currency, unlike named bands |
| D58 | Vendor updates are partial on every path (only sent fields are written); `response_hours` is stored raw and ranked as min(h/48, 1); `{min, max}` rescaling only for score and rates. `PUT /v1/vendors` writes Redis directly (plan events rate); `catalog.vendor.v1` and `catalog.vendor_mapping` are written by the indexer, payload vendors only for non-stale products, and a Redis failure there is logged without stalling catalog indexing | Defaults written for unsent fields (earlier draft) would overwrite real values. The update shape lives in `internal/catalog` (`catalog.Vendor`) to avoid an import cycle with `providers/features` |
| D59 | featurejob runs every 10 min (`FEATUREJOB_INTERVAL`) for every tenant with an active config, centroids hourly for tenants with a platform vector index; logic in `internal/featurejob` | 10 min plus a run of seconds keeps features ≤ 15 min old; a package makes the job testable against real ClickHouse |
| D60 | S4 holds the `FeatureReader` (platform-wide) instead of `Providers`; the read goes through `resilience.Call` (breaker `online_features`) with half of S4's remaining budget; failure → declared defaults and trace reason `online_features:<reason>` | S4's budget is 15 ms (10 ms for image queries); scoring must keep time, and Redis trouble must never fail a search |
| D61 | `seed` publishes demo vendors to `catalog.vendor.v1` with a documented score formula (DATA §4); `seed -events` shifts event timestamps so the simulator's `ref_date` lands on the current time | Exercises the real vendor path instead of writing Redis directly; the 7/30-day windows stay populated whenever the stack is seeded |
| D62 | The ClickHouse Kafka table names the Compose broker (`redpanda:9092`), types `type` as an Enum in the Kafka table and skips up to 1000 broken messages per block; ISO timestamps parse with `best_effort` | A bad message is skipped instead of blocking the stream (a failing CAST in the view would block it). Integration tests substitute their broker; production needs its own broker setting (carried forward) |
| D63 | CEL `item` and `query` are `map(string, dyn)` variables, with field names checked by walking the compiled AST; `item` is the ranking doc (extended with brand, stock, status, ship regions and filterable custom fields, read by every store's retrievers) plus `vendor` from the online features | cel-go native types caught typos too but could not hold `custom` and were no faster (~3.5 µs for the documented example rule pair); retrievers already read ranking fields, so rules need no extra round trip |
| D64 | `internal/providers/policy` depends only on cel-go; `tenant.PolicyRule` aliases `policy.Rule` | Config validation (tenant) and S3 compile the same rules without an import cycle (pipeline imports tenant) |
| D65 | S3 evaluates rules in parallel across CPUs, checks the deadline between chunks of 32, and fails open: a rule that errors keeps the candidate and adds nothing, candidates not evaluated in time pass unchanged; both are counted in the trace. When rules exist S3 reads the online features (for `item.vendor`) and S4 reuses them | A broken or slow rule must never empty results or fail a search; one Redis round trip per request |
| D66 | Learned scorers: `{type, model}` names a platform scorer, `{type, model_id}` a tenant's registered model; `policy_boost` is added to a learned model's output instead of being an input; a scorer that cannot load or fails falls back to the stage's input order and is rebuilt after 30 s | Models are trained without tenant rules, so boosts must stay effective after retraining; a model outage must not 503 search |
| D67 | Request log: search-api writes one record per served page to `search.requests.v1` → ClickHouse `search_requests` (180-day TTL); the simulator writes the same log for its sessions | Training needs each request's query, which impression events do not carry; real tenants' traffic gets the same training path |
| D68 | The dataset is built by the Go serving code (`featurejob dataset`): each logged query is replayed through S1/S2 restricted to its impressed products, S4's feature builder runs, item features are aggregated as of the end of the training period, vendor features are current | No train/serve feature skew from a second implementation; no leakage of evaluation-period clicks into features. RRF ranks are within the restricted set (documented skew) |
| D69 | Pre-rank is capped at 120 trees (200 took 7.4 ms for 2,000 candidates, over the 5 ms budget); learning rates are picked from a small grid by validation nDCG@10; the report evaluates all test requests and, separately, with inverse propensity weights (position-debiased); each model is compared with `default_linear` at its own stage | Spec bound beats the tree cap; the test split is only used for reporting; logged clicks are position-biased |
| D70 | Platform scorers live in the models bucket (`platform/<id>/model.onnx` + `features.json` with sha256); `featurejob publish` uploads them and writes Triton's `rank_gbdt_v1` (restart Triton to load); tenant uploads go to `tenants/<tid>/models/<upload>.onnx` and their sha256 is checked on every load | One artifact store for platform and tenant models; integrity checked end to end |
| D71 | Services are built with cgo and ship ONNX Runtime in a distroless `cc` image; the runtime is initialized once per process; without it ONNX uploads are refused (503) and ONNX scorers fall back | onnxruntime_go loads the library with dlopen; ORT's environment is process-global by design |
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

## M3 — Image search and similar items ✅

**Done when:** image-eval recall@10 is reported (`docs/DATA.md` §7), and p95 ≤ 250 ms excluding upload. **Met: recall@10 0.583, p95 166 ms.**

- [x] `internal/imaging`: ≤ 10 MB, magic-byte sniffing (JPEG, PNG, WebP via `x/image/webp`), format-specific `DecodeConfig` ≤ 4096² before decoding, crop box, downscale to a 336 px short side, JPEG re-encode (strips EXIF)
- [x] S1 image: CLIP image embedding; category = similarity-weighted majority vote over the top-50 image neighbours with leaf-to-parent fallback (**D26**); `budgets.image` (**D24**)
- [x] `POST /v1/search/image` multipart (image, crop, q, filters, user, page, explain) with the same paging and snapshots as text search; `vector_image` plus text retrievers fused with RRF when `q` is present (**D25**); `predicted_categories`
- [x] `GET /v1/products/{id}/similar`: anchor's stored vectors → `vector_image` + `vector_text`, anchor excluded by a pushed-down filter (**D27**)
- [x] `tools/eval`: builds `data/eval/image_queries.jsonl` (alternate images, graded relevance); recall@10, recall@50, MRR, graded recall, nDCG@10 and latency; report in `tools/eval/reports/`
- [x] Unit tests for imaging (malformed, oversized, polyglot, EXIF) and handlers; image-query p95 measured by the eval (166 ms)

---

## M4 — control-api: sign-up, config lifecycle, propagation ✅

**Done when:** activating a new version changes search behavior within 5 s, without a restart. **Met: 5 ms in the e2e test, 226–279 ms live (including the new version's first pipeline run).**

- [x] Auth: sign-up (tenant, owner, default draft, onboarding state; idempotent), login (argon2id, PHC strings, rehash on login when parameters change), email verification (Mailpit in Compose, **D35**), users and roles (owner, admin, developer, viewer → `tenant:read|write|admin`), API keys (shown once, hashed), `POST /v1/auth/token`, refresh rotation with reuse detection (**D29**), Ed25519 key ring with rotation, revocation and JWKS (**D28**, **R7**)
- [x] Platform admin: `platform_admins` and `platform_settings` tables (migration 000003), `platform:admin` scope (token without `tid`), `/v1/admin/*` (settings with R7 bounds, plans list/upsert, signing keys list/rotate/revoke, tenant list), `platform.settings.changed` pub/sub, bootstrap admin in `cmd/seed` from `PLATFORM_ADMIN_EMAIL`/`PLATFORM_ADMIN_PASSWORD` (**R3**, **R7**)
- [x] Idempotency-Key middleware: Redis `idem:{scope}:{key}` (scope = tenant, or `signup`), 24 h, replay on match, 422 on body mismatch, 409 while the first request runs (**R8**)
- [x] Config: draft GET/PUT (saved even when invalid, validation returned), validate (every error), publish (immutable, monotonic under `SELECT … FOR UPDATE`), activate (needs a verified email, **D30**), shadow (0–20%, 0 clears), diff (JSON Pointer changes)
- [x] Propagation: Redis pub/sub `tenant.config.activated` → resolver invalidation in search-api; 30 s reconcile against MySQL; superseded providers drain for 30 s, then close; the new version's providers are built ahead of traffic (**D34**, **D38**)
- [x] `internal/registry`: `(kind, type)` factories, `Deps`, LRU per `(tenant, version, role)` with close on eviction or supersede after the drain grace, time-sensitive entries (centroids) refreshed
- [x] Shadow execution on sampled fresh pipeline runs, bounded worker pool, batched into ClickHouse `shadow_results` (migration 000002, 90-day TTL), never returned (**D33**, **R8**)
- [x] Onboarding: state machine (in-order unlock, revisit allowed, `use_defaults`), per-step validation that edits the draft, presets, go-live creates the first API key; engine explainer (base and tenant with Default/Custom badges) (**D37**)
- [x] Catalog mapping preview, presigned JSONL upload, index jobs run by the indexer (upload → catalog events → backfill `v{n+1}` → alias swap; progress, status) (**D36**)
- [x] Playground search, text and image (draft or a version, explain forced) through a control-api proxy (**D32**)
- [x] `control.yaml` request/response schemas for every M4 endpoint; TS client regenerated
- [x] Integration tests: control-plane flows on MySQL + Redis; e2e "activate v2 → search changes within 5 s" (`internal/e2e`)

Carried forward: the admin and tenant console UIs (M9 builds the onboarding wizard on these endpoints); a startup warm-up for search-api, because the very first queries after a restart can exceed the stage budgets while gRPC/HTTP connections open (now they are no longer cached when empty, **D38**); BYO stores, embedders and models (M5, M7); CEL policies (M7).

New deps: `golang.org/x/crypto/argon2`, `clickhouse-go/v2` (moved into search-api), `alicebob/miniredis/v2` (tests), testcontainers Redis module.

---

## M5 — BYO providers, egress security, secrets, resilience ✅

**Done when:** in a chaos test, a BYO endpoint that fails or hangs falls back to the default within the stage budget. **Met: `internal/e2e` TestBYOChaos, every failure mode inside its stage budget (see §0).**

- [x] `internal/security`: `Egress` (https only, resolve on every dial, `net.Dialer.Control` rejects RFC 1918, loopback, link-local, metadata, CGNAT, ::1, fc00::/7, NAT64/6to4 and reserved ranges after resolution; no proxy, no redirects; gRPC passthrough targets and pgx identity lookup so the dialer sees host names), per-plan host allowlist, trusted platform hosts (**D45**)
- [x] Indexer image fetches (`indexer.HTTPFetcher`) go through the egress client, with the platform's own S3 hosts trusted
- [x] SecretStore: AES-256-GCM envelope encryption in MySQL (per-secret data key wrapped by `SECRETS_MASTER_KEY`, bound to tenant and ID) and a Vault KV v2 implementation (**D52**); `POST /secrets`; values travel as `security.Secret`, redacted in logs, `%v` and JSON
- [x] Vector adapters: Qdrant (BYO collection), pgvector (`pgx/v5`, SQL WHERE pushdown, column introspection), Milvus (gRPC API, **D39**); `payload_mapping`; one conformance suite (`storetest`) passes on all three against real stores
- [x] Embedders: KServe v2 (gRPC, REST fallback with the binary tensor extension, **D46**), `openai_embeddings` (client-side L2 normalization)
- [x] Connections CRUD and tester (configure, health, collection, vectors dims/metric, fields, sample query latency); live-connection updates tested before saving, in-use deletes refused (**D48**); model register and validate (probe inference, transport detection); `GET /v1/platform/models`
- [x] Validation rules 3–6 with live probes (**D50**); BYO lexical rejected (**D42**); per-tenant bulkhead semaphores (**D44**); BYO failures degrade to the platform lexical index (**D41**)
- [x] Indexer: BYO embedders and stores, written in place with prune and exact count (**D40**); a BYO tenant's failures cannot stall other tenants (**D49**)
- [x] Chaos tests: failing, hanging and slow BYO embedder and store; tenant isolation (no p95 impact on other tenants); TCP fault proxy `internal/testinfra/faultproxy`
- [x] `control.yaml` schemas for every M5 endpoint, Redocly clean, TS client regenerated; migration 000004; docs (ARCHITECTURE §4/§5/§7/§9, TENANT_CONFIG §4/§5, API, MODELS §1, DATA §6, ONBOARDING §4.1)

Carried forward: BYO scorers and ONNX uploads (M7); the BYO re-ranker (M8); the optional per-tenant egress allowlist for enterprise plans (R3); a startup warm-up for search-api (from M4).

New deps: `jackc/pgx/v5`, `milvus-io/milvus-proto/go-api/v2`, testcontainers postgres and vault modules. Test images: `pgvector/pgvector:0.8.7-pg17-trixie`, `milvusdb/milvus:v2.6.25`, `hashicorp/vault:2.1.2`.

---

## M6 — Events, ClickHouse, features ✅

**Done when:** online item and user features are readable in a request, with freshness ≤ 15 min. **Met: stored features are read by S4, shown in the explain trace and change the ranking; event → Redis feature in under 10 s plus the 10 min schedule (see §0).**

- [x] `POST /v1/events`: strict validation reporting every error (order fields, UUIDs, impression positions, future `ts`, `randomized` on impressions only, tenant only from the token), ≤ 500 per batch, dedup by `event_id` within the batch and in ClickHouse across batches (**D56**), franz-go producer to `search.events.v1` (202 after the Kafka ack, 503 to retry), plan events rate counted per event (burst: one batch); `search.yaml` first
- [x] search-api emits async `impression` events for every served page, image search and similar items (`TryProduce`, bounded buffer, drops counted in `events_dropped_total`; Kafka is not a readiness dependency) (**D55**)
- [x] ClickHouse migration 000003: Kafka engine table plus materialized view into `events`, `randomized` column, broken messages skipped (**D53**, **D62**)
- [x] `tools/dataprep/simulate.py`: 5,000 users (affinity, price band, region), 40,000 sessions, hidden utility, 10% randomized positions marked `randomized`, 2M impressions → `data/events/`, byte-identical per seed (RNG event IDs, gzip `mtime=0`); `cmd/seed -events` / `make seed-events` with timestamps shifted to the present (**D61**)
- [x] `cmd/featurejob` + `internal/featurejob`: propensity η fitted from randomized traffic, position-debiased CTR and CVR (7d/30d) with Bayesian shrinkage, orders, user category affinity and price band → Redis keys and TTLs per **R9**; every live tenant every 10 min (`-once`, `make features`); hourly centroid refresh (**R6**) (**D54**, **D57**, **D59**)
- [x] Vendor data from the tenant: `catalog.vendor_mapping` in product payloads (onboarding catalog step too), `PUT /v1/vendors` (bulk, `catalog:write`) and topic `catalog.vendor.v1` → `f:{tid}:v:{vendor_id}`, partial updates, optional `{min, max}` normalization (**R9**, **D58**)
- [x] `providers/features`: Redis feature store, one pipelined read per request; S4 reads it under half its remaining budget with a breaker and falls back to the declared defaults (**D60**)
- [x] Tests: unit (validation, records, store, S4 merge and fallback, featurejob math, vendor mapping and topic, indexer), search-api acceptance test (features in the trace change the ranking; Redis outage → defaults), integration test Kafka → ClickHouse → featurejob → Redis on real Redpanda and ClickHouse, pytest determinism; docs (API §1, DATA §4–6/§8, MODELS §4, TENANT_CONFIG §3/§5, ARCHITECTURE §1–3/§6, ONBOARDING)

Carried forward: the synthetic user signal is weak (text queries ignore the user's affinities; M8 should bias them before evaluating personalization); the per-tenant ClickHouse retention and a production ClickHouse Kafka broker setting (the migration names the Compose broker, **D62**); exploration slots served by search-api itself (impressions it emits are never `randomized`, so tenants without their own randomized traffic get η = 0 until then); the startup warm-up for search-api (from M4).

New deps: none (`clickhouse-go/v2` moved into featurejob). Test image: `clickhouse/clickhouse-server:26.8.34.13-alpine` (same as Compose).

---

## M7 — Learned ranking and CEL policies ✅

**Done when:** nDCG@10 on the held-out set beats `default_linear`. **Met: `rank-gbdt-v1` nDCG@10 0.3583 vs `default_linear` 0.2708 (+0.0875 gain); `prerank-gbdt-v1` 0.3348 vs 0.2563 (+0.0785 gain) on held-out test split (see §0).**

- [x] `tools/training`: ClickHouse dataset (graded labels order=3, cart=2, click=1), time split 80/10/10, LightGBM lambdarank prerank (≤ 15 features, ≤ 200 trees) and rank → ONNX via onnxmltools plus `.features.json`; metrics report (**D67**–**D69**)
- [x] Feature builder for the catalog in `docs/MODELS.md` §4 (ordered schema, declared defaults) (`internal/pipeline/feature/`)
- [x] `providers/scorer/onnx`: `onnxruntime_go`, one session per `(tenant, version)`, intra-op thread limit; ≤ 5 ms for 2000 candidates (**D71**)
- [x] `providers/scorer/kservev2`: remote rank through Triton's ONNX backend (`rank-gbdt-v1`)
- [x] ONNX upload flow: presign, ≤ 200 MB, sha256, dry-run against the declared schema (`internal/controlplane/scorers.go`) (**D70**)
- [x] `providers/policy`: cel-go restricted env (`item`, `query`), compile at validation, `cel.CostLimit`, ≤ 50 rules / ≤ 2 KB; boosts clamped to [-1, 1] → `policy_boost` (**D63**–**D66**)
- [x] Ranking eval: learned model vs `default_linear` on the held-out split (`tools/eval/reports/2026-10-26_ranking.md`)

Carried forward: exploration traffic slots served by search-api (impressions it emits are never `randomized`); the startup warm-up for search-api (from M4); ads and personalization (M8).

---

## M8 — Personalization, diversity, ads

**Done when:** sponsored slots are honored, and organic ranking is unaffected when ads are disabled (golden test).

- [x] Personalization: `rank + α·affinity + β·price_band_match`; BYO re-ranker (KServe v2) replaces it
- [x] Diversity: greedy `max_per_vendor` per window
- [x] `internal/ads`: campaigns and creatives CRUD, eligibility, relevance floor, quality = relevance + smoothed pCTR, GSP with reserve CPC, probabilistic pacing in Redis, dedup against organic, `sponsored=true` (**R10**)
- [x] Admin ad controls (`platform_settings.ads`, per-plan overrides): tenant-editable campaign fields, sponsored display fields, max slots, minimum floor, reserve bounds (**R10**)
- [x] `data/campaigns.jsonl` (300) seeded; ads reports from ClickHouse
- [x] Golden test: ads disabled gives organic output identical to the pre-ads baseline

Carried forward (R10 parts not built yet): enforcing the admin's `tenant_fields` on campaign reads and edits; the sponsored disclosure (label, vendor name, link) in search responses; `per_plan` overrides (stored, not applied); tenant-tunable `ads.quality` weights; pCTR from position-debiased ad CTR with impression counts (today the item's organic `ctr_30d`, smoothed toward 2%); cosine relevance for image queries; a `campaign_id` on events, so reports need not attribute by product.

---

## M9 — Onboarding wizard ✅

**Done when:** a new tenant goes from sign-up to a live search playground end to end.

- [x] Session as an httpOnly secure cookie; all control-api calls through Route Handlers and Server Actions
- [x] `/onboarding/[step]` with a step guard; Server Components for data, client components for interactive pieces
- [x] Components: `Stepper`, `EngineDiagram` (accessible, mobile-first SVG), `StagePanel`, `ProviderChooser`, `ConnectionForm`, `ModelRegistrationForm`, `JsonPathMapper`, `CelEditor`, `WeightSliders`, `SlotPicker`, `IndexProgress`, `PlaygroundSearch`, `ExplainTrace`
- [x] Server `errors[].path` mapped to fields; focus management; `aria-live` progress
- [x] `/admin` (platform admin): settings, plans, signing keys, ad controls, tenants (**R7**, **R10**)
- [x] Playwright E2E per `docs/ONBOARDING.md` §6 plus an admin settings flow (`npx playwright install chromium` first; `make e2e`)

Carried forward: the playground compares versions and the draft side by side, not "platform defaults" (they have no searchable version); the personalization step does not register a BYO re-ranker (set `rerank.model` through the config API); the admin session lasts one access-token lifetime (no refresh); sign-up sends no Idempotency-Key (Cache Components cannot prerender a client UUID; the button is disabled while pending); the search-api startup warm-up (D38) is still open.

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
