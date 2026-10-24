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
make up               # infra: MySQL, Redis, OpenSearch, Qdrant, Redpanda, ClickHouse, MinIO
make migrate
make run-search-api   # http://localhost:8080/healthz
make run-control-api  # http://localhost:8081/healthz
make help             # all targets
```

| Service | Host port |
|---|---|
| MySQL | 3307 |
| Redis | 6379 |
| OpenSearch | 9200 |
| Qdrant | 6333 (HTTP), 6334 (gRPC) |
| Redpanda (Kafka) | 19092 |
| ClickHouse | 8123 (HTTP), 9000 (native) |
| MinIO | 9100 (S3), 9101 (console) |
| Triton (`make up-models`) | 8000, 8001, 8002 |
| Jaeger / Prometheus / Grafana (`make up-obs`) | 16686 / 9090 / 3001 |

Ports and credentials live in `.env` (created from `.env.example`). Credentials are dev-only.

## Layout

`cmd/` binaries · `internal/` packages · `api/openapi/` REST contracts (source of truth) ·
`proto/kserve/` vendored inference protos · `db/` migrations and queries · `deploy/` Compose and images ·
`tools/` Python data, models, training, eval · `web/onboarding/` Next.js wizard · `loadtest/` k6.
