<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# CUDA checkpoint helper

`cuda-checkpoint-helper` is the long-running daemon used by the snapshot agent
for CUDA checkpoint and restore operations. When CUDA CustomStorage is
available, it transfers checkpoint extents through the NIXL POSIX backend.

The production transfer path supports deterministic file sharding and a
bounded ring of pinned transfer buffers. Buffer count and chunk size are
validated per device and per operation before workers start.

Successful CustomStorage operations emit a `cuda_custom_storage_transfer` JSON
payload. Its timing fields cover:

- storage directory and manifest validation
- CUDA initialization, device enumeration, and primary-context management
- the synchronous CUDA checkpoint or restore API call
- transfer-job construction and worker orchestration
- post-transfer validation and operation completion

Totals contain subphases and must not be added to them. Worker service times
are sums across workers and can overlap in wall time; the `timing_scope` field
records these rules.

The helper resolves CustomStorage capability once at daemon startup. It retains
primary contexts only for the current target GPU set and reuses them across
adjacent per-process calls in the same process-tree checkpoint or restore. The
cache is replaced when a different target set arrives and released after 60
seconds idle or at daemon shutdown. This keeps unrelated GPUs untouched while
avoiding context teardown between restore and unlock. Transfer telemetry marks
the context as cached; cache-release events report the final release status.

## Running

The Snapshot agent image runs the helper as a privileged sidecar and shares its
Unix socket at `/run/cuda-checkpoint-helper/helper.sock` with the agent. Start a
standalone daemon only on a Linux CUDA host with the required host process and
checkpoint mounts:

```sh
cuda-checkpoint-helper --daemon \
  --socket /run/cuda-checkpoint-helper/helper.sock \
  --max-operation-seconds 21600
```

Check readiness with:

```sh
cuda-checkpoint-helper --health \
  --socket /run/cuda-checkpoint-helper/helper.sock
```

Production checkpoint and restore requests are sent by the Snapshot agent over
that socket; the helper CLI is not a user-facing checkpoint interface.
