# API.md

The OpenAPI 3.1 files in `api/openapi/` are the source of truth. Write them first, from this document.

**Common rules**
- JSON only, except image upload, which uses `multipart/form-data`.
- Errors use RFC 9457 `application/problem+json` with the extension `errors: [{path, code, message}]`.
- Pagination uses opaque `cursor` tokens.
- Idempotency: `POST` endpoints that create resources accept an `Idempotency-Key` header, with keys stored for 24 h.

**Auth**
- `Authorization: Bearer <JWT>` (EdDSA). Required claims: `tid` (tenant), `sub`, `scope`, `exp`.
- `search-api` verifies tokens against `control-api` `/.well-known/jwks.json`, cached and rotated.
- Server-to-server callers exchange a tenant API key for a JWT at `POST /v1/auth/token`.

## 1. search-api

### POST /v1/search  (scope `search:read`)

Request:

```json
{
  "q": "red leather handbag",
  "filters": { "and": [ { "field": "price_minor", "op": "lte", "value": 20000 }, { "field": "ship_regions", "op": "contains", "value": "US" } ] },
  "user": { "id": "u_123", "session_id": "s_abc" },
  "page": { "size": 24, "cursor": null },
  "explain": false
}
```

Response:

```json
{
  "request_id": "01J...",
  "config_version": 7,
  "degraded": false,
  "results": [
    { "product_id": "p_1", "score": 0.83, "sponsored": false, "position": 1, "product": { "title": "...", "price_minor": 15999, "currency": "USD", "image_url": "...", "vendor_id": "v_9" } }
  ],
  "facets": { "category": [ { "value": "handbags", "count": 120 } ], "brand": [] },
  "next_cursor": "eyJ...",
  "trace": null
}
```

Filter operators: `eq`, `ne`, `in`, `nin`, `gt`, `gte`, `lt`, `lte`, `contains`, `range`. Filters combine with `and`/`or` with a maximum nesting depth of 3, and only on fields marked filterable in the canonical schema.

### POST /v1/search/image  (scope `search:read`, `multipart/form-data`)

| Part | Required | Content |
|---|---|---|
| `image` | yes | JPEG, PNG or WebP, ≤ 10 MB |
| `crop` | no | JSON `{x, y, w, h}` in source pixel coordinates |
| `q` | no | Refinement text, fused with RRF |
| `filters`, `user`, `page`, `explain` | no | JSON parts, same as text search |

The response matches `/v1/search`, plus `"predicted_categories": [{ "id": "...", "confidence": 0.72 }]`.

### GET /v1/products/{product_id}/similar?size=24  (scope `search:read`)

Uses the product's stored vectors; no upload required.

### POST /v1/events  (scope `events:write`)

Batch of up to 500 events:

```json
{ "events": [ { "event_id": "uuid", "type": "click", "ts": "2026-10-24T10:00:00Z", "request_id": "01J...", "product_id": "p_1", "position": 3, "user_id": "u_123", "session_id": "s_abc", "sponsored": false } ] }
```

Event types are `impression`, `click`, `add_to_cart`, `order`, `rfq`. `order` requires `order_id`, `quantity` and `value_minor`. Events are deduplicated by `event_id`.

### GET /healthz, GET /readyz, GET /metrics

## 2. control-api

### Auth and sign-up

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/signup` | Creates tenant and owner user, seeds the default draft config and onboarding state; returns a session JWT |
| POST | `/v1/auth/login` | |
| POST | `/v1/auth/token` | API key → JWT |
| GET | `/.well-known/jwks.json` | |
| CRUD | `/v1/tenants/{tid}/api-keys` | Secret shown once on create |
| CRUD | `/v1/tenants/{tid}/users` | Roles: `owner`, `admin`, `developer`, `viewer` |

### Onboarding

| Method | Path | Notes |
|---|---|---|
| GET | `/v1/tenants/{tid}/onboarding` | Current step, completed steps, blockers |
| PUT | `/v1/tenants/{tid}/onboarding/steps/{step}` | Saves the step payload into the draft, runs step validation, returns errors or the next step |
| GET | `/v1/onboarding/engine-explainer` | Static stage descriptions, plus what is configurable per stage and the defaults |
| GET | `/v1/tenants/{tid}/engine-explainer` | Same, overlaid with the tenant's current draft choices (drives the personalized diagram) |

### Config

| Method | Path | Notes |
|---|---|---|
| GET/PUT | `/v1/tenants/{tid}/config/draft` | |
| POST | `/v1/tenants/{tid}/config/draft/validate` | Returns the full error list |
| POST | `/v1/tenants/{tid}/config/versions` | Publish the draft |
| GET | `/v1/tenants/{tid}/config/versions` | |
| GET | `/v1/tenants/{tid}/config/versions/{v}` | |
| POST | `/v1/tenants/{tid}/config/versions/{v}/activate` | |
| POST | `/v1/tenants/{tid}/config/versions/{v}/shadow` | Body `{ "sample_rate": 0.1 }` |
| GET | `/v1/tenants/{tid}/config/diff?from=6&to=7` | |

### Connections and secrets

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/tenants/{tid}/secrets` | Body `{kind, value}` → `{secret_ref}`; the value is never readable again |
| CRUD | `/v1/tenants/{tid}/connections` | Vector store and lexical connections |
| POST | `/v1/tenants/{tid}/connections/{id}/test` | Health check, collection exists, dims and metric match, sample query latency |

### Models

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/tenants/{tid}/models` | Register a remote model (`kserve_v2` or `openai_embeddings`) with declared schemas |
| POST | `/v1/tenants/{tid}/models/uploads` | Returns a presigned S3 PUT for an ONNX artifact; then `POST /models` with `artifact_uri` |
| POST | `/v1/tenants/{tid}/models/{id}/validate` | Dry-run inference with probe inputs; checks shapes, dtypes, latency |
| GET | `/v1/platform/models` | Catalog of platform pre-trained models tenants can select |

### Indexing

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/tenants/{tid}/index/jobs` | Backfill for a config version: builds `v{n+1}` then swaps the alias |
| GET | `/v1/tenants/{tid}/index/jobs/{id}` | Progress |
| POST | `/v1/tenants/{tid}/catalog/uploads` | Presigned upload for JSONL/CSV batch ingestion |
| POST | `/v1/tenants/{tid}/catalog/mapping/preview` | Body: a sample payload plus mapping → canonical product, or errors |

### Ads

| Method | Path | Notes |
|---|---|---|
| CRUD | `/v1/tenants/{tid}/ads/campaigns` | |
| CRUD | `/v1/tenants/{tid}/ads/campaigns/{id}/creatives` | |
| GET | `/v1/tenants/{tid}/ads/reports?from&to` | Impressions, clicks, spend, CTR |

### Playground

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/tenants/{tid}/playground/search` | Runs `search-api` against the draft or a specified version, with `explain=true`, using the caller's control-plane session |
