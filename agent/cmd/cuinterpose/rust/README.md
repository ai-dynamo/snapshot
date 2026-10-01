<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# CuInterpose Rust components

CuInterpose reconstructs same-node CUDA VMM sharing, supported memory IPC, and multicast around native CUDA checkpoint/restore. It consists of the GNU/glibc `libcuinterpose.so` C frontend, lazily loaded Rust `libcuinterpose_core.so`, and static-musl `cuinterpose-coordinator` and `cuinterpose-launch` executables.

See the [SNEP-295](../../../../docs/proposals/295-cuinterpose/README.md) for interception, ownership, protocol, capture/restore ordering, and isolation.

## Build and test

From the repository root:

```sh
make -C agent cuinterpose-test
make -C agent cuinterpose-build
```

These targets use GCC and the digest-pinned Rust 1.95 Bookworm builder in `agent/Dockerfile`. The workspace uses edition 2024 with MSRV 1.88. The exported artifacts are in `agent/cmd/cuinterpose/build/`. Always test and ship a matched frontend/core/coordinator set. The standard-library-only launcher uses the existing musl target and preserves its executable permission in the exported bundle.

The protocol records CUDA metadata as explicit fixed-width primitive fields. CUDA FFI structs stay inside the core crate and are never serialized directly.

For local development, install GNU and musl targets and musl tools, then run `make native` or `make test-native` from `agent/cmd/cuinterpose`. On hosts whose default linker is not the system GNU toolchain, set `CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER=/usr/bin/gcc`. The C frontend requires GCC 15 or newer for `musttail`. The builder copies it from the digest-pinned `gcc:15.2.0-bookworm` image. Set `FRONTEND_CC` for local builds.

The packaged gate runs GCC warnings-as-errors, rustfmt, strict Clippy, Rust unit tests, scripted coordinator checks, and process-isolated loader/startup checks. The loader fixtures provide CUDA symbol addresses and return values; CUDA memory behavior is tested on real GPUs.

Physical-GPU tests live in `../tests/gpu`. Stage a matched artifact set with:

```sh
python3 ../tests/gpu/stage.py --help
```

The staging layout is `DEST/tests/gpu` and `DEST/build`. Run pytest and require all GPU cases to pass with zero skips. The suite calls the native CUDA checkpoint API directly. These tests cover shared/private contents, unicast import reconstruction, multicast collective/graph replay, raw-import refusal, and context teardown. The context cases require one GPU and verify malloc cleanup, surviving direct VMM, and carrier reconstruction after context destruction or reset. They do not exercise the Go agent's namespace-entry wrapper. Full Snapshot qualification additionally requires cross-node capture, restore, and post-restore workload inference.

## Module boundaries

| Crate | Responsibility |
| --- | --- |
| `abi` | Private C-layout frontend/backend tables using cudarc CUDA types |
| `protocol` | CUDA metadata, allocation references, state entries, versioned MessagePack, and FD transport |
| `core` | Driver calls, process runtimes, tracking, host carriers, lifecycle |
| `coordinator` | CLI, participants, topology validation, barriers, durable state |
| `launcher` | Preserve the resolved environment, prepend the shim, and exec the workload |

The private C ABI is version **1**. The MessagePack wire/state format, virtual shareable handle, and virtual IPC memory handle are version **1**. Earlier experimental artifacts are rejected, not translated. Rust objects, allocators, mutexes, and unwinding never cross the library boundary. Debug and release builds use `panic = "abort"` for the entire Rust workspace, including the coordinator and launcher. A panic terminates the process without stack unwinding; ordinary `Result` errors retain their normal handling. Cargo's unit test harness still uses unwinding. `FrontendAbi` contains the resolver supplied by the C frontend; `BackendAbi` contains the initialization, context, and memory callbacks supplied by the Rust backend. The frontend is outside this workspace in `../frontend`. `make frontend` uses cbindgen 0.29.4 to generate the C header from the ABI crate's explicit `repr(C)` tables, then compiles it with GCC. The pinned cbindgen CLI reads `abi/cbindgen.toml`; the generated private header includes the `cuda.h` copied from the pinned CUDA development image. Cbindgen emits only the two private tables and the ABI version, not CUDA declarations. Generated headers stay in `../build`. Rust table initialization and C static assertions check the forwarding functions against the canonical callback signatures. An ABI test compiles the generated C declarations with size, alignment, and field-offset assertions computed from Rust's layouts. Cbindgen is not linked into either shipped library or the coordinator. The frontend linker script `../frontend/libcuinterpose.ldscript` is the authoritative allowlist of workload-visible definitions; every unlisted definition is localized. The `RTLD_LOCAL` Rust backend exports only `cuinterpose_core_init`. Its glibc `dlvsym` bootstrap requires glibc 2.34 or newer. It does not override explicit `dlvsym` or implement a custom ELF loader. The backend is loaded through glibc's `$ORIGIN` using the frontend's load-time directory, so changing directory does not break sibling discovery. Manual `LD_PRELOAD` paths must be absolute to remain valid across a later exec; the launcher prepends `/tmp/snapshot-cuda/libcuinterpose.so` to the existing `LD_PRELOAD` using OS strings, retaining non-UTF-8 values and argument boundaries. It executes the supplied command directly with no persistent parent process. SnapshotJob delivery requires an explicit target-container `command`; the operator leaves its `args` and `env` unchanged. Ordinary Pods must place both libraries at `/tmp/snapshot-cuda` and preload the frontend before startup.

### CUDA bindings

The Rust ABI and core use cudarc 0.19.9's generated CUDA 13.1 types and constants directly, including `CUresult`, allocation properties, access descriptors, flags, device identifiers, and handles. The C frontend uses the matching canonical declarations from the pinned NVIDIA `cuda.h`; this project does not redefine those CUDA types or values.

Only `std`, `driver`, `cuda-13010`, and `dynamic-loading` features are enabled; the last avoids a link-time or runtime CUDA library requirement. No cudarc loader or CUDA-call wrapper is invoked: every call still uses the frontend's `FrontendAbi.resolve` callback. Its safe context/buffer owners are not used because the workload owns those resources, and checkpoint teardown/reconstruction requires explicit context and DMA cleanup. The coordinator shares cudarc's generated CUDA types through the protocol crate, but it does not load the CUDA driver or issue CUDA calls.

CUDA Runtime 11 is unsupported. Runtime driver-entry lookup wrappers require version 12.0 or newer, verified through `cudaRuntimeGetVersion` from the same library as the resolved lookup function. Older or unverifiable versions return `cudaErrorInitializationError` before calling the lookup or accessing its output and status pointers. Use one runtime library per process, visible globally or already retained through an intercepted handle lookup. Multiple runtimes and an undiscovered runtime confined to `RTLD_LOCAL` are outside the supported scope.

## Error handling

Application calls use CUDA to validate arguments and return its errors. The shim translates its virtual handles and records operations only after CUDA succeeds; it does not duplicate driver bounds, alignment, or overlap checks. Checks that protect the shim's own handle encoding, supported checkpoint contract, and checkpoint metadata remain local to those responsibilities. Unpublished resources created by an application call are cleaned up on failure. Failed cleanup or irreversible checkpoint mutation is fail-stop.

## Operating constraints

The [workload contract](../../../../docs/reference/workload-contract.md#cuinterpose-synchronization-and-lifetime) collects the application synchronization and lifetime requirements and the shim's failure behavior.

Before preparation, the agent checks the mapped frontend/core identities in every discovered CUDA participant. Coordinator `--inspect` sends the existing read-only `INSPECT` request to those participants and validates topology without entering checkpoint mode. Missing libraries or endpoints and incomplete coverage fail before mutation. Inspection does not lock the group: preparation still validates its `BEGIN_CHECKPOINT` replies, and any failure after preparation begins conservatively terminates the source.

Capture records two library SHA-256 hashes. Restore compares the exact supplied libraries before CRIU, independently of compatibility skipping; launcher and coordinator executable bytes are not part of this identity. Keep library files stable during capture. Changed shim bytes require matching artifacts or a new checkpoint; obsolete boolean manifests must be recreated.

The Go namespace command helper pins the mount namespace and executes the open host binary through an inherited descriptor. Other namespace lookups still use the target PID. Cancellation kills the helper's process group, including the forked coordinator or restore helper, and bounds waiting for inherited output pipes. Coordinator sockets use captured namespace PIDs; native CUDA restore uses the resolved process PIDs visible to its helper.

Applications must finish all CUDA calls and GPU work before `BEGIN_CHECKPOINT` and remain parked through restore. Entry closes the memory API and returns stable records under the process mutex. A process-wide counter excludes entry during unlocked driver calls; application code owns synchronization of object lifetimes. Only exactly POSIX-FD exportable VMM and multicast are supported. Unsupported exportable creations, non-POSIX multicast, and foreign imports fail at the API before creating driver state. Tracked unicast allocations support pinned DEVICE and HOST_NUMA backing; nonexportable VMM creation remains on the native path. All sharing peers must use the shim and belong to the fixed checkpoint group. One coordinator executes each phase once; failed or ambiguous phases are never retried, rolled back, or resumed. Per-object progress flags are unnecessary because a mutation failure terminates the process. Never-shared allocations remain native-owned even when exportable.

The original creator must retain a generic allocation handle or local mapping while any exported descriptor or imported allocation remains usable. Releasing a handle while retaining its mapping is supported. A virtual shareable FD stores identity and does not independently retain creator backing; imported allocations do not acquire remote ownership leases. FD-only creator lifetime is therefore outside the supported contract. Preflight checks tracked imports, not arbitrary application FDs, and cannot detect every remaining FD-only token.

Successful intercepted `cuInit` starts the shim; function lookup creates no workers or endpoint. Fork before CUDA initialization allows each child to initialize independently. After CUDA initialization, children must exec or exit; CUDA initialization errors propagate, and shim memory calls reject inherited runtime state before taking any lock. Shim-owned FDs are close-on-exec. Long-lived fork children during checkpoint are outside this contract. Failed destructive capture or reconstruction cannot safely resume the application. Unknown asynchronous-copy completion is fail-stop. Host carriers are the only shim storage implementation. They save shared creator bytes; private allocations remain native CUDA state. DEVICE backing uses asynchronous CUDA copies, while HOST_NUMA backing uses CPU copies through a temporary host-accessible VMM alias of the full allocation. Restore recreates the original allocation properties, including NUMA placement, before reconnecting importers. There is no PageBroker client, backend selection, or save-all mode in the shim.

The arena retains one full copy of each shared creator allocation through capture and reconstruction; its size is their total backing size, not their mapped ranges. Application mappings can be absent, partial, or read-only, so save and load use temporary aliases of the full backing before restoring application mappings and permissions. The current implementation registers the whole arena for each transfer, uses one stream per device/context group, and copies HOST_NUMA allocations serially. Large-arena registration, copy, and CRIU restore costs need workload measurements before changing this strategy. A smaller pinned staging buffer could reduce pinned memory, but the complete captured copy must still exist; memfd backing alone does not remove that storage requirement.

The memory-IPC adapter implements synchronous malloc, IPC export/open/close, free, and address-range lookup through tracked VMM. It does not call native memory IPC. It requires fully interposed peers; foreign native handles are rejected. Converted mallocs grant access only to the allocating device. Native `cuCtxEnablePeerAccess` does not update VMM permissions, so same-process peer access to these pointers is unsupported; use one GPU per process with supported IPC sharing. Each converted malloc consumes backing rounded up to the device's minimum VMM allocation granularity, which can increase memory use for small allocations. Event IPC, pool IPC, and managed/async/pitched allocation families are outside the supported scope.

Successful context destruction, primary-context reset, and final primary-context release reclaim that context's malloc and imported IPC mappings. A nonfinal primary release and failed teardown leave those allocations intact. Explicit VMM allocations survive; their cached operational context is cleared so later carrier work can use a primary context. DEVICE backing uses its device ordinal for this fallback; HOST_NUMA backing uses CUDA device zero without changing the allocation's NUMA node ID. Applications must synchronize context lifetime changes against other uses of that context. Carrier memory is registered only for a save/load transfer and unregistered before its registration context is released.

Multicast membership uses `(namespace PID, local device ordinal)`. Each participant must add and bind its device in the same process. Different ranks may each use local device zero; adding a device in one process and binding it from another is outside the supported scope.
