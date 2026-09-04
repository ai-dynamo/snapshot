<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# cuinterpose

cuinterpose is the CUDA interposer: a library that a workload loads through
`LD_PRELOAD` so Snapshot can checkpoint and restore CUDA memory that is shared
between processes on one node, plus a small coordinator the snapshot agent runs
at checkpoint and restore time.

## Using it

1. Install Snapshot with the chart; the operator learns the agent image from
   `image.agent` and, for private registries, the pull secrets from
   `cuinterpose.imagePullSecrets`.
2. Opt a source Pod in with the annotation `nvidia.com/cuinterpose: enabled`
   and give its container an explicit `command`. A `SnapshotJob` shapes the Pod
   itself; for a `PodSnapshot` of a Deployment you manage, shape the template
   with `podcontract.ShapeCuinterposeCapture` (init container copying the shim
   out of the agent image, `LD_PRELOAD`, the `cuda-checkpoint --launch-job`
   wrapper).
3. Keep the workload's allocators on POSIX file-descriptor handles; fabric
   handles pass through untracked (see `docs/limitations.md`).
4. Checkpoint and restore as usual. The agent's log carries one line per
   coordinator phase, for example:

```
cuinterpose-coordinator phase=save_host_carrier status=ok elapsed_ms=339.7 participants=4 carrier_count=382 carrier_bytes=2076180480 gb_per_s=6.10 copy_gb_per_s=82.54
```

Knobs, read by the shim and the coordinator: `SNAPSHOT_CONTROL_DIR` (default
`/snapshot-control`), `SNAPSHOT_PARTICIPANT_ID` (tests only),
`SNAPSHOT_CONTROL_TIMEOUT_SECONDS` (default 10) for ordinary exchanges, and
`SNAPSHOT_CARRIER_TIMEOUT_SECONDS` (default 3600) for the two phases that copy
allocation contents.

The design, the flows as sequence diagrams, the invariants, the pitfalls that
shaped it, and what is explicitly unsupported are in
[`docs/reference/cuinterpose.md`](../../../docs/reference/cuinterpose.md).

## Build

```bash
make -C agent/cmd/cuinterpose CUDA_HOME=/usr/local/cuda-13.1
```

Only headers are used from `CUDA_HOME`; the shim never links libcuda. Every C
file in the directory is part of the shim except `coordinator.c`, which is the
coordinator. The build asserts, on the produced `libcuinterpose.so`:

- no `NEEDED` entry for libcuda or libcudart;
- no glibc symbol newer than `GLIBC_2.34` (the shim must load into Ubuntu
  22.04 images);
- the real `dlsym` is obtained through `dlvsym(..., "GLIBC_2.34")`;
- the exported symbol set is exactly the CUDA entry points it replaces,
  `dlsym`, and `cuinterpose_*` (everything else is hidden, because an
  `LD_PRELOAD` library is first in the symbol search order of the whole
  process);
- the CUDA headers are 13.1 or newer (`ALLOW_OLD_CUDA_HEADERS=1` overrides).

`cuinterpose_build_info` records the CUDA header version the loaded shim was
compiled against; `nm -D libcuinterpose.so` shows it.

The coordinator is statically linked so it can run inside the restored
container's mount namespace regardless of that image's C library.

## Tests

```bash
make -C agent/cmd/cuinterpose CUDA_HOME=/usr/local/cuda-13.1 test
```

GoogleTest suites, no GPU needed, found by file name under `tests/`:
`<name>_unit_test.cc` links the shim's CUDA-free objects and runs on its own;
`<name>_preload_test.cc` runs with a sanitizer-instrumented build of the shim
`LD_PRELOAD`ed over a fake `libcuda.so.1` / `libcudart.so.13` that record what
they were called with; `coordinator_test.cc` runs the sanitizer-instrumented
coordinator against fake participants. All are built with AddressSanitizer and
UndefinedBehaviorSanitizer (`SANITIZE=` disables). The agent image build runs
them; a failing test fails the image. GPU tests live under `tests/gpu`.
