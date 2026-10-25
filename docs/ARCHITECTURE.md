# ARCHITECTURE.md

## 1. Services

| Binary | Plane | Responsibility |
|---|---|---|
| `search-api` | Data | Stateless. Resolves tenant config, executes the pipeline, ingests events. Horizontally scaled. |
| `control-api` | Control | Tenant sign-up, config drafts and versions, model registration, connection tests, index jobs, onboarding state, ads campaigns. Issues JWTs and serves JWKS. |
| `indexer` | Data | Consumes `catalog.product.v1`, maps products to the canonical schema, embeds them via the tenant's embedder, upserts to the lexical and vector stores. Also runs backfill jobs. |
| `featurejob` | Data | Aggregates ClickHouse events into Redis online features on a schedule. |
| `seed` | Dev | Publishes the prepared dataset as catalog events and creates the `demo` tenant. |

Infrastructure: MySQL (control plane), Redis (config invalidation, features, rate limits, ad pacing), OpenSearch, Qdrant, Kafka/Redpanda, ClickHouse, Triton, S3 object storage (SeaweedFS in dev).

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
     -> async: emit search.impression events
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
	Items(ctx context.Context, tenant TenantID, ids []string) (map[string]Features, error)
	User(ctx context.Context, tenant TenantID, userID string) (Features, error)
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
- `Providers` bundles a tenant's providers for one config version (embedders, stores, centroids, scorers); the registry builds and caches it. The filter AST lives in `internal/pipeline/filter`, the feature catalog in `internal/pipeline/feature`, and the HTTP layer of search-api in `internal/searchapi`.

`Filter` (the request-level structured filter) is not an interface. It is a typed AST (`FilterExpr`) that each `VectorStore` and `LexicalIndex` adapter translates into its native filter: Qdrant payload filter, OpenSearch `bool.filter`, or a SQL `WHERE` for pgvector. Pushdown is mandatory; post-filtering structured constraints is a bug.

## 4. Registry

- `registry.Registry` maps `(kind, type)` to a `Factory`, for example `("vector_store", "qdrant")` or `("scorer", "onnx")`.
- A factory receives the stage's validated config block plus a `Deps` struct (EgressClient, SecretStore, Meter, Logger) and returns the provider.
- Built providers are cached per `(tenant_id, config_version, stage)` in an LRU and closed on eviction or when the version is superseded.
- Platform defaults are registered under the `platform` provider type and point at shared infrastructure, using a tenant-scoped collection or index.

## 5. Pipeline engine

- Each stage gets `ctx` with a deadline of `min(stage.budget_ms, remaining request budget)`.
- Every provider call is wrapped by `resilience.Call(ctx, key, primary, fallback)`:
  - The circuit breaker is keyed by `tenant_id + stage + provider`.
  - When the primary errors or hits its deadline, the fallback runs if the remaining budget allows. Otherwise the stage passes its input through unchanged (stages 4–6) or returns an empty contribution (one retriever in stage 2).
  - A fallback is recorded in the trace and in the metric `search_stage_fallback_total{tenant,stage,reason}`.
- Retrieval fan-out uses `errgroup`. One retriever failing does not fail the stage. The stage fails only if all retrievers fail, in which case the response is an empty result with a `degraded=true` flag, never a 5xx.
- Merge uses Reciprocal Rank Fusion: `score = Σ w_r / (k + rank_r)`, with `k=60` by default and `w_r` per retriever from config.
- The explain trace records, per stage: provider, version, latency, input/output counts, fallback flag, and per-candidate scores (top 50).
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

### S4 Pre-rank

- An in-process ONNX scorer, or `default_linear` if none is configured.
- Features come from the candidate payload, retrieval scores and Redis item features, normalized per request: `rrf_score` and `bm25_norm` divide by the request maximum, cosines are used as returned, counts and prices are log-scaled.
- Keeps the top `prerank.keep` (default 300). Candidates that are cut stay behind in RRF order, and those cut by S5 stay in pre-rank order, so deep pages (up to the plan's page depth) follow pre-rank and then RRF order.

### S5 Rank

- Uses a remote KServe v2 scorer, the in-process ONNX scorer, or `default_linear`.
- Candidates are sent in one batched call.
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

**Bulkheads.** Each tenant gets a bounded concurrency semaphore for its BYO remote calls, so a slow tenant endpoint cannot exhaust the shared HTTP or gRPC pools.

**Metrics.** Metrics carry a `tenant` label. Tenant count is bounded by plan, so cardinality stays bounded; this must be revisited if tenants exceed about 1,000.

## 8. Config propagation

1. `control-api` writes an immutable version row, marks it active, and publishes `tenant.config.activated` on Redis pub/sub.
2. `search-api` instances invalidate their cache and lazily reload from MySQL.
3. A reconcile loop runs every 30 s and compares active versions, because pub/sub is lossy.
4. Provider instances for the old version drain: in-flight requests finish, then the providers close.

## 9. Security

**Egress.**
- Tenant endpoints must be `https`.
- Hosts are resolved at registration and on every dial. Connections to private, loopback, link-local and metadata ranges (RFC 1918, 127/8, 169.254/16, ::1, fc00::/7) are rejected via a custom `net.Dialer.Control`.
- An optional per-plan host allowlist applies on top.

**Secrets.**
- Credentials for BYO stores and models are stored only via `SecretStore` and referenced by `secret_ref` in config.
- Secrets are never returned by any API and never logged.

**CEL.**
- Uses the restricted environment from `docs/TENANT_CONFIG.md` §5.
- Programs are compiled at config validation.
- Runtime cost limit is set with `cel.CostLimit`, and expression size is bounded.

**Image uploads.**
- Maximum 10 MB; accepted formats are JPEG, PNG and WebP, detected by magic bytes.
- `image.DecodeConfig` must report dimensions ≤ 4096×4096 before full decode.
- The image is re-encoded to JPEG before embedding, which strips EXIF and metadata.

**Explain trace.** Allowed only for tokens with the `search:debug` scope.

**ONNX uploads.**
- Maximum 200 MB; the SHA-256 is recorded.
- Loaded in a session with intra-op threads limited and one session per `(tenant, version)`.
- Validated against the declared feature schema with a dry-run inference at registration.

## 10. Observability

- One OTel span per stage and per provider call, with attributes `tenant`, `stage`, `provider`, `fallback`, `in_count` and `out_count`.
- Metrics:
  - `search_request_duration_seconds{tenant,query_type}`
  - `search_stage_duration_seconds{tenant,stage,provider}`
  - `search_stage_fallback_total{tenant,stage,provider,reason}`
  - `search_results_empty_total`
  - `provider_breaker_state`
- Logs: one structured access log per request with the trace ID. Query text is logged only if the tenant enables it.
