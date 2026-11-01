<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Use Your Own Framework or Image

The recipes show three engines, but Snapshot works with any inference
engine whose workload meets the
[Workload Contract](workload-contract.md): how it prepares for a
checkpoint, how it resumes after a restore, and what its pod must carry.

- To package an engine the way the supported frameworks do, with the runtime image
  unmodified and a small program mounted from a ConfigMap, or to bake the
  program into your own image, see
  [Packaging Methods](workload-contract.md#packaging-methods).
- To add support for an engine that is not covered here, follow
  [Support a New Inference Server](workload-contract.md#support-a-new-inference-server).
  The [supported frameworks](../guides/supported-frameworks/vllm.md) are worked examples of the same contract.

See [Installation](../get-started/installation.md) for installing and uninstalling
Snapshot and the [API Reference](../reference/api.md) for full resource detail.
