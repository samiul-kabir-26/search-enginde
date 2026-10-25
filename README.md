# Search Engine

Multi-tenant search engine microservice (Go) for a multi-vendor e-commerce platform: text, image and
similar-item search through a pluggable six-stage pipeline, with per-tenant BYO providers.

- **Spec:** [`PROJECT.md`](PROJECT.md) and [`docs/`](docs/)
- **Plan and status:** [`PLAN.md`](PLAN.md)

## Prerequisites

Docker with Compose v2, GNU Make, Go (any version ≥ 1.21; the pinned 1.27.3 toolchain downloads
automatically), Node 24, Python 3.12, and `curl`.

## Quick start

```bash
make setup            # pinned Go tools → ./bin, tools/.venv, web deps, ONNX Runtime
make up               # infra: MySQL, Redis, OpenSearch, Qdrant, Redpanda, ClickHouse, SeaweedFS (S3)
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
| Triton (`make up-models`) | 8000, 8001, 8002 |
| Indexer (`make run-indexer`) | 8082 (health, metrics) |
| Jaeger / Prometheus / Grafana (`make up-obs`) | 16686 / 9090 / 3001 |

Ports and credentials live in `.env` (created from `.env.example`). Credentials are dev-only.

## Layout

`cmd/` binaries · `internal/` packages · `api/openapi/` REST contracts (source of truth) ·
`proto/kserve/` vendored inference protos · `db/` migrations and queries · `deploy/` Compose and images ·
`tools/` Python data, models, training, eval · `web/onboarding/` Next.js wizard · `loadtest/` k6.
