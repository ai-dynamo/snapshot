<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Use Your Own Engine or Image

Snapshot is not limited to vLLM, SGLang and TensorRT-LLM. It works with any
inference engine whose workload meets the
[Workload Contract](../../reference/workload-contract.md), which covers:

- How the workload prepares for a checkpoint
- How it resumes after a restore
- What its pod must carry

Where to go next:

- To package your engine the way the pages in this section do, see
  [Packaging Methods](../../reference/workload-contract.md#packaging-methods).
  You can keep the runtime image unmodified and mount a small program from a
  ConfigMap, or bake the program into your own image.
- To checkpoint an engine that runs from its stock image and command, with no
  program of yours in the container, see
  [Lifecycle Hooks](../../reference/workload-contract.md#lifecycle-hooks). The
  engine's own signals, such as a health endpoint, tell Snapshot when to
  checkpoint.
- To add support for an engine that this section does not cover, see
  [Support a New Inference Server](../../reference/workload-contract.md#support-a-new-inference-server).
  The [vLLM](vllm.md), [SGLang](sglang.md) and
  [TensorRT-LLM](tensorrt-llm.md) pages are worked examples of the same contract.
