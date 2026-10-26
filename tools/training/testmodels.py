"""Writes the small ONNX models the Go scorer tests load (internal/providers/scorer/onnx/testdata).

    uv run python -m training.testmodels

- linear3.onnx: SCORES = FEATURES · [1, 2, 3]
- gbdt4.onnx + gbdt4.expected.json: a tiny LightGBM lambdarank model (4 features) exported like
  the real ones, with inputs and the scores LightGBM gives them, for a Go parity test
- bad_input.onnx: a model whose input is not named FEATURES
"""

from __future__ import annotations

import json
from pathlib import Path

import lightgbm as lgb
import numpy as np
import onnx
from onnx import TensorProto, helper

from training import export

OUT = Path(__file__).resolve().parents[2] / "internal/providers/scorer/onnx/testdata"


def tiny_gbdt() -> tuple[lgb.Booster, np.ndarray]:
    rng = np.random.default_rng(7)
    x = rng.random((400, 4))
    y = np.clip(np.round(3 * x[:, 0] * x[:, 1] + rng.normal(0, 0.2, 400)), 0, 3).astype(int)
    params = {
        "objective": "lambdarank",
        "num_leaves": 7,
        "learning_rate": 0.2,
        "min_data_in_leaf": 5,
        "verbose": -1,
        "deterministic": True,
        "seed": 7,
        "num_threads": 1,
    }
    booster = lgb.train(params, lgb.Dataset(x, y, group=[20] * 20), num_boost_round=10)
    return booster, rng.random((6, 4))


def main() -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    onnx.save(export.linear([1.0, 2.0, 3.0], "linear3"), OUT / "linear3.onnx")

    booster, probe = tiny_gbdt()
    model = export.to_onnx(booster, "gbdt4")
    export.check_parity(booster, model, probe)
    onnx.save(model, OUT / "gbdt4.onnx")
    expected = {
        "inputs": probe.astype(np.float32).tolist(),
        "scores": booster.predict(probe).tolist(),
    }
    (OUT / "gbdt4.expected.json").write_text(json.dumps(expected, indent=1) + "\n")

    bad = export.linear([1.0], "bad")
    bad.graph.input[0].name = "X"
    bad.graph.node[0].input[0] = "X"
    bad.graph.input[0].type.CopyFrom(helper.make_tensor_type_proto(TensorProto.FLOAT, [None, 1]))
    onnx.save(bad, OUT / "bad_input.onnx")
    print(f"wrote test models to {OUT}")


if __name__ == "__main__":
    main()
