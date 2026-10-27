# ARCHITECTURE.md

## 1. Services

| Binary | Plane | Responsibility |
|---|---|---|
| `search-api` | Data | Stateless. Resolves tenant config, executes the pipeline, ingests events (`POST /v1/events` → Kafka) and vendor features (`PUT /v1/vendors` → Redis), and emits an impression per returned result. Horizontally scaled. |
| `control-api` | Control | Tenant sign-up, users and API keys, config drafts and versions, onboarding state, catalog uploads and index jobs, the playground proxy, model registration and connection tests (M5), ads campaigns (M8), and the platform admin API. Issues JWTs from a MySQL key ring and serves the JWKS. |
| `indexer` | Data | Consumes `catalog.product.v1` and `catalog.vendor.v1`, stores vendor features in Redis, maps products to the canonical schema, embeds them via the tenant's embedder, upserts to the lexical and vector stores. Also runs index jobs queued by control-api: it claims them with `FOR UPDATE SKIP LOCKED`, publishes uploads as catalog events, and backfills a new index version. |
| `featurejob` | Data | Every 10 min, for each live tenant: estimates the position propensity from randomized traffic and aggregates 30 days of ClickHouse events into Redis item features (debiased CTR, CVR, orders) and user features (category affinity, price band). Refreshes category centroids hourly. |
| `seed` | Dev | Creates the `demo` tenant and publishes the prepared dataset as catalog and vendor events; `seed -events` publishes the simulated interaction history. |

Events: `search.events.v1` → ClickHouse Kafka engine table `events_kafka` → materialized view → `events` (`ReplacingMergeTree` on `event_id`) → featurejob → Redis `f:{tid}:i|v|u:*` → S4.

Infrastructure: MySQL (control plane), Redis (config invalidation, settings changes, idempotency keys, result snapshots, features, rate limits, ad pacing), Mailpit (dev SMTP), OpenSearch, Qdrant, Kafka/Redpanda, ClickHouse, Triton, S3 object storage (SeaweedFS in dev).

## 2. Request flow (`search-api`)

```
HTTP -> auth (JWT, tid claim) -> rate limit (per tenant) -> tenant resolver (config vN from cache)
     -> pipeline.Engine.Run(ctx, cfg, req)
          S1 QueryUnderstanding
          S2 Retrieval   (errgroup fan-out; RRF merge)
          S3 Filter      (pushdown filters already applied in S2; CEL rules applied here)
          S4 PreRank     (in-process scorer, cut to cfg.prerank.keep)
          S5 Rank        (scorer, cut to cfg.rank.keep)
          S6 ReRank      (personalization, diversity, ads blend)
     -> response (+ trace when explain=true and the caller is allowed to see it)
     -> async: emit one impression event per returned result (dropped, never waited on, if Kafka is down)
```

## 3. Consumer-side interfaces (`internal/pipeline`)

```go
type Embedder interface {
	EmbedText(ctx context.Context, texts []string) ([][]float32, error)
	EmbedImage(ctx context.Context, images [][]byte) ([][]float32, error)
	Dim() int
}

type VectorStore interface {
	Search(ctx context.Context, req VectorSearch) ([]Hit, error)
	Upsert(ctx context.Context, points []VectorPoint) error
	Delete(ctx context.Context, ids []string) error
	Health(ctx context.Context) error
}

type LexicalIndex interface {
	Search(ctx context.Context, req LexicalSearch) ([]Hit, error)
	Upsert(ctx context.Context, docs []Product) error
	Delete(ctx context.Context, ids []string) error
	Health(ctx context.Context) error
}

type Retriever interface {
	Name() string
	Retrieve(ctx context.Context, q *Query, k int) ([]Hit, error)
}

type PolicyEngine interface {
	Filter(ctx context.Context, q *Query, c []Candidate) ([]Candidate, error)
	Boost(ctx context.Context, q *Query, c []Candidate) error
}

type Scorer interface {
	Score(ctx context.Context, q *Query, c []Candidate) ([]float32, error)
	FeatureSchema() FeatureSchema
}

type FeatureReader interface {
	Read(ctx context.Context, tenant TenantID, req FeatureRequest) (*OnlineFeatures, error)
}

type ReRanker interface {
	ReRank(ctx context.Context, q *Query, u *UserContext, c []Candidate) ([]Candidate, error)
}

type AdsBlender interface {
	Blend(ctx context.Context, q *Query, organic []Candidate) ([]Slot, error)
}
```

As implemented:
- `LexicalIndex.Upsert` and `Delete` take source-versioned documents and return the product IDs rejected as stale, because the lexical index arbitrates catalog event order (`docs/DATA.md` §6). The indexer depends only on the write halves (`LexicalWriter`, `VectorWriter`).
- `LexicalIndex.Search` takes base filters (always_filters, in the query) and request filters (a post-filter) separately and returns hits plus disjunctive facet counts and the filtered match count; `Docs` hydrates display fields for one page. Retrievers read only ranking fields (OpenSearch doc values, a trimmed Qdrant payload), which is 3–5x faster than loading stored documents for hundreds of hits.
- Platform stores also implement `IndexVersions` (`Active`, `Latest`, `Create`, `Promote`, `Count`, `Drop`) for the alias-swap re-index.
- `FeatureReader.Read` takes the product IDs, vendor IDs and user of one request and returns their item, vendor and user features in one pipelined Redis round trip (`internal/providers/features`). It is platform-wide, not per tenant config, so S4 holds it rather than `Providers`.
- `Providers` bundles a tenant's providers for one config version (embedders, stores, centroids, scorers); the registry builds and caches it. The filter AST lives in `internal/pipeline/filter`, the feature catalog in `internal/pipeline/feature`, and the HTTP layer of search-api in `internal/searchapi`.

`Filter` (the request-level structured filter) is not an interface. It is a typed AST (`FilterExpr`) that each `VectorStore` and `LexicalIndex` adapter translates into its native filter: Qdrant payload filter, OpenSearch `bool.filter`, or a SQL `WHERE` for pgvector. Pushdown is mandatory; post-filtering structured constraints is a bug.

## 4. Registry

- `registry.Registry` maps `(kind, type)` to a `Factory`, for example `("vector_store", "qdrant")` or `("scorer", "onnx")`.
- A factory receives a `Spec` (the role's validated config block, the resolved tenant version, and the embedder dimensions that stores size their vectors from) plus a `Deps` struct: the platform clients (Triton, OpenSearch, Qdrant, the centroid store), the logger, and for BYO providers `registry.BYO` (the tenant's registered models and connections, the `SecretStore`, the egress client and the per-tenant bulkheads).
- BYO factories (`("embedder", "byo")`, `("vector_store", "byo")`) look up the referenced `tenant_models` or `tenant_connections` row, read its secret and build the adapter without any network I/O: gRPC and SQL connections are made by the first call, under that call's deadline, so a hung endpoint can never block a request outside a stage budget. A model or connection that is missing, not `validated`, or cannot be built becomes a provider whose every call fails; the stage then falls back like for any other failure, and the registry rebuilds it after 30 s. BYO providers are rebuilt every 5 min (`BYORefresh`) so connection edits and secret rotations are picked up.
- The lexical index is always platform-hosted in v1; only vector stores and embedders can be BYO.
- Built providers are cached per `(tenant_id, config_version, role)` in an LRU (roles: text/image embedders, lexical, vector store, centroids, pre-rank and rank scorers; the playground draft gets its own entries).
- A provider leaves the cache when it is evicted, or when activation supersedes its version (`Supersede` keeps the active and shadow versions). It is closed only after a drain grace (30 s), so requests already running on it finish. A version that comes back (rollback) is rebuilt.
- After an activation, search-api builds the new version's providers right away, so the first query does not pay for it.
- Time-sensitive entries (category centroids, which follow the live index version) are rebuilt after `Refresh` (2 min).
- Platform defaults are registered under the `platform` provider type and point at shared infrastructure, using a tenant-scoped collection or index. An unregistered `(kind, type)` fails the request with a clear error until its milestone registers it.

## 5. Pipeline engine

- Each stage gets `ctx` with a deadline of `min(stage.budget_ms, remaining request budget)`.
- Every provider call is wrapped by `resilience.Call(ctx, key, primary, fallback)`:
  - The circuit breaker is keyed by `tenant_id + stage + provider`.
  - When the primary errors or hits its deadline, the fallback runs if the remaining budget allows. Otherwise the stage passes its input through unchanged (stages 4–6) or returns an empty contribution (one retriever in stage 2).
  - A fallback is recorded in the trace and in the metric `search_stage_fallback_total{tenant,stage,reason}`.
- Retrieval fan-out uses `errgroup`. One retriever failing does not fail the stage. The stage fails only if all retrievers fail, in which case the response is an empty result with a `degraded=true` flag, never a 5xx.
- Merge uses Reciprocal Rank Fusion: `score = Σ w_r / (k + rank_r)`, with `k=60` by default and `w_r` per retriever from config.
- The explain trace records, per stage: provider, version, latency, input/output counts, fallback flag, and per-candidate scores (top 50).
- A vector retriever that depends on a BYO embedder or vector store falls back to the platform lexical index: when the store fails, times out or its breaker is open, or when S1 produced no vector because the BYO embedder failed (`no_vector`). The lexical search runs at most once per request, its hits are fused under `lexical_bm25` (so features stay truthful), and it is skipped when `lexical_bm25` already runs in the fan-out (its hits carry the request on their own). The trace records `vector_text:timeout>lexical_bm25`.
- Breakers (per tenant, stage and provider) trip on 10 consecutive failures, or on a failure ratio of at least 50% over at least 20 calls in 10 s, and stay open for 3 s, so a transient overload costs seconds of degraded results rather than tens. A primary that has a fallback gets 75% of the remaining budget, so the fallback can still run. Client cancellations do not count as failures.

## 6. Stage specifications

### S1 Query understanding

- Text: normalize (Unicode NFKC, lowercase, whitespace collapse). Extract attributes (color, material, size, price phrases) with a tenant-extensible dictionary. Price phrases become a pushdown `price_minor` filter. Predict the category by kNN vote over the category centroids of the text embedding (top 3 by a softmax over cosine similarity, confidence ≥ 0.15). An embedder failure skips the vector retrievers for that request and is reported as a fallback.
- Image: validate via `internal/imaging` (see §9), apply the optional crop box, embed the image, and predict the category by majority vote over the categories of the top-50 image neighbors (weighted by similarity; it votes on leaf categories and moves up one level of the category paths while no category reaches a 15% share). Image and similar-item queries use the `budgets.image` overrides.
- Similar: the handler loads the anchor's stored `text` and `image` vectors and excludes the anchor with a pushed-down `product_id ne` filter; the anchor's leaf category is the predicted category.
- Output is a `Query` struct: normalized text, attributes, predicted categories with confidence, embeddings per vector name, request filters, user context.

### S2 Retrieval

Available retrievers: `lexical_bm25`, `vector_text`, `vector_image`, `vector_text_to_image` (CLIP text tower against image vectors), `category_popular`.

| Query type | Retrievers used |
|---|---|
| Text | Those configured for the text query type |
| Image | `vector_image`; plus text retrievers if a refinement text is present |
| Similar | `vector_image` and `vector_text` on the anchor product's stored vectors |

Each retriever gets `k` from config (default 500, max 2000).

### S3 Filter and policy

- Pushdown filters (request facets plus the tenant's always-on filters, such as `status = active` and `stock > 0`) are applied inside S2.
- CEL `filter` rules run here on candidate payloads.
- CEL `boost` rules add bounded score modifiers. Boosts are clamped to `[-1, 1]` and applied in S5 as a feature, `policy_boost`.
- The rules are compiled once per (tenant, config version) by the registry. When a config has rules, S3 reads the request's online features (for `item.vendor`; S4 then reuses them) and evaluates every candidate in parallel across the CPUs, checking the stage deadline between chunks of 32. Candidates not evaluated in time, and rules that fail on a candidate, leave it unchanged; both are reported as `cel:timeout(n unevaluated)` and `cel:errors(n)`. Without rules S3 does nothing.
- Retrievers read the fields rules need with the ranking fields (doc values in OpenSearch, a trimmed payload in vector stores): brand, stock, status, ship regions and filterable custom fields.

### S4 Pre-rank

- An in-process ONNX scorer (the platform's `prerank-gbdt-v1` or a tenant's upload), or `default_linear`. A learned model gets its declared features in order (`FeatureSchema.Matrix`, declared defaults for missing values) and `policy_boost` is added to its output.
- Features come from the candidate payload, retrieval scores and Redis online features, normalized per request: `rrf_score` and `bm25_norm` divide by the request maximum, cosines are used as returned, counts and prices are log-scaled.
- Online features (item, vendor and user, `docs/MODELS.md` §4) are read once per request through `resilience.Call` (breaker `online_features`) with at most half of S4's remaining budget, so scoring always has time left. A failure or timeout leaves the declared defaults and is reported as `online_features:<reason>` in the trace; the search never fails for it. User features are carried to S6 on the candidates.
- Keeps the top `prerank.keep` (default 300). Candidates that are cut stay behind in RRF order, and those cut by S5 stay in pre-rank order, so deep pages (up to the plan's page depth) follow pre-rank and then RRF order.

### S5 Rank

- Uses a remote KServe v2 scorer (the platform's `rank-gbdt-v1` in Triton, or a tenant's endpoint), the in-process ONNX scorer, or `default_linear`.
- Candidates are sent in one batched call. A scorer that fails, times out or cannot be loaded leaves the pre-rank order (`resilience.Call` fallback); search never fails for it.
- Keeps the top `rank.keep` (default 100).

### S6 Re-rank and blend

1. Personalization adjusts the score: `final = rank_score + α·affinity(user, item) + β·price_band_match`. Alternatively a BYO re-ranker endpoint can replace this.
2. Diversity is a greedy pass enforcing `max_per_vendor` within each window of `window` results.
3. The ads blender fetches eligible sponsored candidates from `internal/ads`, then:
   - drops any whose relevance (S5 score, or the cosine for image queries) falls below the tenant's `relevance_floor`;
   - runs a GSP auction on `bid × quality`;
   - places winners at the configured slot positions, deduplicated against organic results;
   - labels each placement `sponsored=true` in the response.

## 7. Tenancy and isolation

**Tenant identity.** The tenant is taken from the JWT claim `tid` only.

**Index naming.**
- Platform-default OpenSearch index: `t_{tenant}_products_v{n}`, behind an alias `t_{tenant}_products`.
- Platform-default Qdrant collection: `t_{tenant}_products_v{n}`, behind an alias.
- Re-indexing builds `v{n+1}` and swaps the alias atomically.

**Rate limiting.** Redis GCRA per tenant, with limits set by the tenant's plan.

**Bulkheads.** Each tenant gets a bounded concurrency semaphore for its BYO remote calls (`BYO_MAX_CONCURRENCY`, default 32 per search-api), so a slow tenant endpoint cannot exhaust shared workers. A full bulkhead refuses the call at once (`bulkhead_full`) instead of queueing, so the stage's fallback runs with the budget left. BYO clients are per tenant and config version, never shared with the platform's pools. The chaos test (`internal/e2e`) holds another tenant's p95 steady while one tenant's endpoints hang.

**Metrics.** Metrics carry a `tenant` label. Tenant count is bounded by plan, so cardinality stays bounded; this must be revisited if tenants exceed about 1,000.

## 8. Config propagation

1. `control-api` writes an immutable version row. On activation, one transaction retires the previous active version and marks the new one active. It then publishes `tenant.config.activated` on Redis pub/sub as `{tenant_id, version, state}`, where `state` is `active`, `shadow` or `shadow_cleared`.
2. `search-api` instances drop that tenant's resolver entry, and the next request reloads it from MySQL. The e2e test measures 5 ms from the activate call to the first request served by the new version.
3. A reconcile loop runs every 30 s. It compares the cached active and shadow versions with MySQL (`ListLiveConfigVersions`), because pub/sub is lossy. The resolver TTL (`TENANT_CONFIG_TTL`, 10 min) is only a last backstop.
4. The registry supersedes the old version's providers, which drain (§4).

**Shadow mode** (R8).
- A tenant can run one published version as a shadow on 0–20% of its traffic.
- A sampled request is copied before the primary run. After the response is written, the copy runs on the shadow version on a bounded worker pool (`SHADOW_WORKERS`, default 8); when the pool is full, the comparison is dropped.
- Only fresh pipeline runs are sampled, not snapshot hits or playground calls.
- The results are never returned. Each comparison (top 10 IDs of both sides, overlap@10, both latencies, a degraded flag) is batched into ClickHouse `shadow_results` (90-day TTL).

**Playground.** control-api proxies playground searches to search-api with a one-minute token carrying `search:playground`. With that scope, search-api resolves the version named in `X-Config-Version` (`Resolver.Pinned`: immutable versions are cached, drafts are always reloaded), forces the explain trace, and skips snapshots and shadow sampling.

## 9. Security

**Egress** (`security.Egress`).
- Tenant endpoints must be `https` (pgvector: a `postgres://` URL with `sslmode=require`, `verify-ca` or `verify-full`). Credentials go in a secret, never in the URL.
- Hosts are resolved at registration and on every dial. The dialer's `Control` hook checks the address actually being connected, after resolution, so a name that resolves (or later rebinds) to a private, loopback, link-local, metadata, CGNAT, documentation, multicast or reserved address (RFC 1918, 127/8, 169.254/16, 100.64/10, ::1, fc00::/7, fe80::/10, NAT64 and 6to4 prefixes, IPv4-mapped forms) is refused. gRPC clients use the `passthrough` resolver and pgx an identity lookup, so the dialer sees host names.
- The plan's host allowlist (`plans.egress_allowlist`, exact names or `*.suffix`; empty means any public host) applies on top. HTTP clients use no proxy and follow no redirects.
- Platform hosts (`EGRESS_TRUSTED_HOSTS`, plus the S3 hosts in the indexer) skip these checks; catalog image URLs go through the same egress client.

**Secrets** (`security.SecretStore`).
- Credentials for BYO stores and models are stored only via `SecretStore` and referenced by `secret_ref` (a ULID) from connections and models.
- Dev (`SECRETS_BACKEND=mysql`): envelope encryption in `tenant_secrets`. Each secret has a random 256-bit data key (AES-256-GCM, bound to tenant and ID); the data key is stored wrapped by `SECRETS_MASTER_KEY`.
- Prod (`SECRETS_BACKEND=vault`): Vault KV v2 at `{mount}/data/{prefix}/{tenant}/{ref}`.
- Secrets are never returned by any API and never logged: values travel as `security.Secret`, which prints, logs and marshals as `[redacted]`.

**CEL.**
- Uses the restricted environment from `docs/TENANT_CONFIG.md` §5 rule 7: two map variables, the standard library only, field names checked against the item and query schemas.
- Programs are compiled at config validation and again (once per config version) by the registry.
- The estimated cost is bounded at compile time and `cel.CostLimit` cuts off each evaluation; expressions are at most 2 KB and 50 per tenant.

**Image uploads.**
- Maximum 10 MB; accepted formats are JPEG, PNG and WebP, detected by magic bytes.
- `image.DecodeConfig` must report dimensions ≤ 4096×4096 before full decode.
- The image is re-encoded to JPEG before embedding, which strips EXIF and metadata.

**Explain trace.** Allowed only for tokens with the `search:debug` scope.

**ONNX uploads.**
- Maximum 200 MB, through a 15-minute presigned PUT into the models bucket; the SHA-256 is recorded and checked again whenever search-api loads the model.
- Loaded in a session with intra-op threads limited (`ONNX_INTRA_OP_THREADS`, default 1) and one session per `(tenant, version)`.
- Validated against the declared feature schema with a dry-run inference at registration: one `FEATURES` FP32 `[N, F]` input of the declared width, a `SCORES` output, finite scores for the declared defaults.
- The services are built with cgo and ship ONNX Runtime (`deploy/Dockerfile`, distroless `cc` image); without the library, uploads are refused and ONNX scorers fall back.

## 10. Observability

- One OTel span per stage and per provider call, with attributes `tenant`, `stage`, `provider`, `fallback`, `in_count` and `out_count`.
- Metrics:
  - `search_request_duration_seconds{tenant,query_type}`
  - `search_stage_duration_seconds{tenant,stage,provider}`
  - `search_stage_fallback_total{tenant,stage,provider,reason}`
  - `search_results_empty_total`
  - `provider_breaker_state`
- Logs: one structured access log per request with the trace ID. Query text is logged only if the tenant enables it.
- Dashboards (M10): Grafana provisions **Search overview** (throughput, end-to-end and per-stage latency percentiles by tenant, query type and provider, empty results, HTTP status) and **Search resilience** (fallbacks by stage, provider and reason, fallback share per tenant, breaker states) from `deploy/grafana/provisioning/dashboards/`, generated by `deploy/grafana/gen_dashboards.py`. Operating procedures are in `docs/RUNBOOK.md`.
