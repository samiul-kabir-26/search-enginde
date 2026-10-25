"""Tokenize UTF-8 text, padding to the longest item in the request and truncating at max_length.

Shared by clip_text_pre and bge_text_pre; the tokenizer comes from ./assets and max_length from
the model config.
"""

import json
from pathlib import Path

import numpy as np
import triton_python_backend_utils as pb_utils
from transformers import AutoTokenizer


class TritonPythonModel:
    def initialize(self, args):
        assets = Path(args["model_repository"]) / args["model_version"] / "assets"
        self.tokenizer = AutoTokenizer.from_pretrained(assets)
        params = json.loads(args["model_config"])["parameters"]
        self.max_length = int(params["max_length"]["string_value"])

    def execute(self, requests):
        responses = []
        for request in requests:
            raw = pb_utils.get_input_tensor_by_name(request, "INPUT_TEXT").as_numpy().reshape(-1)
            try:
                texts = [b.decode("utf-8") for b in raw]
            except UnicodeDecodeError as e:
                err = pb_utils.TritonError(f"invalid UTF-8: {e}", pb_utils.TritonError.INVALID_ARG)
                responses.append(pb_utils.InferenceResponse(error=err))
                continue
            enc = self.tokenizer(
                texts,
                padding=True,
                truncation=True,
                max_length=self.max_length,
                return_tensors="np",
            )
            outputs = [
                pb_utils.Tensor("input_ids", enc["input_ids"].astype(np.int64)),
                pb_utils.Tensor("attention_mask", enc["attention_mask"].astype(np.int64)),
            ]
            responses.append(pb_utils.InferenceResponse(output_tensors=outputs))
        return responses
