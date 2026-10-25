"""Decode encoded images and apply CLIPImageProcessor (resize, center crop, normalize)."""

import io
from pathlib import Path

import numpy as np
import triton_python_backend_utils as pb_utils
from PIL import Image
from transformers import CLIPImageProcessor

# Matches the API-side limit (docs/API.md): anything larger is rejected instead of decoded.
Image.MAX_IMAGE_PIXELS = 4096 * 4096


class TritonPythonModel:
    def initialize(self, args):
        assets = Path(args["model_repository"]) / args["model_version"] / "assets"
        self.processor = CLIPImageProcessor.from_pretrained(assets)

    def execute(self, requests):
        responses = []
        for request in requests:
            raw = pb_utils.get_input_tensor_by_name(request, "INPUT_IMAGE").as_numpy().reshape(-1)
            try:
                images = [Image.open(io.BytesIO(b)).convert("RGB") for b in raw]
            except (OSError, ValueError, Image.DecompressionBombError) as e:
                err = pb_utils.TritonError(f"invalid image: {e}", pb_utils.TritonError.INVALID_ARG)
                responses.append(pb_utils.InferenceResponse(error=err))
                continue
            pixels = self.processor(images=images, return_tensors="np")["pixel_values"]
            out = pb_utils.Tensor("pixel_values", pixels.astype(np.float32))
            responses.append(pb_utils.InferenceResponse(output_tensors=[out]))
        return responses
