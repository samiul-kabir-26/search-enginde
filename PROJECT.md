# PROJECT.md — Multi-Tenant Search Engine Microservice (Go)

## 1. Mission

Build a multi-tenant search engine microservice in Go for a multi-vendor e-commerce platform. It must support text search, image search and similar-item search through a staged pipeline:

1. Query understanding
2. Retrieval
3. Filter and policy
4. Pre-rank
5. Rank
6. Re-rank and ads blend

Every stage is pluggable per tenant. A tenant uses either a platform default provider or its own (BYO) provider: its own vector DB, embedding models, ranking models, lightweight ONNX scorer, policy rules, personalization and sponsored listings.

Tenants self-onboard through a guided wizard that explains how the engine works and walks them through configuration (see `docs/ONBOARDING.md`).

## 2. Read these before writing code

| File | Purpose |
|---|---|
| `docs/ARCHITECTURE.md` | Services, pipeline, interfaces, tenancy, resilience, security |
| `docs/TENANT_CONFIG.md` | Tenant pipeline config schema, validation rules, versioning |
| `docs/API.md` | REST contracts for `search-api` and `control-api` |
| `docs/DATA.md` | Dataset (~10K products), synthetic data, embeddings, indexes, eval sets |
| `docs/MODELS.md` | Inference contracts, default models, Triton setup, training |
| `docs/ONBOARDING.md` | Tenant sign-up wizard, engine explainer diagram, step validation |

When a spec is ambiguous, stop and ask; do not invent behavior. When a library or API capability is uncertain, verify it against upstream docs or source before using it.

## 3. Tech stack (pinned decisions)

| Concern | Choice |
|---|---|
| Language | Go 1.25+ (use the latest stable release at implementation time) |
| HTTP | `net/http` with stdlib `ServeMux` method and path patterns; no web framework |
| Internal RPC to models | gRPC, KServe v2 / Open Inference Protocol (generated from the official protos) |
| Control-plane DB | MySQL 8.4 LTS, with `go-sql-driver/mysql`, `sqlc` and `golang-migrate` |
| Lexical index | OpenSearch (BM25), with `opensearch-project/opensearch-go` |
| Default vector DB | Qdrant, with `qdrant/go-client` |
| BYO vector adapters (v1) | Qdrant, pgvector (`jackc/pgx/v5`), Milvus (official Go client v2) |
| Model serving (defaults) | NVIDIA Triton Inference Server (CPU in dev), ONNX models plus a Python-backend ensemble for preprocessing |
| In-process scorer | ONNX Runtime via `yalue/onnxruntime_go` (cgo) |
| Policy rules | `google/cel-go`, with cost limits |
| Events | Kafka API (Redpanda in dev), `twmb/franz-go` |
| Event analytics | ClickHouse (Kafka engine plus materialized views) |
| Online features, cache, rate limit | Redis 7, `redis/go-redis/v9`, `go-redis/redis_rate` |
| Circuit breaker | `sony/gobreaker/v2` |
| Concurrency | `golang.org/x/sync/errgroup` |
| Image decoding | stdlib `image/jpeg`, `image/png`, plus `golang.org/x/image/webp` |
| Object storage | S3 API (SeaweedFS in dev), `aws-sdk-go-v2` |
| Secrets | `SecretStore` interface: Vault in prod; AES-256-GCM envelope encryption in MySQL for dev |
| Auth | EdDSA JWT issued by `control-api`; JWKS consumed by `search-api` |
| Observability | OpenTelemetry (traces and metrics), `log/slog` JSON logs, Prometheus exporter |
| Testing | stdlib `testing`, table-driven tests, `testcontainers-go` for integration |
| Lint | `golangci-lint` (config committed) |
| Load test | k6 |
| Data and training tooling | Python 3.12 under `tools/` (dataprep, ONNX export, LightGBM training) |
| Onboarding UI | Next.js (App Router), TypeScript, Tailwind CSS, under `web/onboarding` |
| Local orchestration | Docker Compose |

Pin every dependency and container image to an exact version. Do not use `latest` tags.

## 4. Repository layout

```
search-engine/
├── cmd/
│   ├── search-api/        # data plane: search, image search, similar, events ingest
│   ├── control-api/       # control plane: tenants, config, models, connections, onboarding, ads
│   ├── indexer/           # consumes catalog events, embeds, upserts to lexical and vector stores
│   ├── featurejob/        # ClickHouse -> Redis online features (scheduled)
│   └── seed/              # loads the prepared dataset as catalog events plus synthetic tenants
├── internal/
│   ├── platform/          # config loading, slog, otel, httpx helpers, problem+json errors
│   ├── auth/              # JWT issue/verify, JWKS, tenant context
│   ├── tenant/            # config model, MySQL store, cache, resolver, validation, versioning
│   ├── pipeline/          # engine, stage interfaces (consumer-side), trace/explain, budgets
│   ├── stages/            # queryunderstanding, retrieval, filter, prerank, rank, rerank
│   ├── providers/
│   │   ├── embedder/      # kservev2, openai_embeddings
│   │   ├── vectorstore/   # qdrant, pgvector, milvus
│   │   ├── lexical/       # opensearch
│   │   ├── scorer/        # onnx (in-process), kservev2 (remote), default_linear
│   │   ├── policy/        # cel
│   │   └── features/      # redis online feature reader
│   ├── registry/          # provider factories keyed by type+version
│   ├── resilience/        # breaker, per-call deadline, fallback wrapper
│   ├── security/          # egress allowlist, SSRF-safe dialer, SecretStore impls
│   ├── imaging/           # upload validation, decode limits, EXIF strip, re-encode
│   ├── ads/               # campaigns, auction (GSP), budget pacing, blender
│   ├── events/            # event schema, producer, validation
│   ├── catalog/           # canonical product schema, mapping, catalog event consumer
│   └── controlplane/      # handlers, onboarding state machine, connection testers, index jobs
├── api/openapi/           # search.yaml, control.yaml (source of truth for REST)
├── proto/kserve/          # vendored Open Inference Protocol protos
├── db/migrations/{mysql,clickhouse}/
├── deploy/
│   ├── docker-compose.yml
│   ├── triton/model_repository/
│   └── nginx/
├── tools/
│   ├── dataprep/          # ABO subset selection, normalization, synthetic data generation
│   ├── models/            # ONNX export scripts, Triton ensemble assets
│   ├── training/          # LightGBM pre-rank and rank training -> ONNX
│   └── eval/              # offline eval harness (recall@k, nDCG@k, MRR)
├── web/onboarding/        # Next.js wizard
├── loadtest/              # k6 scripts
└── docs/
```

## 5. Engineering conventions

- `context.Context` is the first parameter on every I/O path, and deadlines propagate. No package-level mutable state.
- Interfaces are defined in the consuming package (`internal/pipeline`) and implemented under `internal/providers`.
- Errors are wrapped with `%w`, and HTTP errors are returned as RFC 9457 `application/problem+json`.
- Every tenant-scoped query goes through code that requires a `tenant.ID`. Never accept `tenant_id` from the request body on the data plane; it comes from the JWT.
- Every outbound call to a tenant-registered endpoint goes through `security.EgressClient` (allowlist plus SSRF-safe dialer) and `resilience.Call` (deadline, breaker, fallback).
- Config is twelve-factor: env vars only, validated at startup, fail fast.
- Tests: unit tests for every stage and provider; integration tests with testcontainers for MySQL, Redis, OpenSearch, Qdrant and Redpanda; golden tests for the explain/trace output.
- Coverage target for `internal/pipeline`, `internal/stages`, `internal/tenant` and `internal/security` is at least 80%.
- Write no code comments except where intent is non-obvious. Exported identifiers get doc comments.
- `make` targets: `up`, `down`, `migrate`, `gen`, `lint`, `test`, `test-integration`, `seed`, `eval`, `loadtest`.

## 6. Milestones

Complete each milestone fully before starting the next one: code, tests and lint passing, docs updated, and `make up && make seed` working.

| # | Milestone | Done when |
|---|---|---|
| M0 | Scaffold, Compose stack, Makefile, CI (lint plus tests), OpenAPI skeletons, migrations framework | `make up` brings up all infra healthy; CI is green |
| M1 | Data prep (`tools/dataprep`), canonical product schema, Triton default models, `indexer`, `seed` | About 10K products indexed into OpenSearch and Qdrant for tenant `demo`; counts verified |
| M2 | `search-api` text search: stages 1–3, `default_linear` ranker, explain trace, JWT auth, per-tenant rate limit | Text search returns ranked results with `?explain=true` per-stage trace; p95 ≤ 150 ms locally at 50 RPS |
| M3 | Image search and similar-items: upload validation, CLIP embedding, kNN category vote, text+image RRF fusion | Image eval recall@10 is reported (see `docs/DATA.md` §7); p95 ≤ 250 ms excluding upload |
| M4 | `control-api`: tenant sign-up, config draft/validate/publish/activate, config propagation to `search-api` | Activating a new version changes search behavior within 5 s without restart |
| M5 | BYO providers: vector adapters, KServe v2 and OpenAI-embeddings adapters, connection testers, egress allowlist, SecretStore, breakers and fallbacks | Chaos test: a BYO endpoint that fails or hangs falls back to the default within the stage budget |
| M6 | Events: ingest API, Kafka → ClickHouse, synthetic event generator, `featurejob` → Redis | Online item/user features readable in a request; freshness ≤ 15 min |
| M7 | Learned ranking: LightGBM pre-rank and rank trained on synthetic events, exported to ONNX; in-process ONNX scorer; remote rank via Triton; CEL policies | nDCG@10 on the held-out set beats `default_linear` |
| M8 | Personalization, vendor diversity, ads (campaigns, GSP auction, pacing, blending with relevance floor) | Sponsored slots honored; organic ranking unaffected when ads are disabled (golden test) |
| M9 | Onboarding wizard (`web/onboarding`) against `control-api` | A new tenant goes from sign-up to a live search playground end-to-end |
| M10 | Eval harness, k6 load tests, OTel dashboards, runbook | Reports committed under `tools/eval/reports/` and `loadtest/reports/` |

## 7. Non-functional targets

| Metric | Target |
|---|---|
| Text search p95 | ≤ 150 ms (≤ 300 ms with BYO remote rank) |
| Image search p95 | ≤ 250 ms (excluding upload transfer) |
| Throughput per `search-api` replica | ≥ 200 RPS on 2 vCPU with defaults |
| Availability | Single-stage failure never fails the request; stages degrade to their fallback |
| Isolation | One tenant's failing or slow provider has no measurable impact on other tenants' p95 |
| Config propagation | ≤ 5 s |

## 8. Out of scope (v1)

- Multilingual analyzers. v1 is English, but the analyzer must be configurable per tenant index.
- Object detection or auto-crop for image queries. v1 accepts an optional client crop box.
- Running tenant code in-process. Tenants never upload executable code; ONNX artifacts are the only uploaded models.
