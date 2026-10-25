"""Smoke-test the Triton embedders: shapes, L2 norms and a semantic sanity check.

uv run python -m models.smoke [--url http://localhost:8000]
"""

from __future__ import annotations

import argparse
import io
import json
import sys
import urllib.request

import numpy as np
from PIL import Image, ImageDraw


def infer(url: str, model: str, name: str, blobs: list[bytes]) -> np.ndarray:
    """Calls the KServe v2 REST API with BYTES sent through the binary data extension."""
    payload = b"".join(len(x).to_bytes(4, "little") + x for x in blobs)
    header = json.dumps(
        {
            "inputs": [
                {
                    "name": name,
                    "datatype": "BYTES",
                    "shape": [len(blobs)],
                    "parameters": {"binary_data_size": len(payload)},
                }
            ],
            "outputs": [{"name": "EMBEDDING", "parameters": {"binary_data": False}}],
        }
    ).encode()
    req = urllib.request.Request(
        f"{url}/v2/models/{model}/infer",
        header + payload,
        {
            "Content-Type": "application/octet-stream",
            "Inference-Header-Content-Length": str(len(header)),
        },
    )
    with urllib.request.urlopen(req, timeout=60) as resp:
        out = json.load(resp)["outputs"][0]
    return np.array(out["data"], dtype=np.float32).reshape(out["shape"])


def circle(color: str) -> bytes:
    im = Image.new("RGB", (320, 240), "white")
    ImageDraw.Draw(im).ellipse((60, 40, 260, 200), fill=color)
    buf = io.BytesIO()
    im.save(buf, "JPEG")
    return buf.getvalue()


def check(cond: bool, msg: str) -> bool:
    print(("ok   " if cond else "FAIL ") + msg)
    return cond


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://localhost:8000")
    url = ap.parse_args().url
    colors = ["red", "blue", "green"]
    ok = True

    img = infer(url, "clip_image", "INPUT_IMAGE", [circle(c) for c in colors])
    txt = infer(url, "clip_text", "INPUT_TEXT", [f"a {c} circle".encode() for c in colors])
    bge = infer(
        url,
        "bge_text",
        "INPUT_TEXT",
        [b"red leather handbag", b"crimson leather purse", b"stainless steel water bottle"],
    )
    for name, v, dim in (("clip_image", img, 512), ("clip_text", txt, 512), ("bge_text", bge, 384)):
        ok &= check(v.shape == (3, dim), f"{name} shape {v.shape}")
        ok &= check(np.allclose(np.linalg.norm(v, axis=1), 1, atol=1e-3), f"{name} L2-normalized")
    ok &= check(
        list((img @ txt.T).argmax(1)) == [0, 1, 2], "clip text->image matches each circle's color"
    )
    ok &= check(
        float(bge[0] @ bge[1]) > float(bge[0] @ bge[2]), "bge: handbag~purse > handbag~bottle"
    )
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
