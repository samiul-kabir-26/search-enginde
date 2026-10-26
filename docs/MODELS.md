# MODELS.md

## 1. Inference contracts

All remote models use the **KServe v2 / Open Inference Protocol** over gRPC. REST is accepted as a fallback transport for BYO endpoints. Tensor names are fixed as below.

| Kind | Inputs | Outputs |
|---|---|---|
| Embedder (text) | `INPUT_TEXT` BYTES `[N]` (UTF-8) | `EMBEDDING` FP32 `[N, D]`, L2-normalized |
| Embedder (image) | `INPUT_IMAGE` BYTES `[N]` (encoded JPEG) | `EMBEDDING` FP32 `[N, D]`, L2-normalized |
| Scorer (prerank/rank) | `FEATURES` FP32 `[N, F]` | `SCORES` FP32 `[N]` |
| Re-ranker | `FEATURES` FP32 `[N, F]`, `USER_FEATURES` FP32 `[1, U]` | `SCORES` FP32 `[N]` |

Preprocessing (tokenization, image resize and normalization) is the model server's responsibility. This guarantees parity between indexing and query time, because both call the same endpoint.

**BYO `kserve_v2` endpoints.** A registered model names one KServe model (`name`, `version`) at `https://host[:port][/base]`. The adapter speaks gRPC by default; REST is the fallback transport, chosen when model validation finds that gRPC fails and REST works:
- REST requests go to `POST {base}/v2/models/{name}[/versions/{version}]/infer`. Text is sent as JSON `BYTES` strings; images use the binary tensor data extension (Triton, KServe): a JSON header (`Inference-Header-Content-Length`) followed by each image as a 4-byte little-endian length and its bytes. The `EMBEDDING` output may come back as JSON data or binary data.
- An image model with `supports_text_to_image` must accept `INPUT_TEXT` on the same model and embed into the same space.
- The secret, when set, is sent as `authorization: Bearer …` (gRPC metadata or HTTP header).

**BYO `openai_embeddings` (text only).**
- Request: `POST {endpoint}/v1/embeddings` with `{model, input: [..]}`.
- The response's `data[i].embedding` is read (ordered by `data[i].index`) and L2-normalized client-side if not already normalized. The registered model's `name` is the `model` field; the secret is the bearer token.

**Batching.** Maximum batch size is declared per model; the adapter splits larger batches.

## 2. Platform default models

| ID | Source | Dim | Use |
|---|---|---|---|
| `clip-vit-b32` | `laion/CLIP-ViT-B-32-laion2B-s34B-b79K` (Hugging Face, MIT) | 512 | Image embeddings, plus text→image via the text tower |
| `bge-small-en-v1.5` | `BAAI/bge-small-en-v1.5` (Hugging Face, MIT) | 384 | Text semantic retrieval (CLS pooling, normalized) |
| `prerank-gbdt-v1` | LightGBM lambdarank (`make train`, §5) | n/a | Pre-rank, in-process ONNX (`{"type": "onnx", "model": "prerank-gbdt-v1"}`) |
| `rank-gbdt-v1` | LightGBM lambdarank (`make train`, §5) | n/a | Rank, Triton ONNX backend as `rank_gbdt_v1` (`{"type": "kserve_v2", "model": "rank-gbdt-v1"}`) |

The learned scorers are published to the models bucket as `platform/<id>/model.onnx` plus `platform/<id>/features.json` (the ordered feature schema, sha256 and metrics), and `rank_gbdt_v1` is written into the Triton model repository (restart Triton to load a new version). `GET /v1/platform/models` lists them with `available: false` until trained; config validation and the onboarding `learned` mode refuse them until then.

Verify each model's license before shipping it as a platform default to tenants.

## 3. Triton setup (`deploy/triton/model_repository`)

**Export.** `tools/models/export.py` exports with `torch.onnx.export`:
- the vision encoder via `transformers.CLIPVisionModelWithProjection`;
- the text encoder via `CLIPTextModelWithProjection`;
- the BGE encoder.

All exports use dynamic batch axes. Verify ONNX vs PyTorch parity: cosine ≥ 0.999 on 100 samples.

Pooling and L2 normalization are part of each exported graph, and the Hugging Face revisions are pinned in `export.py`. `make models` writes the ONNX files and tokenizer/processor assets (git-ignored); the configs and `model.py` files are committed.

**Ensembles.** Each public embedder is an ensemble of a Python-backend preprocessing model (`*_pre`) and an ONNX model:

| Ensemble | Preprocessing | ONNX model |
|---|---|---|
| `clip_image` | `clip_image_pre`: decode JPEG/PNG/WebP (≤ 4096² pixels), `CLIPImageProcessor` | `clip_vision` → `[N, 512]` |
| `clip_text` | `clip_text_pre`: tokenizer, max 77 tokens | `clip_text_encoder` → `[N, 512]` |
| `bge_text` | `bge_text_pre`: tokenizer, max 512 tokens | `bge_encoder` (CLS pooling) → `[N, 384]` |

Ensembles and preprocessors take `[N]` BYTES exactly as in §1 (`max_batch_size: 0`). The ONNX models batch dynamically (`max_batch_size: 64`). Text is padded to the longest item in a request rounded up to a multiple of 16 tokens, so queries of similar length share a shape and merge in the batcher; `clip_text_pre` truncates at 64 tokens (CLIP has 77 positions). The query-time text encoders run two instances each with 2 intra-op threads: more threads oversubscribe the CPU when CLIP and BGE run concurrently, which pushed S1 past its 40 ms budget under load. An undecodable image fails its request with `INVALID_ARG`; the indexer then embeds that batch one image at a time and indexes the bad product without an image vector. `make models-smoke` checks shapes, norms and a text→image sanity case.

**Runtime.** Use the CPU instance group in dev, and configure dynamic batching (`max_queue_delay_microseconds` ≈ 2000). Pin the Triton image tag; the Python backend dependencies are installed into a custom image built in `deploy/triton/Dockerfile`.

## 4. Feature catalog (scorer inputs)

Scorers declare an ordered list of feature names; the engine builds the `[N, F]` tensor in that order. Missing values use the declared default.

| Feature | Source |
|---|---|
| `rrf_score`, `bm25_norm`, `cosine_text`, `cosine_image`, `retriever_count` | Retrieval |
| `policy_boost` | CEL boost rules |
| `price_minor_log`, `rating`, `rating_count_log`, `listing_completeness`, `image_count`, `age_days` | Product payload |
| `ctr_7d`, `ctr_30d`, `cvr_7d`, `cvr_30d`, `orders_30d_log` | Redis item features (`featurejob`) |
| `vendor_score`, `vendor_on_time_dispatch_rate`, `vendor_dispute_rate`, `vendor_response_hours`, `vendor_verified` | Redis vendor features |
| `query_category_match` | S1 predicted categories ∩ item categories |
| `user_category_affinity`, `user_price_band_match` | Redis user features (re-rank only) |

Features are normalized per request to keep linear weights comparable: `rrf_score` and `bm25_norm` divide by the request maximum, `rating` by 5, `price_minor_log` = ln(1 + price/100)/10, `rating_count_log` = ln(1 + count)/10, `image_count` = min(n, 10)/10, `age_days` = days/730 (capped at 1). Declared defaults for missing values live in `internal/pipeline/feature`.

**Online features** (`featurejob`, PLAN.md R9). Every run reads the last 30 days of ClickHouse events (`FINAL`, so resent events count once) and writes Redis; S4 reads them in one pipelined round trip.
- **Propensity.** The examination probability is modelled as `p(pos) = pos^-η`. In a randomized request every item is equally relevant in expectation at every position, so CTR at position k is proportional to `p(k)`: η is fitted by least squares on `ln CTR_k = c − η·ln k` over impressions marked `randomized` (positions with ≥ 50 impressions, weighted by clicks), clamped to [0, 2], and needs ≥ 200 randomized clicks over ≥ 5 positions. Otherwise the previous estimate is kept (`f:{tid}:propensity`), and a tenant that never had randomized traffic gets η = 0 (no debiasing, logged). On the synthetic data the estimate is 0.70 against the simulator's hidden 0.7.
- **`ctr_7d`, `ctr_30d`.** Clicks per *examination*, where each impression counts `p(pos)` examinations, shrunk toward the tenant mean with a prior of 20 examinations: `(clicks + 20·μ) / (exams + 20)`. An item shown only at deep positions is not penalized for being rarely seen, and an item with a few lucky clicks is not ranked by noise.
- **`cvr_7d`, `cvr_30d`.** Orders per click, shrunk toward the tenant mean with a prior of 10 clicks. A click already implies examination, so CVR needs no position correction of its own.
- **`orders_30d_log`** = min(ln(1 + orders)/10, 1). Products with no events in 30 days have no entry and take the declared default (0).
- **Vendor features** come from the tenant (`PUT /v1/vendors`, `catalog.vendor.v1`, `catalog.vendor_mapping`); score and rates are used as stored, `vendor_response_hours` = min(hours/48, 1), `vendor_verified` ∈ {0, 1}. At most 300 distinct vendors are read per request.
- **User features.** Per user, interactions in 30 days are weighted like the training labels (order 3, cart 2, click 1). `user_category_affinity` is the user's share of that weight in the candidate's categories (the best of its category IDs; top 20 categories kept), and `user_price_band_match` is 1 when the price lies within the user's weighted 20th–80th percentile of interacted prices (≥ 3 products needed). Categories and prices come from the tenant's lexical index.
- **Freshness.** ClickHouse ingests events within seconds (Kafka engine flush) and featurejob runs every `FEATUREJOB_INTERVAL` (10 min), so a feature is at most ~10 min plus one run (seconds) old; item keys expire after 48 h without a refresh and user keys after 30 days. A Redis failure in S4 leaves the declared defaults (trace `online_features:<reason>`) and never fails the search; the read gets at most half of S4's remaining budget.

## 5. Training (`tools/training`)

`make train TENANT=demo` runs the three steps below and then `featurejob publish`.

**Dataset** (`featurejob dataset`, Go). Training features must be the ones the serving pipeline computes, so the dataset is built by the serving code itself:
- **Requests.** search-api logs every served page (`search.requests.v1` → ClickHouse `search_requests`: request ID, type, query, user, config version); `seed -events` publishes the simulator's request log the same way. The builder takes the window's text requests with at least one click, cart or order, in time order, evenly sampled to `-max-requests` (default 20,000).
- **Labels.** Each request's impressions joined with its click, cart and order events: graded `order=3`, `add_to_cart=2`, `click=1`, otherwise 0. A `randomized` column marks requests shown in a randomized order.
- **Split.** By time: the first 80% of requests train, the next 10% validate, the last 10% test.
- **Features.** Each query is replayed through S1 and S2 of the tenant's active config with a filter restricted to the request's impressed products, then S4's feature builder computes the request-local features (`age_days` relative to the request time) and the online features are added. Item features (CTR, CVR, orders) are aggregated by the feature job **as of the end of the training period**, so no feature sees the period it is evaluated on; vendor features are the tenant's current values. Retrieval ranks inside the restricted set differ slightly from a full query's (RRF and `bm25_norm` are normalized within the request), a skew the models tolerate because they learn relative orderings.
- **Output.** CSV: identifiers, label, `randomized`, the `default_linear` scores of the config (the baseline), then every non-user catalog feature in §4 order, missing values at their declared defaults (exactly what `FeatureSchema.Matrix` sends a model).

**Training** (`tools/training/train.py`, Python).
- **Algorithm.** LightGBM `objective=lambdarank` grouped by request, nDCG@10 truncation 20, early stopping on validation nDCG@10 (patience 50), deterministic with a fixed seed.
- **Pre-rank.** 15 cheap features (retrieval scores, payload, `query_category_match`, `ctr_7d`, `cvr_7d`, `vendor_score`), at most **120 trees** of 15 leaves: 200 trees took 7.4 ms for 2,000 candidates on one thread, over the 5 ms budget, while 120 take ~4.3 ms. The learning rate is chosen from {0.05, 0.1, 0.15, 0.2} by validation nDCG@10.
- **Rank.** All 22 non-user features except `policy_boost`, up to 500 trees of 31 leaves, learning rate from {0.05, 0.1} by validation nDCG@10.
- **`policy_boost`** is never a model input: tenant rules change without retraining, so serving adds it to a learned model's output (§1 scorer contract, `FeatureSchema.WithPolicyBoost`). User features stay re-rank only.
- **Export.** `onnxmltools` (opset 15) with the output reshaped to `SCORES [N]`; ONNX Runtime must match LightGBM within 1e-4 on test rows. Each model gets `features.json`.
- **Metrics.** nDCG@10 and MRR of each ranker re-ranking the test requests' logged impressions: the logged order, `default_linear` (pre-rank and rank) and both learned models, with a 95% bootstrap interval of the difference to `default_linear`, on all test requests and on the randomized ones only (unbiased: their order carried no information). Written to `tools/eval/reports/{date}_ranking.md`; the run fails if the rank model does not beat `default_linear`.

**Your own model.** A tenant can train on its own data with these tools and upload the result (§6).

## 6. Uploaded and remote scorers

A tenant (plan with BYO providers) registers a learned scorer for `prerank` or `rank`:
- **ONNX upload.** `POST /v1/tenants/{tid}/models/uploads` → a 15-minute presigned PUT (≤ 200 MB) into the models bucket; then `POST /models` with `{kind, type: "onnx", name, version, upload_id, input_schema: {features}}`. The features (1–64, catalog names, no user features) are the model's input order. Registration reads the artifact, records its SHA-256 and dry-runs it in ONNX Runtime on two rows of declared defaults: one input `FEATURES` FP32 `[N, F]` with F = the declared count, an output `SCORES` FP32 `[N]` or `[N, 1]`, finite scores. It is `validated` at once; search-api checks the SHA-256 again when it loads the model.
- **Remote kserve_v2.** `{kind, type: "kserve_v2", name, version, endpoint, secret_ref?, input_schema: {features}}`; `POST /models/{id}/validate` scores two rows over gRPC, then REST, and records the transport that worked.
- **Serving.** In-process ONNX scorers run one session per (tenant, config version) with `ONNX_INTRA_OP_THREADS` (default 1) intra-op threads; remote scorers take a slot of the tenant's bulkhead. A model that cannot be loaded, or fails, falls back to the stage's input order (trace reason); it is rebuilt after 30 s.

