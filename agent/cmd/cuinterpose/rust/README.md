<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Rust cuinterpose experiment

This workspace is an incomplete, main-based port. It builds a run-ai-style
`libcuinterpose.so` front end, a separate `libcuinterpose_core.so`, and
`cuinterpose-coordinator`. Unicast, host-carrier, and multicast reconstruction
are implemented and exercised against fake CUDA. Fork generation reset is covered
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

The private host/core ABI is version 4, checked by version and size.
`cuinterpose_core_init` is the core's only dynamic export. It returns a
`repr(C)` table of typed C function pointers; memory calls do not look up
untyped core functions by name. The host's real-symbol resolver remains a
C-ABI callback because ELF lookup produces untyped addresses. The front end
loads the sibling core with `RTLD_LAZY | RTLD_LOCAL` on the first memory call,
successful `cuInit`, or successful CUDA resolver activity. The typed
`ensure_ready` callback also initializes a new child generation after fork.
The ELF constructor still only registers fork hooks; it does not load the core.
The sibling libraries are trusted code: a matching version and size promises
valid non-null callbacks with the declared signatures. Prefix checks reject
incompatible tables; they do not validate arbitrary foreign function pointers.

`RTLD_NEXT` returns the original-caller object walk's result without a second
substitution: a following interceptor must not jump backward into this shim.
Other lookups substitute only a CUDA-provider result, with concrete handles
restricted to that provider family in the base namespace. Unrelated plugins
and separate namespace handles keep their own result. Provider retention
checks the retained mapping identity, not merely its pathname.

Successful resolver requests for tracked APIs use the actual returned symbol's
ABI, constrained to the requested API family. Anonymous pointers, unknown
aliases, and unrelated symbols fail with `NOT_SUPPORTED` and a null output;
they cannot silently bypass tracking. Real lookup errors and query-status
failures retain their outputs/status. Untracked successful results are preserved,
but still require the process endpoint to initialize. No requested-version ABI
guessing is used.
If an inner intercepted resolver already returned a shim wrapper, the outer
resolver accepts only its exact known address in the requested API family.
The frontend's `build.rs` applies `-Bsymbolic-functions` only to that cdylib:
references to its own function definitions bind locally, so an earlier
preload cannot replace a wrapper-inventory address. Undefined CUDA/libc
functions and explicit `RTLD_NEXT` discovery remain dynamic. The core and
coordinator do not inherit this linker policy. This is a narrow identity
invariant, not a claim of compatibility with arbitrary loader namespaces.
This makes nested runtime-to-driver queries idempotent without trusting
arbitrary functions from a library named cuinterpose.

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
Cache identity includes resource kind and allocation ID, so a unicast request
cannot retrieve a multicast descriptor even if it names the same allocation ID.

The multicast module shares logical handles and VA mappings with unicast while
owning object/device/binding records. Runtime create, import, add-device, bind,
and map calls drop the state mutex around CUDA; an in-flight counter prevents
inspection or preparation while their results are not yet recorded. Objects
and tracked unicast members are pinned against concurrent release/unmap.
Driver handles in multicast records use `Option<u64>`: zero is a valid handle,
not the prepared-state marker.

Capture drains multicast exports, unmaps, unbinds, and releases objects before
saving shared unicast members. Successful BindMem or tracked BindAddr marks
the member shared even without a unicast ticket export. Restore recreates
creators, imports objects, attaches devices, then replays bindings/mappings,
with a global barrier after each operation. These driver calls also run
outside the state mutex under an exclusive lifecycle phase. Original requested
properties and v1/v2 ABIs are retained; inspection reports the largest extent
accepted by CUDA. BindMem replay temporarily retains mapped members whose
application handles were released, then drops that temporary reference.

Wrong-phase requests, unsupported sharing, and in-flight collectives refuse
without poisoning state. A driver failure after lifecycle mutation starts is
fail-stop. Per-resource checkpoint markers govern replay, and inspection
refuses more than 4096 topology records before allocating the response.

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
image, then runs all 14 coordinator, 13 tracking, 5 unicast lifecycle, and
6 multicast tests against Rust, with no fork exclusions. The temporary extracted
test harness receives one explicit adaptation: coordinator process creation
retries only `fork()` returning `EAGAIN`, up to a five-second absolute deadline,
and reports its retry count. Other errors or deadline exhaustion abort before
`waitpid`; no CUDA call, coordinator phase, child operation, or suite is retried.
The runner validates the pinned coordinator-header checksum before overlaying
that one launch site. Runtime sources, the fake driver, test assertions, and
actual fork-generation calls are unchanged. Five deterministic launch-helper
cases exercise recovery, terminal errors, deadline exhaustion, and child return.
`--fixtures <build-directory>` reuses an existing build with a warning that this
adaptation cannot be applied or verified; the default fresh build is authoritative.
Eight additional multicast modes cover
released handles and binding-only sharing, resource-kind/ticket mismatch,
partial mappings and unknown access, destructive versus harmless refusal,
native-address binding replay, a blocked collective with simultaneous control
requests, retain refusal during map publication, and driver-written create-error
output. Eight extra
process-isolated fork regressions cover
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
