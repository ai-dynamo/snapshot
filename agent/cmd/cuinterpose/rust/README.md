<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Rust cuinterpose experiment

The [development log](../../../../docs/development/cuinterpose-rust-development-log.md)
records failed approaches, reproduced defects, their fixes, and remaining
validation gaps. It distinguishes production behavior from test-only adapters.

This main-based port awaits integrated CRIU/vLLM qualification. It builds a run-ai-style
`libcuinterpose.so` front end, a separate `libcuinterpose_core.so`, and
`cuinterpose-coordinator`. Unicast, host-carrier, and multicast reconstruction
are implemented and exercised against fake CUDA. The existing three-test
physical-GPU suite also passed against revision `41dd090` on two B200s
(zero failures/skips, 19.186 seconds); see development-log section 11.
Fork generation reset is not qualified for general post-CUDA fork.
The separate native GLM 5.2 TE8 test passed capture on eight B200s on `tx5tk`,
CRIU/native CUDA/Rust restore on eight B200s on `s2877`, and fresh post-restore
inference. Development-log section 14 records the exact images and limitations.

## Relationship to the draft C design

The checkpoint semantics remain the target: sealed POSIX tickets identify the
original creator; only shared creator allocations receive host carriers;
never-shared allocations remain native-owned; unicast precedes multicast
reconstruction; the coordinator enforces topology validation and phase barriers.
This is not a claim of identical behavior or complete parity.

| Area | Intentional Rust implementation difference from the pinned C stack |
| --- | --- |
| Library boundary | Two shared objects with a versioned typed C ABI, rather than one interposition library |
| Resolver selection | Qualify the actual returned symbol and its provider; C selects replacement ABI from the requested name/version. Unknown tracked results fail closed rather than guessing. |
| Endpoint startup | First memory call, successful `cuInit`, or successful resolver activity initializes the endpoint; the Rust constructor only installs fork hooks, unlike C's endpoint constructor. |
| Fork reset | C-style atfork metadata locks; the child abandons its inherited Rust generation rather than destroying CUDA-bearing objects or reinitializing mutexes in place. Quiescent fork only. |
| Control dispatch | Prestarted bounded worker plus independent export listener, rather than C's per-connection thread with synchronous handling on spawn failure |
| Uncertain asynchronous copies | If synchronization cannot establish completion, terminate the process without cleanup; never free potentially DMA-referenced memory. |
| Allocation failure | Standard Rust allocation OOM can abort rather than return a CUDA error; catching panics does not change that. |

Snapshot packaging and namespace orchestration are implemented. The original
three-test GPU suite passed at `41dd090`; the packaged artifacts also passed
the native two-node GLM test. CustomStorage/NIXL composition remains untested.

## Snapshot integration

`make -C agent cuinterpose-build` builds both GNU libraries and the static musl
coordinator in a digest-pinned Rust 1.95.0 Debian bookworm container. Docker is
required; the shared host's Rust installation is not changed. The resulting
ELFs are checked for exact exports, a maximum `GLIBC_2.34` requirement, and no
CUDA/runtime linkage; the coordinator must have neither `PT_INTERP` nor
`DT_NEEDED`. `make -C agent cuinterpose-test` runs unit, loader, endpoint, and
static coordinator CLI/protocol tests in the same builder. A newer build libc
is not permission to raise the artifact's checked glibc baseline.

The agent image installs the frontend and core under `/usr/local/lib/snapshot`
and the coordinator under `/usr/local/bin`. Both libraries are copied as
`0644`, with the NVIDIA executable as `0755`, into every checkpoint target's
`/tmp/snapshot-cuda`. Only the frontend is added to `LD_PRELOAD`, on annotation
opt-in. Possible multi-GPU targets (including unknown DRA counts) independently
use `cuda-checkpoint --launch-job`; single-GPU and CPU-only targets are not
wrapped solely because of interposition.

Prepare opens trusted executable/checkpoint-directory descriptors, the target
root, and mount/UTS/IPC/network/PID namespace descriptors before `nsenter`.
Root and working-directory entry are explicit: entering a mount namespace
alone does not change filesystem root. Restore pins the same namespace set
and root, remounts both libraries at their source paths, and opens the static
coordinator before CRIU. The restore-complete sentinel is published only after
CRIU, native CUDA restore/unlock, and coordinator reconstruction succeed.
User and cgroup namespaces remain in the agent context.

Prepared manifests without CUDA metadata, requested interposition, delivered
tools, or `cuinterpose.state` are refused. The mount helper checks all three
tools before entering a namespace; a missing frontend or core is not a native
restore fallback. Main's image/host/GPU compatibility and process/artifact
identity checks are retained.

## Implementation boundaries

Core lifecycle operations and phases use enums rather than numeric states.
The numeric `DebugPhase` values exist only at the diagnostic C ABI boundary.

The coordinator emits one typed JSON report per completed phase using Serde
and `serde_json`, with numeric timings and allocation counts. The Go agent
decodes the same fields using `encoding/json`. This replaces the experimental
key/value progress format; producer and consumer must be updated together.
Serde is currently coordinator-only: the frontend still depends only on the
ABI crate and `libc`. MessagePack transport and typed topology records are
the next cleanup increment, not implemented by this report-format change.

| Crate | Owns |
| --- | --- |
| `frontend` | CUDA/resolver exports, checked allocation-free ELF bootstrap, caller-relative lookup, provider retention, and lazy core loading |
| `abi` | C layouts and the single `memory_api!` signature inventory generating wrappers and the typed core table |
| `core` | Allocation state, CUDA operations, host carriers, control listener, sealed-ticket transport, and leased peer-export descriptors |
| `protocol` | C v2 binary layouts, named records and tickets, checked codecs, socket framing, and descriptor transport |
| `coordinator` | Participant discovery, named-field topology validation, phase barriers, and state-file publication |

The private host/core ABI is version 5, checked by version and size.
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
Driver handles in unicast and multicast records use `Option<u64>`: zero is a valid handle,
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

The host-carrier module stages all mappings before timing copy enqueue and
synchronization. Its reported `copy_us` excludes allocation, context, mapping,
and export work. Context entry avoids redundant switches and releases a
retained primary context even if switching fails. Missing or failing host
registration queries trigger re-registration on load.

Load keeps fresh driver handles private until copying and staging cleanup
succeed. If transfer completion is established, failures roll back acquired
handles and save's temporary mapping-retained handles. Explicit cleanup then
attempts all stream, mapping, VA, and context releases even if one fails,
preserving the original CUDA error. CUDA never runs from `Drop`.
An arena is unmapped only after successful host unregister. Actual cleanup
failures remain fail-stop, not a promise that a failing driver freed resources.

If both synchronization attempts fail, completion is unknown. The shim writes
a diagnostic and immediately calls `_exit(127)`, without destructors, CUDA
cleanup, or a successful coordinator response. The agent must terminate the
remaining failed process tree. There is no quarantine registry, recovery
operation, or permanent fork lease. Redundant-retain release failure records the extra
reference and poisons capture eligibility rather than silently losing ownership.

Two mandatory threads start before generation publication: a peer listener
and a control worker with an eight-request bounded queue. The listener serves
`EXPORT` without taking the state mutex; all other requests, including
`INSPECT`, execute on the control worker. Reciprocal imports therefore retain
peer progress without per-request thread creation. A full queue refuses the
request before mutation, rather than dropping it or executing lifecycle work
on the listener. Startup failure logs the OS error, closes the owned listener
and path, and disconnects the only queue producer. An already-started idle
worker exits asynchronously; initialization never joins it while a caller may
hold the loader lock. The failed generation is never published or retried.
Header reads and peer writes use a per-blocking-I/O timeout (10 seconds by
default), not a total request deadline. Partial input can therefore occupy the
listener longer than that overall; this is not a hostile-client fairness
guarantee. Coordinator I/O errors include endpoint and operation.

Recoverable identity/handle-capacity checks precede new tracked allocations,
and failed import bookkeeping releases unpublished driver handles. Standard
Rust allocator OOM can still abort; panic catching does not convert it into a
CUDA error. During preparation, provably native handles and nonoverlapping
native ranges continue through CUDA while tracked or pending ranges remain
protected.

Wrong-phase requests, unsupported sharing, and in-flight collectives refuse
without poisoning state. A driver failure after lifecycle mutation starts is
fail-stop. Per-resource checkpoint markers govern replay, and inspection
refuses more than 4096 topology records before allocating the response.

The frontend registers `pthread_atfork`; it does not export `fork` or gate
every intercepted call. Prepare locks initialization, STATE, the export cache,
the owned-socket registry, then frontend provider references. Cache leases drain
before the snapshot, and socket open/register and unregister/close share a short
registry lock. Peer export never takes STATE. Parent callbacks release locks
without changing records.

The child closes only inventoried shim FDs, unmaps the carrier without CUDA
unregistration, and detaches its inherited generation. It abandons that
generation and its state/cache locks without invoking inherited CUDA-bearing
destructors. Process-lifetime initialization, registry, and provider mutexes are
unlocked by the surviving thread that acquired them; no Rust mutex is overwritten.
Fresh state and a random participant ID are created on first activity, ignoring
an inherited override. Parent state and application-owned ticket FDs survive.
The registry is cleared before unlocking so nested fork cannot close reused FDs.

The supported contract is quiescent fork, normally worker creation before CUDA.
Fork while CUDA/lifecycle calls are in flight (including collectives temporarily
outside STATE), recursive fork inside interception, constructor-time fork, and
arbitrary foreign atfork ordering are unsupported. No EAGAIN retry policy tries
to extend that contract. NVIDIA CUDA initialization inherited through fork is
not made usable by resetting shim metadata; real workloads should use spawn/exec
or fork before CUDA. Constructor reentry and concurrent
initialization of either the core library or a post-fork generation return
not-initialized rather than waiting for an initializer that may need the caller's
loader lock. This transient refusal does not poison initialization. A generation
is published as ready only after both mandatory thread starts succeed; a bound socket alone
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

The simplified fork path passed a fresh unsanitized reference run without
coordinator-launch adaptations or suite retries. Loader coverage is 23 cases
(three old recursive/constructor fork-policy cases removed), plus 19 actual-core
endpoint cases. The separate development log preserves previous failed runs.
The traced prior HANDSHAKE timeout occurred before either successfully created
importer thread entered its Rust closure, only with the sanitized fixture.
Equivalent unsanitized state→lifecycle sequences passed 300/300 in the targeted
diagnosis. The underlying sanitizer/runtime/old-gate interaction remains unknown;
it is not evidence of a protocol or carrier defect. None of these results
qualify native CUDA/CRIU or physical-GPU restore.

Unit tests include wire known-byte compatibility fixtures, malformed ELF
tables, panic poisoning, sealed memfds, and export-cache retirement races.
The carrier suite adds 22 process-isolated zero-handle, import rollback,
native-phase, partial-copy/cleanup, fail-stop, and timing cases. Persistent
D2H/H2D sync-failure subprocesses must exit 127 with the diagnostic; the provider
exits with a distinct failure if destructive cleanup runs. RPC tests cover both
mandatory worker startup failures, queue-full refusal before mutation, and
reciprocal unicast/multicast imports with thread creation unavailable after
startup. Constructor controls cover success and either startup spawn failing;
failures preserve output values and leave no path, FD, or eventual worker,
while the parent generation remains usable. The test-only carrier provider
uses `RTLD_NEXT`, translating a valid zero handle into the pinned fake driver's
nonzero model; production contains no such translation. A separate unit
subprocess checks absent-query fallback and primary-error preservation.
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
6 multicast tests against Rust, including ordinary forked importers. The pinned
fixtures retain their CUDA assertions. After the original C self-tests run,
`core/tests/json-reports.patch` changes only their progress-output assertions
before rebuilding them for Rust. There is no coordinator-fork retry adapter.
Prebuilt fixtures must carry the matching patch fingerprint.
By default `SANITIZE=` explicitly disables fixture instrumentation and readelf
checks actual linkage. `--sanitized` selects separate ASan/UBSan diagnostic
coverage; `--fixtures` reuse must match the selected linkage.
Eight additional multicast modes cover
released handles and binding-only sharing, resource-kind/ticket mismatch,
partial mappings and unknown access, destructive versus harmless refusal,
native-address binding replay, a blocked collective with simultaneous control
requests, retain refusal during map publication, and driver-written create-error
output. Six extra
process-isolated fork regressions cover
pre-init fork, identity and descriptor reset, nested-fork FD reuse, saved-carrier
unmapping without CUDA cleanup, generation poisoning, and sticky startup failure.
Concurrent-CUDA fork and the redundant child-constructor fork mode were removed;
ordinary loader-constructor reentry remains covered in the endpoint suite.
Python may warn about multithreaded fork: these are deliberately
fake-driver regression tests, not a relaxation of POSIX/CUDA fork restrictions.
ASan instruments the C fixtures only, with leak detection disabled; it does not
instrument Rust. The frontend suite checks same/cross-thread constructor reentry,
not fork from those constructors. Real workloads should use spawn/exec or fork before CUDA
initialization; shim reset cannot repair inherited NVIDIA runtime state.

For the physical-GPU suite, `core/tests/stage_gpu.py DEST --artifacts DIR
--cuda-checkpoint PATH` extracts the pinned test sources and applies only
`gpu-json-reports.patch` to the harness parser. It stages both Rust libraries
and the coordinator, and exercises the staged JSON parser without loading CUDA
on the build host. GPU test bodies and throughput assertions are unchanged.
