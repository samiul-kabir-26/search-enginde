"""Export LightGBM rankers to ONNX with the scorer tensor contract (docs/MODELS.md §1).

The model takes `FEATURES` FP32 [N, F] and returns `SCORES` FP32 [N]. onnxmltools emits an
[N, 1] output named `variable`; a Reshape renames and flattens it.
"""

from __future__ import annotations

import lightgbm as lgb
import numpy as np
import onnx
import onnxruntime as ort
from onnx import TensorProto, helper
from onnxmltools import convert_lightgbm
from onnxmltools.convert.common.data_types import FloatTensorType

INPUT = "FEATURES"
OUTPUT = "SCORES"
OPSET = 15  # the highest opset onnxmltools 1.16 converts LightGBM to


def to_onnx(booster: lgb.Booster, name: str) -> onnx.ModelProto:
    """Converts a trained booster; the result scores like booster.predict (parity-checked)."""
    n = booster.num_feature()
    model = convert_lightgbm(
        booster,
        name=name,
        initial_types=[(INPUT, FloatTensorType([None, n]))],
        target_opset=OPSET,
        zipmap=False,
    )
    return flatten_output(model)


def flatten_output(model: onnx.ModelProto) -> onnx.ModelProto:
    """Reshapes the single [N, 1] output to SCORES [N]."""
    graph = model.graph
    if len(graph.output) != 1:
        raise ValueError(f"expected one output, got {[o.name for o in graph.output]}")
    raw = graph.output[0].name
    shape = helper.make_tensor("scores_shape", TensorProto.INT64, [1], [-1])
    graph.initializer.append(shape)
    graph.node.append(
        helper.make_node("Reshape", [raw, "scores_shape"], [OUTPUT], name="to_scores")
    )
    del graph.output[:]
    graph.output.append(helper.make_tensor_value_info(OUTPUT, TensorProto.FLOAT, [None]))
    onnx.checker.check_model(model)
    return model


def check_parity(booster: lgb.Booster, model: onnx.ModelProto, x: np.ndarray, tol=1e-4) -> float:
    """Max absolute difference between LightGBM and ONNX Runtime scores on x; raises above tol."""
    sess = ort.InferenceSession(model.SerializeToString(), providers=["CPUExecutionProvider"])
    got = sess.run([OUTPUT], {INPUT: x.astype(np.float32)})[0]
    want = booster.predict(x)
    diff = float(np.max(np.abs(got - want))) if len(x) else 0.0
    if diff > tol:
        raise AssertionError(f"ONNX parity {diff:.2e} exceeds {tol:.0e}")
    return diff


def linear(weights: list[float], name: str = "linear") -> onnx.ModelProto:
    """A linear scorer (SCORES = FEATURES · weights), for tests of the scorer runtimes."""
    w = helper.make_tensor("w", TensorProto.FLOAT, [len(weights), 1], weights)
    shape = helper.make_tensor("scores_shape", TensorProto.INT64, [1], [-1])
    graph = helper.make_graph(
        [
            helper.make_node("MatMul", [INPUT, "w"], ["y"]),
            helper.make_node("Reshape", ["y", "scores_shape"], [OUTPUT]),
        ],
        name,
        [helper.make_tensor_value_info(INPUT, TensorProto.FLOAT, [None, len(weights)])],
        [helper.make_tensor_value_info(OUTPUT, TensorProto.FLOAT, [None])],
        [w, shape],
    )
    model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", OPSET)])
    model.ir_version = 8
    onnx.checker.check_model(model)
    return model
