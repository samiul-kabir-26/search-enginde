# TENANT_CONFIG.md

## 1. Model

- **Draft.** Each tenant has exactly one mutable `draft`. The wizard and the admin UI edit the draft.
- **Version.** Publishing validates the draft and snapshots it as an immutable `version`, numbered monotonically.
- **Active.** Exactly one version is `active`. Activation is a separate action, so a rollback is simply activating an older version.
- **Shadow.** One version may be `shadow`. Shadow receives a sampled copy of traffic (configurable 0–20%); its results are logged, not returned.
- **Storage.** Config is stored as JSON in MySQL (`tenant_config_versions.document JSON`) along with `schema_version`.
- **Secrets.** Secrets appear only as `secret_ref` strings.

## 2. MySQL tables (control plane)

```
tenants(id CHAR(26) PK ULID, slug UNIQUE, name, plan, region, status, created_at, updated_at)
tenant_users(id, tenant_id FK, email UNIQUE per tenant, password_hash, role ENUM(owner,admin,developer,viewer), ...)
tenant_api_keys(id, tenant_id FK, prefix, hash, scopes JSON, last_used_at, revoked_at)
tenant_config_drafts(tenant_id PK FK, document JSON, schema_version, updated_at, updated_by)
tenant_config_versions(id, tenant_id FK, version INT, document JSON, schema_version, state ENUM(published,active,shadow,retired), created_at, created_by, UNIQUE(tenant_id, version))
tenant_secrets(id CHAR(26), tenant_id FK, kind, ciphertext VARBINARY, dek_wrapped VARBINARY, created_at, rotated_at)
tenant_models(id, tenant_id FK, kind ENUM(embedder,prerank,rank,rerank), type ENUM(onnx,kserve_v2,openai_embeddings), name, version, artifact_uri NULL, artifact_sha256 NULL, endpoint NULL, secret_ref NULL, input_schema JSON, output_schema JSON, status, validated_at)
tenant_connections(id, tenant_id FK, kind ENUM(vector_store,lexical), type, endpoint, secret_ref, settings JSON, status, last_tested_at, last_error)
onboarding_state(tenant_id PK FK, current_step, completed_steps JSON, updated_at)
index_jobs(id, tenant_id FK, config_version, status, total, done, failed, started_at, finished_at, error)
ad_campaigns(id, tenant_id FK, vendor_id, status, daily_budget_minor, currency, bid_minor, targeting JSON, start_at, end_at)
ad_creatives(id, campaign_id FK, product_id)
```

All tenant-scoped tables index `(tenant_id, ...)` as the leading column.

## 3. Config document (schema_version 1)

```json
{
  "schema_version": 1,
  "locale": "en",
  "budgets": {
    "total_ms": 300, "s1_ms": 40, "s2_ms": 120, "s3_ms": 10, "s4_ms": 15, "s5_ms": 40, "s6_ms": 30,
    "image": { "s1_ms": 130, "s2_ms": 80, "s3_ms": 5, "s4_ms": 10, "s5_ms": 25, "s6_ms": 20 }
  },

  "catalog": {
    "ingestion": "events",
    "field_mapping": {
      "product_id": "$.id",
      "title": "$.name",
      "description": "$.description",
      "category_path": "$.category.path",
      "brand": "$.brand",
      "price_minor": "$.price.amount_minor",
      "currency": "$.price.currency",
      "image_urls": "$.images[*].url",
      "vendor_id": "$.vendor.id",
      "attributes": "$.attributes"
    },
    "custom_fields": [
      { "name": "season", "type": "keyword", "source": "$.extra.season", "filterable": true, "facetable": true }
    ]
  },

  "query": {
    "attributes": { "material": ["vegan leather"], "style": ["boho", "mid century"] }
  },

  "embedders": {
    "text":  { "provider": "platform", "model": "bge-small-en-v1.5" },
    "image": { "provider": "platform", "model": "clip-vit-b32" }
  },

  "stores": {
    "lexical": { "provider": "platform" },
    "vector":  { "provider": "platform" }
  },

  "retrieval": {
    "text":  { "retrievers": [ { "name": "lexical_bm25", "k": 500, "weight": 1.0 }, { "name": "vector_text", "k": 500, "weight": 1.0 }, { "name": "vector_text_to_image", "k": 200, "weight": 0.5 } ] },
    "image": { "retrievers": [ { "name": "vector_image", "k": 500, "weight": 1.0 } ] },
    "rrf_k": 60,
    "field_boosts": { "title": 3.0, "brand": 2.0, "attributes": 1.5, "description": 1.0 },
    "ef": 128
  },

  "facets": [
    { "field": "category_ids", "size": 20 },
    { "field": "brand", "size": 20 },
    { "field": "price_minor", "ranges": [ { "to": 2500 }, { "from": 2500, "to": 5000 }, { "from": 5000 } ] }
  ],

  "policy": {
    "always_filters": [
      { "field": "status", "op": "eq", "value": "active" },
      { "field": "stock", "op": "gt", "value": 0 }
    ],
    "rules": [
      { "id": "hide-unverified-electronics", "kind": "filter", "expr": "!(item.category_ids.exists(c, c == 'electronics') && !item.vendor.verified)" },
      { "id": "boost-fast-shippers", "kind": "boost", "expr": "item.vendor.on_time_dispatch_rate > 0.95 ? 0.2 : 0.0" }
    ]
  },

  "prerank": { "scorer": { "type": "default_linear" }, "keep": 300 },

  "rank": {
    "scorer": {
      "type": "default_linear",
      "weights": {
        "rrf_score": 1.0, "bm25_norm": 0.4, "cosine_text": 0.6, "cosine_image": 0.6,
        "ctr_7d": 0.3, "cvr_7d": 0.5, "rating": 0.2, "vendor_score": 0.3, "policy_boost": 1.0
      }
    },
    "keep": 100
  },

  "rerank": {
    "personalization": { "enabled": true, "alpha": 0.15, "beta": 0.05 },
    "diversity": { "max_per_vendor": 2, "window": 10 }
  },

  "ads": {
    "enabled": true,
    "slots": [1, 5, 10],
    "relevance_floor": 0.35,
    "auction": "gsp"
  }
}
```

## 4. Provider block variants

**Platform default** (shared infrastructure, tenant-scoped collection or index):

```json
{ "provider": "platform", "model": "<platform model id>" }
```

**BYO vector store:**

```json
{ "provider": "byo", "connection_id": "01J...", "collection": "products", "vectors": { "text": { "name": "text", "dim": 384, "metric": "cosine" }, "image": { "name": "image", "dim": 512, "metric": "cosine" } }, "payload_mapping": { "product_id": "pid", "vendor_id": "vendor" } }
```

**BYO embedder:**

```json
{ "provider": "byo", "model_id": "01J..." }
```

The referenced `tenant_models` row has `type` set to `kserve_v2` or `openai_embeddings`.

**Scorers:**

```json
{ "type": "onnx", "model_id": "01J..." }
{ "type": "kserve_v2", "model_id": "01J..." }
{ "type": "default_linear", "weights": { ... } }
```

**BYO re-ranker** (replaces the built-in personalization formula; diversity and ads still apply after it):

```json
"rerank": { "model": { "type": "kserve_v2", "model_id": "01J..." }, "diversity": { ... } }
```

**Budgets.** `budgets.image` overrides any stage budget for image and similar-item queries (zero fields inherit): embedding an image on CPU needs a larger S1 than text (default `s1_ms` 130), and the default stays within the smallest plan maximum (300 ms). The defaults give S2 the largest share because it fans out to three stores, and S5 only 40 ms because `default_linear` scores in under a millisecond; a tenant with a remote rank model moves budget back to S5.

**Optional sections.** `query.attributes` extends the built-in S1 attribute dictionary (colours, materials, sizes) with tenant terms. `facets` replaces the default facets (`category_ids`, `brand`, `price_minor` ranges, `ship_regions`); numeric fields need `ranges` (half-open `[from, to)`), keyword fields take a `size`. `retrieval.ef` is the HNSW search breadth (default 128). `field_boosts` keys are canonical searchable fields or `custom.{name}`.

**Catalog mapping and custom fields** (PLAN.md R1). `field_mapping` maps canonical fields to RFC 9535 JSONPath expressions over the tenant's product JSON; `product_id`, `title`, `category_path`, `price_minor` and `currency` must be mapped, and `tenant_id`, `category_ids` and `listing_completeness` are derived, never mapped. `custom_fields` adds tenant fields: `{name, type, source, filterable, facetable, searchable, sortable, required}` with `type` one of `keyword`, `keyword[]`, `text`, `int`, `float`, `bool`, `date`. They are stored under `custom.{name}` (explicit OpenSearch mapping per index version, Qdrant payload index when filterable) and addressed as `custom.{name}` in filters, facets and CEL. Adding a field is an online change; changing a field's type or making it filterable needs a re-index.

## 5. Validation rules (enforced on publish)

**Budgets**
1. `budgets.total_ms` must be ≤ the plan maximum.
2. The sum of stage budgets must be ≤ `total_ms`. Both rules also apply to the effective image budgets (`budgets.image` over the text budgets).

**Embedders and stores**
3. Embedder output dimension must equal the corresponding vector store `dim`. Checked via a live embed of a fixed probe input.
4. The `metric` must be supported by the adapter.
5. Every `connection_id` and `model_id` must exist, belong to the tenant, and have `status = validated`.

**Retrieval**
6. Retrievers must be known for the query type, listed once, with `k` in `[1, 2000]` and `weight` in `(0, 5]`; `rrf_k` > 0. Retrievers referenced must be available given the configured embedders and stores. For example, `vector_text_to_image` requires the image embedder to expose a text tower: `clip-vit-b32` does; a BYO embedder must declare `supports_text_to_image: true`.

**Policy**
7. CEL rules compile in the restricted environment:
   - Variables: `item` (product payload plus `vendor`) and `query` (normalized text, attributes, predicted categories, user segment).
   - No custom functions beyond the standard library and `matches`.
   - `filter` rules must return `bool`; `boost` rules must return `double`.
   - Maximum 50 rules; each expression ≤ 2 KB; the estimated cost must be under the configured limit.
8. `always_filters` fields must exist in the canonical schema and be indexed for pushdown.

**Scorers and re-ranking**
9. Every feature in a scorer's feature schema must be resolvable from the feature catalog (`docs/MODELS.md` §4); `default_linear` weights are within `[0, 5]` and cannot use user features (re-rank only).
10. `prerank.keep` must be ≥ `rank.keep`, which must be ≥ 10.

**Ads**
11. `ads.slots` must be strictly increasing and within `[1, rank.keep]`.
12. `relevance_floor` must be in `[0, 1]`.

**Mapping**
13. `field_mapping` is a JSONPath per field. Keys must be mappable canonical fields, every mandatory field must be mapped, and every expression must parse. It is validated against a sample payload supplied in onboarding step 2.
14. `custom_fields` names match `^[a-z][a-z0-9_]{0,62}$` and are unique, their count is within the plan cap, `text` fields can only be searchable, only `text`/`keyword` fields can be searchable, `date` fields cannot be facets, and `keyword[]` fields cannot be sortable.

**Facets**
15. Facet fields must be facetable; numeric facets need `ranges`, keyword facets cannot have them; at most 20 facets, `size` ≤ 100.

The validation response returns every error, not just the first: `[{ "path": "/rank/scorer/model_id", "code": "model_not_validated", "message": "..." }]`.

## 6. Go representation

- Each section maps to a typed struct with `json` tags and `Validate(ctx, deps) []FieldError`.
- Decode with `DisallowUnknownFields`.
- Schema evolution: on load, a `schema_version` migration func upgrades older documents.
