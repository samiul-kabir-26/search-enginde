# PLAN.md — Implementation plan

This plan turns `PROJECT.md` and `docs/*.md` into ordered, checkable work. Milestones follow `PROJECT.md` §6:
finish each one fully (code, tests, lint, docs, and `make up && make seed` working) before starting the next.

**Legend:** `[x]` done · `[ ]` to do · **Q** open question that must be answered before that step starts.

---

## 0. Where we are

**Milestone M0 (scaffold) is done locally.** The only M0 item left is a green CI run, which needs a GitHub remote.

What is already verified on this machine:

| Check | Result |
|---|---|
| `make up` | MySQL, Redis, OpenSearch, Qdrant, Redpanda, ClickHouse and MinIO all report healthy. Kafka topics and S3 buckets are created. |
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
| D3 | MinIO is **built from source** (`deploy/minio/Dockerfile`, tag `RELEASE.2025-10-15T17-29-55Z`) | Upstream archived the repo and removed the `minio/minio` images. It is dev-only and the code uses only the S3 API, so it can be swapped for SeaweedFS or RustFS later. **Q:** keep MinIO or switch? |
| D4 | MySQL is exposed on host port **3307** | A local MySQL already uses 3306. All host ports are configurable in `.env`. |
| D5 | Events topic is named `search.events.v1` | The spec names only `catalog.product.v1`. |
| D6 | Generated code lives in `internal/gen/inference` (KServe protos) and `internal/tenant/tenantdb` (sqlc) | These paths are not in the spec's layout. |
| D7 | Compose profiles: default = infra, plus `init`, `models` (Triton), `obs` (OTel, Jaeger, Prometheus, Grafana) and `apps` (Go services + nginx) | The Triton image is about 15 GB, so it is only needed from M1 (`make up-models`). |
| D8 | OpenSearch 3.9.0, Qdrant 1.20.0, Redpanda 26.2.6, ClickHouse 26.8 LTS, Redis 7.4.11, MySQL 8.4.13, Triton 26.09 | Latest stable releases on the spec's pinned lines. Every image and dependency is pinned exactly. |
| D9 | Python tooling uses `uv` (pinned 0.13.5, binary in `./bin`) with dependency groups; torch comes from the CPU wheel index | No pip or venv bootstrap exists on the host, and this keeps setup reproducible. |
| D10 | `GOTOOLCHAIN=go1.27.3` is pinned in `go.mod` and the Makefile | The system Go is 1.22. golangci-lint must be built with a Go version at least as new as the module's. |

### Open questions (the spec is silent; answer before the listed milestone)

| Q | Needed by | Question |
|---|---|---|
| Q1 | M1 | What is the `catalog.product.v1` envelope (key, headers, JSON shape, delete/tombstone semantics)? |
| Q2 | M1 | Can the ABO archives (listings plus the small-images tarball, several GB) be downloaded to this machine, and where? |
| Q3 | M2 | What are the plan definitions: plan names, max `budgets.total_ms`, rate limits, and per-plan egress allowlist? |
| Q4 | M2 | Facets: which fields, how many values, and computed from which candidate set? What is the deep-paging limit for cursors? |
| Q5 | M2 | How is the JWT issued before `control-api` exists? Proposal: ship the `auth` issuer, JWKS and `POST /v1/auth/token` in M2 as a thin `control-api` slice for the `demo` tenant. |
| Q6 | M3 | Category centroids for S1 kNN: who computes them (indexer or a job), and where are they stored (Redis or MinIO)? |
| Q7 | M4 | Password hashing (proposal: argon2id), email verification delivery in dev (proposal: Mailpit), JWT lifetime and key rotation period. |
| Q8 | M4 | Idempotency-key store (proposal: Redis with a 24 h TTL) and where shadow-mode results are logged (proposal: ClickHouse). |
| Q9 | M6 | Redis key scheme and TTLs for online features. How is `vendor_score` defined? |
| Q10 | M8 | Ad pacing algorithm (proposal: probabilistic throttling against the hourly spend curve) and the GSP quality score definition. |
| Q11 | M1 | Model licenses for `clip-vit-b32` and `bge-small-en-v1.5` must be confirmed before they are offered to tenants (`docs/MODELS.md` §2). |

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
- [ ] **CI green on GitHub.** Needs a remote; push the repo and confirm.

---

## M1 — Data prep, canonical schema, Triton models, indexer, seed

**Done when:** about 10K products are indexed into OpenSearch and Qdrant for tenant `demo`, and the counts are verified.

**Phase 1: data** (`tools/dataprep`, Python)
- [ ] Download the ABO listings, small images and `images.csv.gz` into `data/raw/` (**Q2**). Inspect them, and record the real archive and field names before writing parsers (`docs/DATA.md` §2).
- [ ] `select.py --seed 42`: filters, dedup, stratification (≤400 per type, drop types with <20), 10K target preferring items with alternates → `data/products.raw.jsonl` and `data/images/`
- [ ] `synthesize.py`: 200 Zipf vendors (`vendors.jsonl`), lognormal prices (`price_params.yaml`), moq, stock (5% zero), ship_regions, status, rating ~ hidden q, created_at, listing_completeness → `products.canonical.jsonl`
- [ ] `MANIFEST.json` with counts, seeds and sha256 per file; `make data`; pytest for determinism (same seed gives same sha256)

**Phase 2: canonical schema** (`internal/catalog`)
- [ ] `Product` struct with the filterable-field registry (drives filter validation and pushdown), validation, and the UUIDv5 point ID
- [ ] JSONPath field mapping (choose a library and verify JSONPath support, including `[*]`), plus a mapping-preview function reused in M4

**Phase 3: models** (`tools/models`, `deploy/triton`)
- [ ] `export.py`: CLIP vision and text towers and BGE-small → ONNX with dynamic batch axes; parity cosine ≥ 0.999 on 100 samples
- [ ] Triton repository: Python preprocessing models plus `clip_image`, `clip_text` and `bge_text` ensembles; CPU instance groups; dynamic batching (`max_queue_delay_microseconds` ≈ 2000)
- [ ] `make up-models` healthy; smoke-infer every ensemble; outputs L2-normalized with the expected dims (512 and 384)

**Phase 4: providers and binaries** (Go)
- [ ] `providers/embedder/kservev2`: gRPC client, tensor names per `docs/MODELS.md` §1, batch splitting
- [ ] `providers/lexical/opensearch`: `mapping.json`, create `t_{tenant}_products_v{n}`, alias swap, bulk upsert and delete, health
- [ ] `providers/vectorstore/qdrant`: named vectors `text`/`image`, HNSW m=16 / ef_construct=128, payload indexes, alias swap, upsert and delete
- [ ] `cmd/indexer`: franz-go consumer for `catalog.product.v1` (**Q1**), batching embed + upsert, backfill mode, idempotent re-processing
- [ ] `cmd/seed`: upload images to MinIO, create the `demo` tenant (tenant row, default draft, active v1), publish products as events
- [ ] Integration tests (testcontainers): OpenSearch, Qdrant, Redpanda
- [ ] Verify the count match and a full backfill in ≤ 10 min on CPU Triton

New deps: `twmb/franz-go`, `opensearch-project/opensearch-go/v4`, `qdrant/go-client`, `aws-sdk-go-v2`, `google/uuid`, `oklog/ulid/v2`, `testcontainers-go`.

---

## M2 — Text search: S1–S3, default_linear, explain, JWT, rate limit

**Done when:** text search returns ranked results with a per-stage trace at `?explain=true`, and p95 ≤ 150 ms locally at 50 RPS.

- [ ] `internal/tenant`: typed config sections (`docs/TENANT_CONFIG.md` §3), `DisallowUnknownFields`, `Validate(ctx, deps) []FieldError` (rules that need no live deps), schema-version migration hook, sqlc store, cache, resolver
- [ ] `internal/auth`: EdDSA verify, JWKS fetch/cache/rotate, tenant context from `tid`, scope checks (`search:read`, `search:debug`); token issuance slice (**Q5**)
- [ ] `internal/pipeline`: the consumer-side interfaces (`docs/ARCHITECTURE.md` §3), `Query`/`Hit`/`Candidate`/`Trace` types, per-stage budget `min(stage, remaining)`, RRF (k=60, per-retriever weights), explain trace (top 50), stage metrics
- [ ] `internal/resilience`: `Call(ctx, key, primary, fallback)` with deadline, `sony/gobreaker/v2` keyed by tenant+stage+provider, and `search_stage_fallback_total`
- [ ] `FilterExpr` AST: parse and validate (operators, depth ≤ 3, filterable fields only), translate to OpenSearch `bool.filter` and the Qdrant payload filter. Pushdown is mandatory.
- [ ] S1 text: NFKC, lowercase, whitespace collapse; attribute dictionary (tenant-extensible); category kNN vote (**Q6**); embeddings
- [ ] S2 retrievers: `lexical_bm25` (field boosts), `vector_text`, `vector_text_to_image`, `category_popular`; errgroup fan-out; partial failure tolerated; all failing returns `degraded=true`, never 5xx
- [ ] S3: `always_filters` pushdown (CEL rules come in M7)
- [ ] S4/S5 `providers/scorer/defaultlinear`, plus keep/cut; S6 passthrough
- [ ] `POST /v1/search`: validation, facets and cursor (**Q4**), problem+json errors; per-tenant GCRA rate limit (`go-redis/redis_rate`, **Q3**)
- [ ] Golden tests for the explain trace; unit tests for every stage; coverage ≥ 80% on pipeline, stages and tenant
- [ ] k6 `loadtest/search_text.js` at 50 RPS: p95 ≤ 150 ms

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

- [ ] Auth: sign-up (tenant, owner, default draft, onboarding state), login, users and roles, API keys (shown once, hashed), `POST /v1/auth/token`, EdDSA key management and JWKS (**Q7**)
- [ ] Idempotency-Key middleware (24 h; **Q8**)
- [ ] Config: draft GET/PUT, validate (returns every error), publish (immutable, monotonic version), activate, shadow (0–20%), diff
- [ ] Propagation: Redis pub/sub `tenant.config.activated` → search-api invalidation; 30 s reconcile loop; old-version provider drain
- [ ] `internal/registry`: `(kind, type)` factories, `Deps`, LRU per `(tenant, version, stage)` with close on eviction or supersede
- [ ] Shadow execution on sampled traffic (results logged, never returned)
- [ ] Onboarding: state machine, `PUT /onboarding/steps/{step}` validation per `docs/ONBOARDING.md` §4, engine-explainer endpoints
- [ ] Catalog mapping preview, catalog upload presign, index jobs (build `v{n+1}`, alias swap, progress)
- [ ] Playground search (draft or version, explain on)
- [ ] Fill in `control.yaml` request/response schemas for every endpoint touched; regenerate the TS client
- [ ] Integration test: activate v2 → behavior changes within 5 s

---

## M5 — BYO providers, egress security, secrets, resilience

**Done when:** in a chaos test, a BYO endpoint that fails or hangs falls back to the default within the stage budget.

- [ ] `internal/security`: EgressClient (https only, resolve on every dial, `net.Dialer.Control` rejects RFC 1918, loopback, link-local, metadata, ::1 and fc00::/7), per-plan allowlist
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
- [ ] `cmd/featurejob`: position-debiased CTR/CVR (7d/30d), orders, vendor features, user affinity and price band → Redis (**Q9**); schedule ≤ 15 min
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
- [ ] `internal/ads`: campaigns and creatives CRUD, eligibility, relevance floor, GSP on bid × quality, pacing in Redis (**Q10**), dedup against organic, `sponsored=true`
- [ ] `data/campaigns.jsonl` (300) seeded; ads reports from ClickHouse
- [ ] Golden test: ads disabled gives organic output identical to the pre-ads baseline

---

## M9 — Onboarding wizard

**Done when:** a new tenant goes from sign-up to a live search playground end to end.

- [ ] Session as an httpOnly secure cookie; all control-api calls through Route Handlers and Server Actions
- [ ] `/onboarding/[step]` with a step guard; Server Components for data, client components for interactive pieces
- [ ] Components: `Stepper`, `EngineDiagram` (accessible, mobile-first SVG), `StagePanel`, `ProviderChooser`, `ConnectionForm`, `ModelRegistrationForm`, `JsonPathMapper`, `CelEditor`, `WeightSliders`, `SlotPicker`, `IndexProgress`, `PlaygroundSearch`, `ExplainTrace`
- [ ] Server `errors[].path` mapped to fields; focus management; `aria-live` progress
- [ ] Playwright E2E per `docs/ONBOARDING.md` §6 (`npx playwright install chromium` first)

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
make up && make migrate
make run-search-api # :8080   (make run-control-api → :8081)
make lint test
make up-obs         # + Jaeger :16686, Prometheus :9090, Grafana :3001
make down           # or `make nuke` to drop volumes
```
