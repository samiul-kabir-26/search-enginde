// Package kservev2 scores candidates through a KServe v2 model: the platform's Triton (rank
// models such as rank-gbdt-v1, served by its ONNX backend) or a tenant's endpoint over gRPC or
// REST. Tensors follow docs/MODELS.md §1: FEATURES FP32 [N, F] in, SCORES FP32 [N] out.
package kservev2
