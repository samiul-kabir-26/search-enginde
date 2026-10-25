"""Export the platform embedders to ONNX and lay out the Triton model repository.

Each embedder becomes three Triton models: a Python-backend preprocessor (`*_pre`), an ONNX
encoder whose graph already ends with pooling and L2 normalization, and an ensemble that wires
them together under the public name (`clip_image`, `clip_text`, `bge_text`). The configs and
`model.py` files are committed; this script writes the ONNX graphs and tokenizer/processor
assets next to them (git-ignored).

    uv run python -m models.export            # export + parity check
    uv run python -m models.export --check    # parity check against existing exports only
"""

from __future__ import annotations

import argparse
import json
import random
from dataclasses import dataclass
from pathlib import Path

import numpy as np
import onnxruntime as ort
import torch
from PIL import Image
from transformers import (
    AutoModel,
    AutoTokenizer,
    CLIPImageProcessor,
    CLIPTextModelWithProjection,
    CLIPTokenizer,
    CLIPVisionModelWithProjection,
)

REPO = Path(__file__).resolve().parents[2] / "deploy" / "triton" / "model_repository"

CLIP = ("laion/CLIP-ViT-B-32-laion2B-s34B-b79K", "1a25a446712ba5ee05982a381eed697ef9b435cf")
BGE = ("BAAI/bge-small-en-v1.5", "5c38ec7c405ec4b44b94cc5a9bb96e735b38267a")

OPSET = 17
PARITY_SAMPLES = 100
PARITY_MIN_COSINE = 0.999


class VisionEncoder(torch.nn.Module):
    def __init__(self, model: CLIPVisionModelWithProjection):
        super().__init__()
        self.model = model

    def forward(self, pixel_values: torch.Tensor) -> torch.Tensor:
        emb = self.model(pixel_values=pixel_values).image_embeds
        return torch.nn.functional.normalize(emb, dim=-1)


class ClipTextEncoder(torch.nn.Module):
    def __init__(self, model: CLIPTextModelWithProjection):
        super().__init__()
        self.model = model

    def forward(self, input_ids: torch.Tensor, attention_mask: torch.Tensor) -> torch.Tensor:
        emb = self.model(input_ids=input_ids, attention_mask=attention_mask).text_embeds
        return torch.nn.functional.normalize(emb, dim=-1)


class BgeEncoder(torch.nn.Module):
    def __init__(self, model):
        super().__init__()
        self.model = model

    def forward(self, input_ids: torch.Tensor, attention_mask: torch.Tensor) -> torch.Tensor:
        cls = self.model(input_ids=input_ids, attention_mask=attention_mask).last_hidden_state[:, 0]
        return torch.nn.functional.normalize(cls, dim=-1)


@dataclass
class Export:
    name: str
    module: torch.nn.Module
    inputs: dict[str, torch.Tensor]
    dynamic_axes: dict[str, dict[int, str]]

    @property
    def path(self) -> Path:
        return REPO / self.name / "1" / "model.onnx"


def export_onnx(e: Export) -> None:
    e.path.parent.mkdir(parents=True, exist_ok=True)
    e.module.eval()
    with torch.no_grad():
        torch.onnx.export(
            e.module,
            tuple(e.inputs.values()),
            str(e.path),
            input_names=list(e.inputs),
            output_names=["EMBEDDING"],
            dynamic_axes={**e.dynamic_axes, "EMBEDDING": {0: "batch"}},
            opset_version=OPSET,
            dynamo=False,
        )
    print(f"exported {e.path.relative_to(REPO.parent.parent.parent)}")


def cosine_rows(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    a = a / np.linalg.norm(a, axis=1, keepdims=True)
    b = b / np.linalg.norm(b, axis=1, keepdims=True)
    return (a * b).sum(axis=1)


def check_parity(e: Export, batches: list[dict[str, torch.Tensor]]) -> float:
    sess = ort.InferenceSession(str(e.path), providers=["CPUExecutionProvider"])
    worst = 1.0
    with torch.no_grad():
        for batch in batches:
            ref = e.module(*batch.values()).numpy()
            got = sess.run(["EMBEDDING"], {k: v.numpy() for k, v in batch.items()})[0]
            worst = min(worst, float(cosine_rows(ref, got).min()))
            norms = np.linalg.norm(got, axis=1)
            if not np.allclose(norms, 1.0, atol=1e-3):
                raise SystemExit(f"{e.name}: outputs are not L2-normalized (norms {norms[:4]})")
    status = "ok" if worst >= PARITY_MIN_COSINE else "FAIL"
    print(f"parity {e.name}: min cosine {worst:.6f} over {PARITY_SAMPLES} samples [{status}]")
    if worst < PARITY_MIN_COSINE:
        raise SystemExit(f"{e.name}: parity below {PARITY_MIN_COSINE}")
    return worst


def sample_texts(rng: random.Random, n: int) -> list[str]:
    colors = ["red", "black", "navy blue", "white", "olive", "beige", "silver", "pink"]
    materials = ["leather", "cotton", "stainless steel", "oak wood", "polyester", "ceramic"]
    types = ["handbag", "running shoe", "office chair", "water bottle", "phone case", "lamp"]
    brands = ["Amazon Basics", "Rivet", "Stone & Beam", "Goodthreads", "Pinzon", "Solimo"]
    out = []
    for i in range(n):
        c, m, t, b = (rng.choice(x) for x in (colors, materials, types, brands))
        long_tail = " ".join(rng.choice(materials) for _ in range(rng.randint(0, 60)))
        out.append(
            f"{c} {m} {t}" if i % 2 else f"{t}. {b}. {t}s. color:{c} material:{m} {long_tail}"
        )
    return out


def sample_images(rng: random.Random, n: int, image_dir: Path | None) -> list[Image.Image]:
    if image_dir is not None:
        files = sorted(p for p in image_dir.rglob("*.jpg"))
        rng.shuffle(files)
        if len(files) >= n:
            return [Image.open(p).convert("RGB") for p in files[:n]]
    np_rng = np.random.default_rng(rng.randint(0, 2**31))
    images = []
    for _ in range(n):
        h, w = int(np_rng.integers(64, 512)), int(np_rng.integers(64, 512))
        base = np.linspace(0, 255, w, dtype=np.float32)[None, :, None].repeat(h, 0).repeat(3, 2)
        noise = np_rng.normal(0, 40, size=(h, w, 3))
        images.append(Image.fromarray(np.clip(base + noise, 0, 255).astype(np.uint8)))
    return images


def chunks(xs: list, size: int) -> list[list]:
    return [xs[i : i + size] for i in range(0, len(xs), size)]


def write_assets(name: str, saver) -> None:
    out = REPO / name / "1" / "assets"
    out.mkdir(parents=True, exist_ok=True)
    saver.save_pretrained(out)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    ap.add_argument("--check", action="store_true", help="only run parity against existing exports")
    ap.add_argument("--images", type=Path, help="directory of JPEGs for image parity samples")
    ap.add_argument("--seed", type=int, default=42)
    args = ap.parse_args()
    rng = random.Random(args.seed)
    torch.manual_seed(args.seed)

    clip_kw = {"revision": CLIP[1], "attn_implementation": "eager"}
    processor = CLIPImageProcessor.from_pretrained(CLIP[0], revision=CLIP[1])
    clip_tok = CLIPTokenizer.from_pretrained(CLIP[0], revision=CLIP[1])
    bge_tok = AutoTokenizer.from_pretrained(BGE[0], revision=BGE[1])

    vision = VisionEncoder(CLIPVisionModelWithProjection.from_pretrained(CLIP[0], **clip_kw))
    clip_text = ClipTextEncoder(CLIPTextModelWithProjection.from_pretrained(CLIP[0], **clip_kw))
    bge = BgeEncoder(
        AutoModel.from_pretrained(BGE[0], revision=BGE[1], attn_implementation="eager")
    )

    texts = sample_texts(rng, PARITY_SAMPLES)
    images = sample_images(rng, PARITY_SAMPLES, args.images)

    def pixel(batch):
        return {"pixel_values": processor(images=batch, return_tensors="pt")["pixel_values"]}

    def tokens(tok, max_length):
        def enc(batch):
            t = tok(
                batch, padding=True, truncation=True, max_length=max_length, return_tensors="pt"
            )
            return {"input_ids": t["input_ids"], "attention_mask": t["attention_mask"]}

        return enc

    seq_axes = {"input_ids": {0: "batch", 1: "seq"}, "attention_mask": {0: "batch", 1: "seq"}}
    clip_enc, bge_enc = tokens(clip_tok, 77), tokens(bge_tok, 512)
    exports = [
        (
            Export("clip_vision", vision, pixel(images[:2]), {"pixel_values": {0: "batch"}}),
            [pixel(b) for b in chunks(images, 25)],
        ),
        (
            Export("clip_text_encoder", clip_text, clip_enc(texts[:2]), seq_axes),
            [clip_enc(b) for b in chunks(texts, 25)],
        ),
        (
            Export("bge_encoder", bge, bge_enc(texts[:2]), seq_axes),
            [bge_enc(b) for b in chunks(texts, 25)],
        ),
    ]

    report = {}
    for e, batches in exports:
        if not args.check:
            export_onnx(e)
        report[e.name] = check_parity(e, batches)

    if not args.check:
        write_assets("clip_image_pre", processor)
        write_assets("clip_text_pre", clip_tok)
        write_assets("bge_text_pre", bge_tok)
        (REPO / "SOURCES.json").write_text(
            json.dumps(
                {
                    "clip": {"repo": CLIP[0], "revision": CLIP[1]},
                    "bge": {"repo": BGE[0], "revision": BGE[1]},
                    "opset": OPSET,
                    "parity_min_cosine": report,
                },
                indent=2,
            )
            + "\n"
        )


if __name__ == "__main__":
    main()
