<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Cuinterpose Rust core and coordinator

Cuinterpose reconstructs same-node CUDA VMM sharing, including multicast,
around native CUDA checkpoint/restore. Production consists of the GNU/glibc
`libcuinterpose.so` C frontend, lazily loaded Rust `libcuinterpose_core.so`, and a
static-musl `cuinterpose-coordinator`.

See the [design](../../../../docs/development/cuinterpose.md) for interception,
ownership, protocol, capture/restore ordering, and isolation.

## Build and test

From the repository root:

```sh
make -C agent cuinterpose-test
make -C agent cuinterpose-build
```

These targets use GCC and the digest-pinned Rust 1.95 Bookworm builder in
`agent/Dockerfile`. The workspace uses edition 2024 with MSRV 1.88.
The exported artifacts are in `agent/cmd/cuinterpose/build/`.
Verification checks ELF exports, GNU libraries' glibc baseline, lack of CUDA
link dependencies, static coordinator linkage, permissions, and CLI options.
Always test and ship a matched frontend/core/coordinator set.

For local development, install GNU and musl targets and musl tools, then run
`make native` or `make test-native` from `agent/cmd/cuinterpose`. On hosts
whose default linker is not the system GNU toolchain, set
`CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER=/usr/bin/gcc`.

The packaged gate runs GCC warnings-as-errors, rustfmt, strict Clippy, Rust
unit/contract tests, and process-isolated headless tests. Small C loader probes and a stateful fake CUDA
provider are test-only; they do not require CUDA headers, C++/gtest, or Git
history. The fake models ownership and calls, not physical device bytes.
Its default behavior is stateful; frontend forwarding tests have a separate
minimal provider.

Physical-GPU tests live in `../tests/gpu`. Stage a matched build and the real
NVIDIA checkpoint tool:

```sh
python3 core/tests/stage_gpu.py --help
```

The staging layout is `DEST/tests/gpu`, `DEST/build`, and
`DEST/bin/cuda-checkpoint`. Launch pytest through
`cuda-checkpoint --launch-job`; require all GPU cases to pass with zero skips.
These tests cover shared/private contents, unicast import reconstruction,
multicast collective/graph replay, and raw-import refusal. They do not exercise
the Go agent's namespace-entry wrapper. Full Snapshot qualification additionally
requires cross-node capture, restore, and post-restore workload inference.

## Module boundaries

| Crate | Responsibility |
| --- | --- |
| `abi` | Private C-layout host/core tables and cudarc-backed CUDA definitions |
| `protocol` | Typed identities, tickets, records, bounded MessagePack and FD transport |
| `core` | Driver calls, process generations, tracking, host carriers, lifecycle |
| `coordinator` | CLI, participants, topology validation, barriers, durable state |

The private C ABI is version **5**; the wire/state/ticket format is version
**4**. Earlier experimental artifacts are rejected, not translated. Rust
objects, allocators, mutexes, and unwinding never cross the library boundary.
The retained diagnostic C callback supports startup/poison/fork tests where
control inspection is unavailable; it is not a second lifecycle interface.
The frontend is outside this workspace in `../frontend`. `make frontend`
uses cbindgen 0.29.4 to generate the C header from the ABI crate's explicit
`repr(C)` tables, then compiles it with GCC. The build-only example invokes
cbindgen with `abi/cbindgen.toml`; no macro expansion, nightly compiler, or CUDA
SDK is needed. Generated headers stay in `../build`.
Rust table initialization and C static assertions check the forwarding
functions against the canonical callback signatures. An ABI test compiles
the generated C declarations with size, alignment, and field-offset assertions
computed from Rust's layouts. Cbindgen is not linked into either shipped library
or the coordinator.
Its glibc `dlvsym` bootstrap requires glibc 2.34 or newer. It does not preserve
arbitrary interposers' original-caller `RTLD_NEXT` scopes, override explicit
`dlvsym`, or implement a custom ELF loader.

### CUDA bindings

The ABI crate uses cudarc 0.19.9's generated CUDA 13.1 types and constants,
including allocation flags, multicast properties, device identifiers and
handles. Enum-bearing allocation/access/location structs retain integer fields
at the C boundary so unknown values remain valid inputs for the driver; their
size, alignment, and every field offset are checked against cudarc at compile
time. Driver errors likewise remain integer codes rather than Rust enum values.

Only `std`, `driver`, `cuda-13010`, and `dynamic-loading` features are enabled;
the last avoids a link-time CUDA requirement. No cudarc loader or CUDA-call
wrapper is invoked: every call still uses the frontend's `Host.resolve`
callback. Its safe context/buffer owners are not used because the workload owns
those resources, and checkpoint teardown/reconstruction requires explicit
context and DMA cleanup. The coordinator does not depend on cudarc.

## Operating constraints

Applications must be quiescent during prepare and remain parked through restore.
Only exactly POSIX-FD exportable VMM is supported. Unsupported exportable
creations and live raw imports are refused at checkpoint inspection.
Never-shared allocations remain native-owned even when exportable.

Fork resets the child's shim generation and closes inherited shim-owned
descriptors; it does not make CUDA use after arbitrary multithreaded fork safe.
Prefer spawn/exec. Failed destructive capture or reconstruction cannot safely
resume the application. Unknown asynchronous-copy completion is fail-stop.
FABRIC, PageBroker GPU content transfer, save-all mode, and backend selection
are not implemented here.
