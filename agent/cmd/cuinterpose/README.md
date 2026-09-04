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

Only headers are used from `CUDA_HOME`; the shim never links libcuda. The build
asserts, on the produced `libcuinterpose.so`:

- no `NEEDED` entry for libcuda or libcudart;
- no glibc symbol newer than `GLIBC_2.34` (the shim must load into Ubuntu
  22.04 images);
- the real `dlsym` is obtained through `dlvsym(..., "GLIBC_2.34")`;
- the exported symbol set is exactly the CUDA entry points it replaces,
  `dlsym`, and `cuinterpose_build_info` (everything else is hidden, because an
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

GoogleTest suites, no GPU needed, found by file name under `tests/` (`<name>_unit_test.cc` links the shim's CUDA-free objects; `<name>_preload_test.cc` runs with the sanitized shim `LD_PRELOAD`ed over the fake driver; `coordinator_test.cc` drives the sanitized coordinator):

- `proto_unit_test`: the ticket format and control-socket helpers (`util.c`,
  `posix.c`), including descriptor passing and the paths that must not leak a
  descriptor on a malformed message, and the export cache, including a drop
  racing in-flight requests; `table_unit_test`: the tables and range index.
- `state_preload_test`: allocation tracking end to end against the fake driver's
  tracked mode: alias collapse, range unmap, access merging, tickets, imports
  in the same process and from a forked child through the listener, raw import
  counting, a churn test that leaves the tables empty, and descriptor
  exhaustion.
- `forward_preload_test`: runs with `LD_PRELOAD` of a sanitizer-instrumented shim
  against fake `libcuda.so.1` / `libcudart.so.13` that record their arguments,
  and checks all four resolution paths above.
- `coordinator_test`: runs the coordinator binary against fake participants
  (small servers speaking the control protocol) and checks phase ordering, the
  multicast phase barrier, topology validation, refusal on live raw imports,
  the state file format, and every restore precondition.
- `lifecycle_preload_test`: the real coordinator drives this process and a forked
  importer child through prepare and restore over the fake driver: host
  carriers for exported and never-exported allocations, the re-pin after a lost
  host registration, the refusal while a raw import is alive, allocations
  created before any CUDA context existed, and a failed host copy leaving the
  workload untouched.
- `multicast_preload_test`: the same two-process flow for a multicast group (bind by
  handle in one rank, by address with the device-explicit ABI in the other),
  the effective extent beyond the requested size, unbinding and rebinding by
  address, pass-through of non-POSIX objects, refusal of untracked members,
  and the order in which PREPARE_MULTICAST closes the cached descriptor and
  releases the object.

All are built with AddressSanitizer and UndefinedBehaviorSanitizer
(`SANITIZE=` disables). The agent image build runs them; a failing test fails
the image. The test helpers never shell out: a child of a sanitized process
inherits its `LD_PRELOAD`, and the base image's `/bin/sh` runs a profile script
whose helpers would then report leaks of their own.

### GPU tests

`tests/gpu/` holds pytest tests that need two GPUs, a CUDA 13 driver with
checkpoint support, and `cuda-checkpoint`:

```bash
cuda-checkpoint --launch-job uv run --project agent/cmd/cuinterpose/tests/gpu \
    pytest agent/cmd/cuinterpose/tests/gpu -v -m gpu
```

The test process starts one interposed parent that forks two PyTorch workers.
Each creates POSIX-shareable allocations of its own (one of them before any
CUDA context exists, one large one filled with seeded random bytes), imports a
descriptor from the uninterposed test process, shares a symmetric-memory buffer
with the other rank, and captures a collective into a CUDA graph. The test then
runs the coordinator, the native checkpoint sequence through `cuda.bindings`,
and the coordinator again, and checks: the state file exists and nothing else
was written; the coordinator's summary line reports at least the workers' own
allocations as carried; the host-to-device copy on restore reached 80% of a
plain pinned copy measured on the same machine
(`CUINTERPOSE_TEST_MIN_H2D_FRACTION`); every byte survived; the graph replays
with the right result; a fresh raw import works. A second test holds a raw
import across `--prepare` and checks the refusal names it and that the workload
keeps working. `test_multicast.py` (marker `multicast`, skipped without NVLink)
runs the same flow with PyTorch's multimem all-reduce, one rank rebinding its
slice with `cuMulticastBindAddr`. `CUINTERPOSE_BUILD_DIR` points the tests at prebuilt binaries
(for example copied out of the agent image); otherwise they build the shim with
`CUDA_HOME`. The seed is printed and can be replayed with `CUINTERPOSE_TEST_SEED`.
