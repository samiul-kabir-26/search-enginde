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
| `prerank-gbdt-v1` | Trained in M7 | n/a | In-process ONNX |
| `rank-gbdt-v1` | Trained in M7 | n/a | Triton ONNX backend |

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

**Pre-rank and rank models**
- **Data.** Labels come from ClickHouse: impression rows joined with click, cart and order events, with graded relevance `order=3`, `add_to_cart=2`, `click=1`, otherwise 0.
- **Algorithm.** LightGBM with `objective=lambdarank`, grouped by `request_id`.
- **Split.** By time: 80/10/10.
- **Export.** To ONNX via `onnxmltools`, with the feature list written to `<model>.features.json`.
- **Metrics.** nDCG@10 and MRR on the test split, written to `tools/eval/reports/`.

**Pre-rank specifically.** At most 15 features (cheap ones only) and at most 200 trees, so inference stays at or under 5 ms for 2000 candidates in-process.
