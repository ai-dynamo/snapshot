<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Use your own engine or image

The supported frameworks are vLLM, SGLang, and TensorRT-LLM, but Snapshot works
with any inference engine whose workload meets the
[workload contract](../../reference/workload-contract.md): how it prepares for a
checkpoint, how it resumes after a restore, and what its pod must carry.

- To package an engine the way these guides do, with the runtime image
  unmodified and a small program mounted from a ConfigMap, or to bake the
  program into your own image, see
  [Packaging methods](../../reference/workload-contract.md#packaging-methods).
- To add support for an engine that is not covered here, follow
  [Support a new inference server](../../reference/workload-contract.md#support-a-new-inference-server).
  The [engine guides](vllm.md) are worked examples of the same contract.
