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

**BYO `openai_embeddings` (text only).**
- Request: `POST {endpoint}/v1/embeddings` with `{model, input: [..]}`.
- The response's `data[i].embedding` is read and L2-normalized client-side if not already normalized.

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

Ensembles and preprocessors take `[N]` BYTES exactly as in §1 (`max_batch_size: 0`). The ONNX models batch dynamically (`max_batch_size: 64`). Text is padded to the longest item in a request, so only same-length requests merge. An undecodable image fails its request with `INVALID_ARG`; the indexer then embeds that batch one image at a time and indexes the bad product without an image vector. `make models-smoke` checks shapes, norms and a text→image sanity case.

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

CTR and CVR are position-debiased using the examination propensity per position, estimated by `featurejob` from synthetic randomized-slot traffic (see `docs/DATA.md` §5).

## 5. Training (`tools/training`)

**Pre-rank and rank models**
- **Data.** Labels come from ClickHouse: impression rows joined with click, cart and order events, with graded relevance `order=3`, `add_to_cart=2`, `click=1`, otherwise 0.
- **Algorithm.** LightGBM with `objective=lambdarank`, grouped by `request_id`.
- **Split.** By time: 80/10/10.
- **Export.** To ONNX via `onnxmltools`, with the feature list written to `<model>.features.json`.
- **Metrics.** nDCG@10 and MRR on the test split, written to `tools/eval/reports/`.

**Pre-rank specifically.** At most 15 features (cheap ones only) and at most 200 trees, so inference stays at or under 5 ms for 2000 candidates in-process.
