// Package onnx scores candidates in-process with ONNX Runtime (onnxruntime_go): one session per
// (tenant, config version), intra-op threads limited, FEATURES FP32 [N, F] in and SCORES FP32
// [N] out (docs/MODELS.md §1). Init loads the shared library once per process.
package onnx
