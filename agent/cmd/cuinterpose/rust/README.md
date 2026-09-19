<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Cuinterpose Rust core and coordinator

Cuinterpose reconstructs same-node CUDA VMM sharing, supported memory IPC, and multicast
around native CUDA checkpoint/restore. It consists of the GNU/glibc
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

The protocol records CUDA metadata as explicit fixed-width primitive fields.
CUDA FFI structs stay inside the core crate and are never serialized directly.

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
`DEST/bin/cuda-checkpoint`. Run pytest without a launch-job wrapper or
`CUDA_CHECKPOINT_JOB_FILE`; require all GPU cases to pass with zero skips.
These tests cover shared/private contents, unicast import reconstruction,
multicast collective/graph replay, and raw-import refusal. They do not exercise
the Go agent's namespace-entry wrapper. Full Snapshot qualification additionally
requires cross-node capture, restore, and post-restore workload inference.

## Module boundaries

| Crate | Responsibility |
| --- | --- |
| `abi` | Private C-layout frontend/backend tables using cudarc CUDA types |
| `protocol` | CUDA metadata, allocation references, state entries, versioned MessagePack, and FD transport |
| `core` | Driver calls, process generations, tracking, host carriers, lifecycle |
| `coordinator` | CLI, participants, topology validation, barriers, durable state |

The private C ABI, MessagePack wire/state format, virtual shareable handle, and
virtual IPC memory handle are all version **1**.
Earlier experimental artifacts are rejected, not translated. Rust
objects, allocators, mutexes, and unwinding never cross the library boundary.
`FrontendAbi` contains the resolver and process identity supplied by the C
frontend; `BackendAbi` contains the lifecycle and CUDA callbacks supplied by
the Rust backend.
The frontend is outside this workspace in `../frontend`. `make frontend`
uses cbindgen 0.29.4 to generate the C header from the ABI crate's explicit
`repr(C)` tables, then compiles it with GCC. The pinned cbindgen CLI reads
`abi/cbindgen.toml`; the generated private header includes the `cuda.h` copied
from the pinned CUDA development image. Cbindgen emits only the two private
tables and the ABI version, not CUDA declarations. Generated headers stay in
`../build`.
Rust table initialization and C static assertions check the forwarding
functions against the canonical callback signatures. An ABI test compiles
the generated C declarations with size, alignment, and field-offset assertions
computed from Rust's layouts. Cbindgen is not linked into either shipped library
or the coordinator.
The frontend linker script `../frontend/libcuinterpose.ldscript` is the
authoritative allowlist of workload-visible definitions; every unlisted
definition is localized, and artifact verification compares the final dynamic
symbol table with that list. The `RTLD_LOCAL` Rust backend exports only
`cuinterpose_core_init`.
Its glibc `dlvsym` bootstrap requires glibc 2.34 or newer. It does not preserve
arbitrary interposers' original-caller `RTLD_NEXT` scopes, override explicit
`dlvsym`, or implement a custom ELF loader.

### CUDA bindings

The Rust ABI and core use cudarc 0.19.9's generated CUDA 13.1 types and
constants directly, including `CUresult`, allocation properties, access
descriptors, flags, device identifiers, and handles. The C frontend uses the
matching canonical declarations from the pinned NVIDIA `cuda.h`; this project
does not redefine those CUDA types or values.

Only `std`, `driver`, `cuda-13010`, and `dynamic-loading` features are enabled;
the last avoids a link-time or runtime CUDA library requirement. No cudarc loader or CUDA-call
wrapper is invoked: every call still uses the frontend's `FrontendAbi.resolve`
callback. Its safe context/buffer owners are not used because the workload owns
those resources, and checkpoint teardown/reconstruction requires explicit
context and DMA cleanup. The coordinator shares cudarc's generated CUDA types
through the protocol crate, but it does not load the CUDA driver or issue CUDA
calls.

## Operating constraints

Applications must be quiescent during prepare and remain parked through restore.
Only exactly POSIX-FD exportable VMM is supported. Unsupported exportable
creations and live raw imports are refused at checkpoint inspection.
Never-shared allocations remain native-owned even when exportable.

Fork resets the child's shim generation and closes inherited shim-owned
descriptors; it does not make CUDA use after arbitrary multithreaded fork safe.
Prefer spawn/exec. Failed destructive capture or reconstruction cannot safely
resume the application. Unknown asynchronous-copy completion is fail-stop.
Host carriers are the only shim storage implementation. They save shared creator
bytes; private allocations remain native CUDA state. There is no PageBroker
client, backend selection, or save-all mode in the shim.

The memory-IPC adapter implements synchronous malloc, IPC export/open/close,
free, and address-range lookup through tracked VMM. It does not call native
memory IPC. It requires fully interposed peers; foreign native handles are
rejected. Event IPC, pool IPC, managed/async/pitched allocation families, and
general cross-context peer-access emulation are outside its supported scope.
