# API.md

The OpenAPI 3.1 files in `api/openapi/` are the source of truth. Write them first, from this document.

**Common rules**
- JSON only, except image upload, which uses `multipart/form-data`.
- Errors use RFC 9457 `application/problem+json` with the extension `errors: [{path, code, message}]`.
- Search pagination: `page.cursor` (opaque), `page.number` or `page.offset`, plus `page.size` (1–100, default 24); at most one mode per request. All three read one cached result snapshot (§1).
- Idempotency: `POST` endpoints that create resources (sign-up, users, API keys, config publish, catalog uploads, index jobs) accept an `Idempotency-Key` header. The first response is stored in Redis for 24 h under `idem:{tenant}:{key}` (`idem:{signup}:{key}` for sign-up) and replayed for a retry with the same body. The same key with a different body returns 422; a retry while the first request is still running returns 409.

**Auth**
- `Authorization: Bearer <JWT>` (EdDSA). Required claims: `tid` (tenant), `sub`, `scope`, `exp`. Only platform-admin tokens (`platform:admin`) have no `tid`; the data plane always requires one.
- `search-api` verifies tokens against `control-api` `/.well-known/jwks.json`, cached and refreshed on an unknown `kid`.
- Server-to-server callers exchange a tenant API key for a JWT at `POST /v1/auth/token` (TTL: the `auth.api_token_ttl` platform setting, default 15 min).
- Console sessions: a short access token (default 15 min) in the response body, plus a rotating refresh token in the `se_refresh` cookie (`HttpOnly`, `SameSite=Strict`, `Path=/v1/`; `Secure` outside dev). `POST /v1/auth/refresh` rotates it. Presenting a refresh token that was already rotated revokes its whole family.
- Tenant roles map to scopes: `owner` and `admin` → `tenant:read tenant:write tenant:admin`; `developer` → `tenant:read tenant:write`; `viewer` → `tenant:read`. Reads need `tenant:read`, edits and publishing need `tenant:write`, and activation, shadow, users and API keys need `tenant:admin`.
- A token for another tenant gets 404 on `/v1/tenants/{tid}/...`, so tenant IDs cannot be probed.
- Signing keys live in MySQL with the private half sealed (AES-256-GCM, `SECRETS_MASTER_KEY`). They rotate on the `auth.key_rotation` schedule (default 30 days), and retired keys stay in the JWKS for `auth.key_retention`.

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

Filter operators: `eq`, `ne`, `in`, `nin`, `gt`, `gte`, `lt`, `lte`, `contains`, `range`. Filters combine with `and`/`or` with a maximum nesting depth of 3, and only on fields marked filterable in the canonical schema (or `custom.{name}` fields marked filterable). `range` takes `{gt, gte, lt, lte}`; `contains` applies to multi-valued fields such as `ship_regions`.

**Query understanding.** Price phrases in `q` ("under $50", "between 20 and 40 dollars", "over 100", but not "under 5 lbs") become a `price_minor` filter and are removed from the matched text. The response's `query` object echoes the normalized text, extracted attributes and predicted categories.

**Pagination and snapshots.** The first request runs the pipeline and caches the ordered result list (up to the plan's page depth) for 10 minutes, keyed by tenant, config version, normalized query, filters and user. Every paging mode reads that snapshot, so pages never overlap or shift. The response carries `offset`, `total` (results available to page through), `total_pages`, `total_matches` (lexical matches with every filter applied, counted up to 10,000) and `next_cursor` (null on the last page). Cursors are HMAC-signed and bound to the tenant and query; a tampered, foreign or expired cursor returns 400 with error code `invalid_cursor`.

**Facets.** Counts are computed over the filtered lexical matches and are disjunctive: a facet ignores the request conditions on its own field. Facets are configured per tenant (`facets` in the config, defaulting to `category_ids`, `brand`, `price_minor` ranges and `ship_regions`); numeric facets return range buckets with `value` `"from-to"` plus `from`/`to`.

**Errors and limits.** 400 for validation errors (every error reported, with JSON Pointer paths), 401 for a missing or invalid token, 403 for a missing scope or an inactive tenant, 429 with `Retry-After` when the plan's rate limit (GCRA, per tenant) is exceeded, 503 when tenant config or providers are unavailable. Unknown request fields are rejected. A response with `degraded: true` is still 200: some providers failed and results may be incomplete.

### POST /v1/search/image  (scope `search:read`, `multipart/form-data`)

| Part | Required | Content |
|---|---|---|
| `image` | yes | JPEG, PNG or WebP, ≤ 10 MB |
| `crop` | no | JSON `{x, y, w, h}` in source pixel coordinates |
| `q` | no | Refinement text, fused with RRF |
| `filters`, `user`, `page`, `explain` | no | JSON parts, same as text search |

The response matches `/v1/search`, plus `"predicted_categories": [{ "id": "...", "confidence": 0.72 }]` (a similarity-weighted vote over the categories of the 50 nearest image neighbours).

- The image is accepted by magic bytes only; its header must report at most 4096×4096 before it is decoded; the crop is applied; it is scaled to a 336 px short side and re-encoded as JPEG, which drops EXIF and other metadata. 413 for more than 10 MB, 415 for other formats, 400 (path `/image` or `/crop`) for undecodable files and crops outside the image.
- JSON parts may be plain form fields or file parts. Paging and cursors work as in text search; the snapshot is keyed by the image content, crop, text, filters and user.
- Without `q` only `vector_image` runs and no facets are returned; with `q` the text retrievers join the RRF fusion and facets are computed from the text matches.

### GET /v1/products/{product_id}/similar?size=24  (scope `search:read`)

Uses the product's stored vectors; no upload required. Runs `vector_image` and `vector_text` with the tenant's always_filters, excludes the product, ranks with the tenant's scorers and returns at most `size` (1–100, default 24) results without paging. 404 when the product is not indexed.

### POST /v1/events  (scope `events:write`)

Batch of up to 500 events:

```json
{ "events": [ { "event_id": "uuid", "type": "click", "ts": "2026-10-24T10:00:00Z", "request_id": "01J...", "product_id": "p_1", "position": 3, "user_id": "u_123", "session_id": "s_abc", "sponsored": false } ] }
```

Event types are `impression`, `click`, `add_to_cart`, `order`, `rfq`. `order` requires `order_id`, `quantity` and `value_minor`.

- **Validation.** The whole batch is checked and every error is reported (400 with `errors[].path`, e.g. `/events/3/order_id`); nothing is written unless every event is valid. `event_id` is a UUID; `ts` may be at most 5 minutes in the future; `position` is within 1–10,000 and required for impressions; IDs are at most 256 bytes; `randomized` (impressions only, see below) is optional. Unknown fields, including `tenant_id`, are rejected: the tenant always comes from the token.
- **Delivery.** 202 `{accepted}` means the events are acknowledged by Kafka (`search.events.v1`, key `{tenant_id}:{event_id}`). Kafka unavailable → 503; retry the same batch. The plan's events rate counts events, not requests, with a burst of one full batch (429 with `Retry-After`).
- **Deduplication.** Repeated `event_id`s are dropped within a batch, and ClickHouse collapses repeats across batches (`ReplacingMergeTree`, read with `FINAL`), so retrying a batch is safe.
- **Impressions.** search-api emits an `impression` for every result it returns (search, image search, similar items, every page), with the response's `request_id`, the result position and the request's user. Clients send `click`, `add_to_cart`, `order` and `rfq` events that reference that `request_id`, and impressions only for results not served by this API. Emitted impressions never slow a search: they are queued without waiting and dropped (counted in `events_dropped_total`) when Kafka is unavailable.
- **`randomized`.** Marks an impression shown in a randomized order (an exploration slot). featurejob estimates the examination probability per position from these requests to position-debias CTR (`docs/MODELS.md` §4).

ClickHouse ingests the topic through a Kafka engine table and a materialized view within seconds; featurejob turns the events into online features at least every 15 minutes.

### PUT /v1/vendors  (scope `catalog:write`)

Bulk update of vendor features (PLAN.md R9), up to 500 vendors:

```json
{ "vendors": [ { "vendor_id": "v_9", "score": 0.82, "on_time_dispatch_rate": 0.97, "dispute_rate": 0.01, "response_hours": 6, "verified": true } ] }
```

Updates are partial: only the fields sent are stored and the others keep their stored value (or the declared default). `score` and the rates are within [0, 1]; `response_hours` is raw hours (ranked as `min(hours/48, 1)`). The tenant computes these; they are stored in Redis only and take effect on the next search, with no reindex. Vendor data can also arrive in product payloads (`catalog.vendor_mapping`, `docs/TENANT_CONFIG.md` §3) or on the `catalog.vendor.v1` topic. 200 `{updated}`; Redis unavailable → 503; the plan's events rate also limits vendor updates (counted per vendor).

### GET /healthz, GET /readyz, GET /metrics

## 2. control-api

### Auth and sign-up

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/signup` | Creates tenant and owner user, seeds the default draft config and onboarding state, emails a verification link; returns a session (201) and sets the refresh cookie |
| POST | `/v1/auth/login` | Body `{tenant, email, password}` (tenant = slug); argon2id; same error for every wrong field |
| POST | `/v1/auth/refresh` | Rotates the `se_refresh` cookie, returns a new access token |
| POST | `/v1/auth/logout` | Revokes the refresh family, clears the cookie (204) |
| POST | `/v1/auth/verify-email` | Body `{token}` from the emailed link (204); single use, expires after `auth.email_verification_ttl` (default 24 h) |
| POST | `/v1/auth/resend-verification` | Emails a new link to the caller (202) |
| POST | `/v1/auth/token` | API key → JWT |
| GET | `/.well-known/jwks.json` | Active and retired, unrevoked keys |
| GET/POST/DELETE | `/v1/tenants/{tid}/api-keys` | Body `{scopes}` (default `search:read events:write`; allowed: `search:read`, `search:debug`, `events:write`, `catalog:write`). Returns `{api_key, key}`; the secret is shown once |
| GET/POST/PATCH/DELETE | `/v1/tenants/{tid}/users` | Roles: `owner`, `admin`, `developer`, `viewer`. Only an owner can create or promote an owner, and the last owner cannot be removed or demoted |

Activation (config activate and the onboarding go-live step) requires a verified email (`email_unverified`, 403). Platform admins are exempt.

### Platform admin (scope `platform:admin`)

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/admin/login` | Body `{email, password}`; the first admin is created by `make seed` from `PLATFORM_ADMIN_EMAIL` / `PLATFORM_ADMIN_PASSWORD` |
| GET/PUT | `/v1/admin/settings` | Platform settings (`auth`: argon2id parameters, password minimum length, token TTLs, key rotation and retention, verification TTL), validated against the R7 bounds; every replica reloads via Redis `platform.settings.changed` |
| GET | `/v1/admin/plans` | |
| PUT | `/v1/admin/plans/{plan_id}` | Create or replace a plan (budgets, rate limits, BYO, egress allowlist, custom fields, page depth) |
| GET | `/v1/admin/keys` | Signing keys with status `active`, `retired` or `revoked` |
| POST | `/v1/admin/keys/rotate` | Returns `{active_kid}`; the old key keeps verifying until its retention ends |
| POST | `/v1/admin/keys/{kid}/revoke` | Tokens signed by the key stop verifying at once; revoking the active key rotates first |
| GET | `/v1/admin/tenants?limit&offset` | |

Platform admins can also call every `/v1/tenants/{tid}/...` endpoint.

### Onboarding

| Method | Path | Notes |
|---|---|---|
| GET | `/v1/tenants/{tid}/onboarding` | Current step, completed steps, all steps in order |
| PUT | `/v1/tenants/{tid}/onboarding/steps/{step}` | Saves the step payload into the draft and runs step validation: 422 with field errors (nothing saved), or `{step, next_step, warnings, data, state}`. Steps unlock in order (409 otherwise) and can be revisited. `?use_defaults=true` applies platform defaults to retrieval through personalization. Payloads: `docs/ONBOARDING.md` §5 |
| GET | `/v1/onboarding/engine-explainer` | Static stage descriptions, plus what is configurable per stage and the defaults |
| GET | `/v1/tenants/{tid}/engine-explainer` | Same, overlaid with the tenant's current draft choices (drives the personalized diagram) |

### Config

| Method | Path | Notes |
|---|---|---|
| GET/PUT | `/v1/tenants/{tid}/config/draft` | PUT stores the document even when invalid (so editing never loses work) and returns it with its validation |
| POST | `/v1/tenants/{tid}/config/draft/validate` | Returns the full error list |
| POST | `/v1/tenants/{tid}/config/versions` | Publishes the draft as the next version (201). Invalid drafts return 422. Versions are immutable and numbered monotonically per tenant |
| GET | `/v1/tenants/{tid}/config/versions` | Summaries with `state`: `published`, `active`, `shadow` or `retired` |
| GET | `/v1/tenants/{tid}/config/versions/{v}` | Includes the document |
| POST | `/v1/tenants/{tid}/config/versions/{v}/activate` | Retires the previous active version and publishes `tenant.config.activated`; search-api serves it within seconds (`docs/ARCHITECTURE.md` §8). Rolling back = activating an older version |
| POST | `/v1/tenants/{tid}/config/versions/{v}/shadow` | Body `{ "sample_rate": 0.1 }` (0–0.2; 0 clears shadow). One shadow version per tenant |
| GET | `/v1/tenants/{tid}/config/diff?from=6&to=7` | `{from, to, changes: [{op: add\|remove\|replace, path, from, to}]}`, paths are JSON Pointers; `from`/`to` accept a version number or `draft` |

### Connections and secrets

All need a plan with BYO providers (403 `plan_forbids_byo` otherwise). Reads need `tenant:read`, the rest `tenant:write`.

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/tenants/{tid}/secrets` | Body `{kind, value}` (`kind` like `api_key` or `password`, value ≤ 4 KB) → 201 `{secret_ref}`; the value is never readable again or logged |
| GET/POST | `/v1/tenants/{tid}/connections` | Vector store connections: `{kind: "vector_store", type: qdrant\|pgvector\|milvus, endpoint, secret_ref?, settings?: {username, database}}`. The endpoint must pass the egress policy (`egress_denied`): https for Qdrant (gRPC port, default 6334) and Milvus (default 19530), `postgres://user@host/db?sslmode=require\|verify-ca\|verify-full` for pgvector (`tls_required`); no credentials in the URL. The secret is the Qdrant API key, the PostgreSQL password, or the Milvus password (with `settings.username`) or token. New connections are `untested`. `kind: "lexical"` is `byo_unavailable` in v1 |
| GET/PUT/DELETE | `/v1/tenants/{tid}/connections/{id}` | PUT on a connection serving the active or shadow version runs the test with the new settings first and saves only if it passes (422 `connection_test_failed`, nothing changed); other connections are saved as `untested`. DELETE is 409 `in_use` while the draft or a non-retired version references it |
| POST | `/v1/tenants/{tid}/connections/{id}/test` | Optional body `{collection, vectors, payload_mapping}`; without it, the draft's `stores.vector` block (which must use this connection). Checks `configure`, `health`, `collection`, `vectors` (dimension and metric as declared and supported), `fields` (product ID and always-on filter fields stored; unstored filterable fields are listed), `sample_query` (latency). Returns `{ok, checks: [{name, ok, message, latency_ms}]}` and sets the status to `validated` or `failed` (`last_error`) |

### Models

| Method | Path | Notes |
|---|---|---|
| GET/POST | `/v1/tenants/{tid}/models` | Register a remote embedder: `{kind: "embedder", type: kserve_v2\|openai_embeddings, name, version, endpoint, secret_ref?, input_schema: {modality: text\|image}, output_schema: {dim}, supports_text_to_image?, max_batch_size?}`. For `kserve_v2`, `name`/`version` are the KServe model name and version; for `openai_embeddings`, `name` is the `model` field (text only). The secret is sent as a bearer token. New models are `registered`. Prerank, rank and rerank models and ONNX uploads arrive in M7 (`not_available`) |
| GET | `/v1/tenants/{tid}/models/{id}` | Includes `status`, `transport` and `last_error` |
| POST | `/v1/tenants/{tid}/models/uploads` | M7: presigned S3 PUT for an ONNX artifact; then `POST /models` with `artifact_uri` |
| POST | `/v1/tenants/{tid}/models/{id}/validate` | Probe inferences for the model's modality (two texts; a small JPEG; the text tower when `supports_text_to_image`): output `[N, dim]` and L2-normalized. A `kserve_v2` model is tried over gRPC and then REST; the transport that works is recorded. Sets the status to `validated` or `failed` |
| GET | `/v1/platform/models` | Public catalog of the platform models tenants can select (`id`, `kind`, `modality`, `dim`, `supports_text_to_image`) |

### Indexing

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/tenants/{tid}/index/jobs` | Body `{config_version, upload_id?}` → 202 with the job. The indexer claims it, publishes the upload (if any) as catalog events, builds `v{n+1}` from the catalog topic with the version's config, verifies counts and swaps the alias |
| GET | `/v1/tenants/{tid}/index/jobs` | Recent jobs |
| GET | `/v1/tenants/{tid}/index/jobs/{id}` | `status` (`queued`, `running`, `succeeded`, `failed`), `total`, `done`, `failed`, `index_version`, `error` |
| POST | `/v1/tenants/{tid}/catalog/uploads` | Body `{format: "jsonl"}` → `{upload_id, method: PUT, url, headers, expires_at}` (15 min). One tenant product JSON per line, in the tenant's own shape (mapped by `catalog.field_mapping`) |
| POST | `/v1/tenants/{tid}/catalog/mapping/preview` | Body `{field_mapping, custom_fields?, samples}` (1–20 samples) → `{results: [{product, errors}]}`; nothing is stored |

### Ads

| Method | Path | Notes |
|---|---|---|
| CRUD | `/v1/tenants/{tid}/ads/campaigns` | |
| CRUD | `/v1/tenants/{tid}/ads/campaigns/{id}/creatives` | |
| GET | `/v1/tenants/{tid}/ads/reports?from&to` | Impressions, clicks, spend, CTR |

### Playground

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/tenants/{tid}/playground/search` | A `search-api` search request plus `config_version` (a number or `"draft"`, default draft). Returns the search response with the explain trace |
| POST | `/v1/tenants/{tid}/playground/search/image` | The image search multipart form; `?config_version=` |

control-api proxies playground calls to search-api with a one-minute token carrying `search:playground`. Only that scope makes search-api honour the `X-Config-Version` header (a version or `draft`). Playground runs always include the trace, and never touch the snapshot cache or shadow sampling.
