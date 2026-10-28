# Search Engine

Multi-tenant search engine microservice (Go) for a multi-vendor e-commerce platform: text, image and
similar-item search through a pluggable six-stage pipeline, with per-tenant BYO providers.

- **How it works:** [`docs/HOW_IT_WORKS.md`](docs/HOW_IT_WORKS.md) (overview with diagrams)
- **Spec:** [`PROJECT.md`](PROJECT.md) and [`docs/`](docs/)
- **Plan and status:** [`PLAN.md`](PLAN.md)

## Tech stack

| Area | Technology |
|---|---|
| Services | Go 1.27 (`search-api`, `control-api`, `indexer`, `featurejob`, `seed`), REST (OpenAPI) + gRPC |
| Lexical search | OpenSearch 3.9 |
| Vector search | Qdrant 1.20 (default); BYO pgvector and Milvus adapters |
| Relational store | MySQL 8.4 (migrations + sqlc) |
| Cache, online features, pub/sub | Redis 7.4 |
| Event streaming | Redpanda (Kafka API, franz-go client) |
| Analytics and events | ClickHouse 26.8 |
| Object storage | SeaweedFS (S3 API) |
| Model serving | NVIDIA Triton 26.09 (KServe v2 / Open Inference Protocol, ONNX + Python backends); ONNX Runtime in-process |
| Ranking rules | CEL (policy boosts and filters) |
| Secrets | HashiCorp Vault KV v2 (BYO provider secrets) |
| ML tooling | Python 3.12 (uv): PyTorch, Transformers, ONNX, LightGBM, scikit-learn, onnxmltools |
| Onboarding UI | Next.js 16, React 19, TypeScript, Tailwind CSS 4, Playwright |
| Observability | OpenTelemetry, Jaeger, Prometheus, Grafana |
| Edge, email, load tests | nginx, Mailpit (dev SMTP), k6 |
| Tooling | Docker Compose, Make, buf, sqlc, golangci-lint, Redocly, testcontainers |

## Models

Platform defaults (details in [`docs/MODELS.md`](docs/MODELS.md)); tenants can bring their own via KServe v2
or OpenAI-compatible embedding endpoints, or upload ONNX scorers.

| ID | Model | Dim | Use |
|---|---|---|---|
| `clip-vit-b32` | [`laion/CLIP-ViT-B-32-laion2B-s34B-b79K`](https://huggingface.co/laion/CLIP-ViT-B-32-laion2B-s34B-b79K) | 512 | Image embeddings, text→image via the text tower |
| `bge-small-en-v1.5` | [`BAAI/bge-small-en-v1.5`](https://huggingface.co/BAAI/bge-small-en-v1.5) | 384 | Text semantic retrieval |
| `prerank-gbdt-v1` | LightGBM lambdarank → ONNX (`make train`) | n/a | Pre-rank, in-process ONNX Runtime |
| `rank-gbdt-v1` | LightGBM lambdarank → ONNX (`make train`) | n/a | Rank, served by Triton |
| `default_linear` | Weighted linear scorer | n/a | Baseline ranker before learned models are trained |

## Prerequisites

Docker with Compose v2, GNU Make, Go (any version ≥ 1.21; the pinned 1.27.3 toolchain downloads
automatically), Node 24, Python 3.12, `curl` and `jq`.

## Quick start

```bash
make setup            # pinned Go tools → ./bin, tools/.venv, web deps, ONNX Runtime
make up               # infra: MySQL, Redis, OpenSearch, Qdrant, Redpanda, ClickHouse, SeaweedFS (S3), Mailpit
make migrate
make run-search-api   # http://localhost:8080/healthz
make run-control-api  # http://localhost:8081/healthz
make help             # all targets
```

### Demo catalog (M1)

```bash
make up-models        # infra + Triton (CPU); the image is large (~25 GB)
make data             # ABO subset: 10K products, synthetic vendors/prices, ~11K images (deterministic)
make models           # export CLIP + BGE to ONNX into deploy/triton/model_repository
make up-models        # restart so Triton loads the exported models; then `make models-smoke`
make seed             # demo tenant + images in S3 + catalog.product.v1 events
make run-indexer      # embed and index into OpenSearch and Qdrant (~7 min on CPU)
make backfill         # optional: rebuild the demo index from the topic and verify counts
```

### Text search (M2)

```bash
make run-control-api  # JWKS + POST /v1/auth/token on :8081
make run-search-api   # POST /v1/search on :8080
TOKEN=$(make -s token)   # exchanges the demo API key in data/demo_api_key for a JWT
curl -s localhost:8080/v1/search -H "Authorization: Bearer $TOKEN" \
  -d '{"q":"red leather handbag under $100","page":{"size":5},"explain":true}'
make loadtest         # k6 at 50 RPS (target p95 <= 150 ms)
```

### Image search and similar items (M3)

```bash
curl -s localhost:8080/v1/search/image -H "Authorization: Bearer $TOKEN" \
  -F image=@photo.jpg -F 'page={"size":10}' -F q=leather
curl -s "localhost:8080/v1/products/abo:B000FTHD9W/similar?size=8" -H "Authorization: Bearer $TOKEN"
make eval             # image eval: recall@10/50, MRR -> tools/eval/reports/
```

### Control plane: sign-up, config versions, activation (M4)

```bash
make seed             # also creates the platform admin from PLATFORM_ADMIN_EMAIL / PLATFORM_ADMIN_PASSWORD
curl -s -c jar localhost:8081/v1/signup -H 'Idempotency-Key: k1' -d '{"company_name":"Acme","slug":"acme",
  "region":"local","plan":"growth","email":"owner@acme.test","password":"correct horse battery staple"}'
open http://localhost:8025   # Mailpit: the verification link (POST its token to /v1/auth/verify-email)
ADMIN=$(curl -s localhost:8081/v1/admin/login -d '{"email":"admin@example.com","password":"change-me-admin-password"}' | jq -r .access_token)
T=$(curl -s localhost:8081/v1/admin/tenants -H "Authorization: Bearer $ADMIN" | jq -r '.items[]|select(.slug=="demo").id')
curl -s localhost:8081/v1/tenants/$T/config/draft -H "Authorization: Bearer $ADMIN"            # edit, then PUT it back
curl -s -X POST localhost:8081/v1/tenants/$T/config/versions -H "Authorization: Bearer $ADMIN"   # publish → {"version":N}
curl -s -X POST localhost:8081/v1/tenants/$T/config/versions/N/activate -H "Authorization: Bearer $ADMIN"
```

search-api picks up an activation within milliseconds (Redis pub/sub, plus a 30 s reconcile), with no restart.
`POST /v1/tenants/{tid}/playground/search` with `"config_version":"draft"` tries a draft first, and
`.../config/versions/N/shadow` with `{"sample_rate":0.1}` logs the comparisons to ClickHouse `shadow_results`.
`docs/API.md` §2 lists every endpoint.

Product data comes from [Amazon Berkeley Objects](https://amazon-berkeley-objects.s3.amazonaws.com/index.html)
(CC BY 4.0, credit Amazon.com); see `tools/dataprep/ATTRIBUTION.md`.

| Service | Host port |
|---|---|
| MySQL | 3307 |
| Redis | 6379 |
| OpenSearch | 9200 |
| Qdrant | 6333 (HTTP), 6334 (gRPC) |
| Redpanda (Kafka) | 19092 |
| ClickHouse | 8123 (HTTP), 9000 (native) |
| SeaweedFS S3 | 9100 |
| Mailpit (SMTP / UI) | 1025 / 8025 |
| Triton (`make up-models`) | 8000, 8001, 8002 |
| Indexer (`make run-indexer`) | 8082 (health, metrics) |
| Jaeger / Prometheus / Grafana (`make up-obs`) | 16686 / 9090 / 3001 |

Ports and credentials live in `.env` (created from `.env.example`). Credentials are dev-only.

## Layout

`cmd/` binaries · `internal/` packages · `api/openapi/` REST contracts (source of truth) ·
`proto/kserve/` vendored inference protos · `db/` migrations and queries · `deploy/` Compose and images ·
`tools/` Python data, models, training, eval · `web/onboarding/` Next.js wizard · `loadtest/` k6.
