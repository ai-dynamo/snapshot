<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Rust cuinterpose experiment

This workspace is an incomplete, main-based port. It builds a run-ai-style
`libcuinterpose.so` front end, a separate `libcuinterpose_core.so`, and
`cuinterpose-coordinator`. Unicast bookkeeping and host-carrier code exist;
multicast reconstruction is not implemented. Fork generation reset is covered
by fake-driver tests, not qualified for real post-CUDA fork. Do not treat
a successful build or loader test as GPU, CRIU, or vLLM qualification.

## Implementation boundaries

| Crate | Owns |
| --- | --- |
| `frontend` | CUDA/resolver exports, checked allocation-free ELF bootstrap, caller-relative lookup, provider retention, and lazy core loading |
| `abi` | C layouts and the single `memory_api!` signature inventory generating wrappers and the typed core table |
| `core` | Allocation state, CUDA operations, host carriers, control listener, sealed-ticket transport, and leased peer-export descriptors |
| `protocol` | C v2 binary layouts, named records and tickets, checked codecs, socket framing, and descriptor transport |
| `coordinator` | Participant discovery, named-field topology validation, phase barriers, and state-file publication |

The private host/core ABI is version 3, checked by version and size.
`cuinterpose_core_init` is the core's only dynamic export. It returns a
`repr(C)` table of typed C function pointers; memory calls do not look up
untyped core functions by name. The host's real-symbol resolver remains a
C-ABI callback because ELF lookup produces untyped addresses. The front end
loads the sibling core with `RTLD_LAZY | RTLD_LOCAL` on the first memory call.
The sibling libraries are trusted code: a matching version and size promises
valid non-null callbacks with the declared signatures. Prefix checks reject
incompatible tables; they do not validate arbitrary foreign function pointers.

The independently versioned C wire format remains v2: 256-byte headers,
256-byte tickets, and 688-byte records. Numeric layout belongs only in
`protocol`, not allocation or topology logic. Record ordering compares the
encoded bytes, preserving the C implementation's canonical order. Record
reserved bytes round-trip unchanged. Header readers ignore reserved input;
ticket readers reject nonzero reserved arrays and require the multicast-only
metadata to be zero for unicast. Fresh encodings zero reserved fields.

The peer-export cache is separate from CUDA state. Each transmission leases
a duplicated descriptor. Replacement/removal retires only the affected entry
and waits only for its leases; capture's clear drains the whole cache.
Descriptor close precedes the wakeup that permits CUDA teardown.

The frontend registers `pthread_atfork` hooks and supplies a reentrant operation
gate to the core. Public fork atomically closes admission only when no operation
is active; otherwise it returns `EAGAIN` without waiting. Prepare snapshots shim-owned
listener/cache descriptors and the carrier mapping, then holds admission closed
through fork. The child closes that inventory, unmaps the carrier without CUDA
unregistration, and detaches its inherited generation. Fresh state and a new
identity are created on first activity; inherited identity overrides are ignored.
The parent and application-owned ticket descriptors remain unchanged.

The child hook uses atomics, precomputed data, and close/munmap only. It does not
drop the inherited Rust generation, acquire a Rust mutex, or call CUDA. That
generation and the snapshot are intentionally leaked in the child; exec/process
exit reclaims them. Common mutexes and immutable loaded API/provider tables remain
usable because prepare quiesces every path that touches them.

Fork invoked recursively from an intercepted operation fails with `EDEADLK`.
A libc-internal bypass that cannot establish quiescence terminates the child with
status 127. CUDA operations racing the fully closed fork barrier return
`CUDA_ERROR_NOT_SUPPORTED` instead of waiting while possibly holding loader locks.
Listener workers wait outside loader locks. Fork never waits for active
operations: a caller may own a loader lock needed by one of those operations.
Applications that fork during CUDA/control traffic must retry `EAGAIN` from a
safe caller or quiesce that traffic first. Constructor reentry and concurrent
initialization of either the core library or a post-fork generation return
not-initialized rather than waiting for an initializer that may need the caller's
loader lock. This transient refusal does not poison initialization. A generation
is published as ready only after listener startup succeeds; a bound socket alone
does not make its state available. Actual generation setup failures are sticky,
and never publish a ready generation. The child immediately invalidates its old socket
registry, including before a second fork with no intervening shim activity.

Both libraries use unwind-catching C boundaries and sticky failure state.
This does not catch aborts, foreign exceptions, or invalid-pointer faults,
and does not make a partially mutated CUDA state safe to resume.

## Local validation

On Linux/amd64 with Rust and `/usr/bin/gcc`:

```sh
export CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER=/usr/bin/gcc
cargo test --workspace --target x86_64-unknown-linux-gnu
cargo build --workspace --release --target x86_64-unknown-linux-gnu
python3 frontend/tests/run.py
python3 core/tests/ticket_interop.py
python3 core/tests/reference.py
```

An explicit system linker avoids the host's Nix compiler linking against a
newer incompatible glibc. Release qualification still needs a controlled
glibc baseline and dynamic-symbol/dependency inspection.

Unit tests include wire known-byte compatibility fixtures, malformed ELF
tables, panic poisoning, sealed memfds, and export-cache retirement races.
The ticket interoperability runner compiles the unmodified reader and writer
from C stack commit `21008b50b93a9879a805665e331e777bb93abf49`, obtained with
`git show`, into a temporary helper. That commit must exist in the local
repository; the runner does not fetch or change branches. An explicitly
selected Rust test exchanges sealed ticket FDs with the helper in both
directions. It is ignored by ordinary `cargo test` because it needs that C
build. No CUDA toolkit is required.
The [standalone loader suite](frontend/tests/README.md) uses independently
compiled C fixtures with real CUDA symbol names and a mock core, without
CUDA headers or GPUs. It validates the front-end ABI and loader behavior,
not the Rust core's complete lifecycle. No test here establishes support for
general post-CUDA fork-without-exec, local `RTLD_NEXT` scopes, arbitrary loader namespaces, or
real multicast reconstruction.

`reference.py` builds the pinned C fixtures using a local CUDA 13.1/gtest Docker
image, then runs all 14 coordinator, 13 tracking, and 5 unicast lifecycle tests
against Rust, with no fork exclusions. `--fixtures <build-directory>` explicitly
reuses an existing build instead. Eight extra process-isolated regressions cover
pre-init fork, identity and descriptor reset, nested-fork FD reuse, saved-carrier
unmapping without CUDA cleanup, concurrent activity, and generation
poisoning, post-fork constructor/worker contention using the actual Rust core,
and sticky startup failure. The constructor regression waits for the child's
socket and the initializing worker's futex wait before reentering the shim,
then verifies that the worker completes after the loader lock is released.
Python may warn about multithreaded fork: these are deliberately
fake-driver regression tests, not a relaxation of POSIX/CUDA fork restrictions.
ASan instruments the C fixtures only, with leak detection disabled; it does not
instrument Rust. The frontend suite additionally checks recursive fork refusal
and same/cross-thread constructor reentry. Real workloads should use spawn/exec or fork before CUDA
initialization; shim reset cannot repair inherited NVIDIA runtime state.
