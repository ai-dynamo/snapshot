<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Rust cuinterpose: failures, fixes, and validation history

This is the engineering investigation log for the main-based Rust cuinterpose
port. It records unsuccessful approaches, reproduced defects, test limitations,
and the corrections made in response. It is not a claim that the port has
completed production or GPU qualification.

The implementation branch is `schwinns/cuinterpose-rust-runai`. Its reference is the
C cuinterpose stack at `21008b50b93a9879a805665e331e777bb93abf49`.
Production cuinterpose consists of a Rust frontend, Rust core, and Rust
coordinator. C and C++ code used below is test infrastructure or the reference
implementation, not a production fallback.

## Current milestones

| Commit or work item | State |
| --- | --- |
| `e4c801d`: initial Rust frontend, core, coordinator, typed ABI and codecs | Committed after review and independent tests |
| `83da220`: fork-generation isolation | Committed after review and independent tests |
| `e807c88`: multicast tracking and reconstruction | Committed after review and independent tests |
| `b99f4bc`: loader composition and participant initialization | Committed after review and independent tests |
| Unicast zero handles, host-carrier cleanup, and prestarted control execution | Retained during fork simplification; focused independent review approved and independent unsanitized gate passed |
| C-style quiescent fork and unknown-completion fail-stop | Focused independent review approved and independent gate passed atop `b99f4bc`; broad global gate and coordinator retry adapter removed at user direction; see section 10 |
| Snapshot delivery/orchestration and static coordinator packaging | Implemented on the working tree after `3184500`; local Go, chart, artifact and containerized Rust checks pass; integrated image/CRIU qualification pending |
| CustomStorage/NIXL optimization composition | Pending; intentionally separate from native integration |
| Standalone physical-GPU suite | Three passed, zero failures/skips at `41dd090` on two B200s; see section 11 |
| Native CRIU/vLLM two-node qualification | GLM 5.2 TE8 passed on eight B200s per node; section 14 |

## What “fallback” means here

The accounts below preserve historical decisions and failures. Section 10
supersedes the global fork gate, EAGAIN policy, coordinator-launch adapter,
and process-lifetime DMA quarantine; those mechanisms are no longer current.

The original control listener gave each accepted Unix-socket connection to a
new handler thread. An attempted correction for handler-thread creation failure
executed that **same accepted request synchronously on the listener thread**.
Ownership was transferred exactly once; the request was not resubmitted.

This was a scheduling fallback inside the Rust shim. It did not load C
cuinterpose, choose a different checkpoint backend, skip a phase, switch to
native capture, or retry a CUDA operation.

Three other behaviors must not be confused with it:

| Behavior | Meaning |
| --- | --- |
| Historical coordinator test-launch retry | Test-only bounded EAGAIN retry; removed with the global gate in section 10 |
| Host-registration recovery | Re-register the restored carrier when its registration cannot be confirmed |
| Historical uncertain-transfer containment | Retained resources and failed the generation; replaced by immediate process fail-stop in section 10 |

Review rejected this synchronous lifecycle dispatch: it can strand peer
requests needed by the request being served. Its replacement prestarts a
dedicated control worker and an independent peer listener. Local validation
passed before the constructor rollback review below; a later sanitizer-linked
run had the handshake timeout classified in section 10. The simplified current
implementation subsequently passed focused independent review and testing.
Exactly-once ownership alone does not make an execution strategy safe.

## Compatibility with the draft C design

The intended checkpoint semantics are unchanged: sealed POSIX tickets name the
original creator, shared creators carry canonical bytes, never-shared memory
remains native-owned, and coordinator barriers reconstruct unicast before
multicast. “Following the C design” does not mean identical behavior everywhere.

| Area | Deliberate implementation or compatibility difference |
| --- | --- |
| Loader/core boundary | Two Rust shared objects and a versioned typed C ABI instead of one C shim |
| Resolver ABI selection | Actual returned symbol/provider identity, rather than C's requested name/version table; unknown tracked results fail closed |
| Endpoint startup | Rust memory calls, successful `cuInit`, and successful resolver activity initialize the endpoint; C also initializes it in its constructor. Rust's constructor only registers fork hooks. |
| Fork | C-style metadata-locking prepare; abandon inherited Rust CUDA-bearing state rather than overwrite its mutexes. Only quiescent fork is supported. |
| Control execution | Prestarted bounded worker and independent export service, unlike C's per-connection spawn/synchronous failure path |
| Uncertain transfer completion | Immediate process termination without cleanup that could invalidate DMA references |
| OOM | Standard allocator failure can abort; panic catching cannot convert all OOM into CUDA errors. |

Snapshot integration, static coordinator packaging, and CRIU/vLLM testing
are unfinished work, not new design features. The standalone physical-GPU
result in section 11 does not establish full parity.

## 1. Build and early loader bootstrap

### The default compiler selected an incompatible libc

The first host builds used the `gcc` on `PATH`, a Nix compiler wrapper.
Artifacts acquired a dependency on a newer Nix glibc rather than the intended
host baseline and could not be used reliably by the host test executables.

Local validation now selects the system linker explicitly:

```sh
CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER=/usr/bin/gcc
```

Artifact inspection subsequently observed a maximum required version of
`GLIBC_2.34`. This is local evidence, not a substitute for a pinned release
builder and dependency inspection of the shipped artifacts.

### Allocating while bootstrapping `dlsym` broke sanitizer startup

The initial ELF bootstrap used ordinary file reading, which allocated memory.
An allocator or sanitizer can call `dlsym` while initializing its own
allocation machinery. Reentering that machinery from our bootstrap failed.

Replacing heap-backed reads with file mapping was necessary but insufficient:
the normal mapping functions could themselves be intercepted during sanitizer
startup. The parser now opens and maps the provider image through raw Linux
syscalls, using checked borrowed slices and stack data to discover symbols.
It does not cast arbitrary file bytes into Rust structure references.

### Ignoring GNU IFUNC symbols still broke sanitizer startup

The early ELF symbol filter excluded GNU indirect functions. Important libc
functions such as `memset` can be IFUNCs; their symbol value names a resolver,
not the final implementation. Sanitizer initialization consequently failed
to obtain the function it needed.

The lookup now recognizes IFUNC entries and invokes their resolver to obtain
the callable implementation. This allowed the sanitizer-built C fixtures to
start with the Rust frontend preloaded. Synthetic parser tests cover IFUNC
identification; that coverage does not prove every possible IFUNC or loader
environment is supported.

### Stale fixtures and incorrect layout assumptions obscured results

An existing `agent/cmd/cuinterpose/build/` contained older CUDA-header/protocol
fixtures. Header-version and forwarding failures from those binaries were
not valid evidence about the pinned reference head. Fresh extraction and
compilation of the pinned C sources replaced them as the authoritative path.

Another layout detail required explicit verification: CUDA's
`CUmemAccessDesc` uses a 32-bit flags enum and occupies 12 bytes on the target
ABI. The control protocol's access record occupies 16 bytes. They are
different formats. The Rust CUDA layout has an assertion; a later Python
carrier fixture's incorrectly declared 64-bit flags field was also corrected.

## 2. Interception and the frontend/core boundary

### The original fake resolver did not expose real CUDA symbol identities

The pinned C fake driver returns pointers named, for example,
`fakeCuMemCreate`. Run-ai-style substitution identifies the function actually
returned by the driver, rather than guessing solely from the query string.
The Rust frontend could not identify these fake names as public CUDA ABIs.

We did not add fake-name rules to production or claim the old forwarding
suite passed. Independent test providers now export actual CUDA symbol names
and versioned entry points. The reference tracking and lifecycle tests still
exercise the Rust core; the independent loader suite tests pointer discovery
and callable signatures.

### Untyped dispatch and raw protocol offsets were replaced

The initial private core ABI returned `void*` from a name lookup. Both sides
shared a signature inventory, but dispatch still required a string lookup
and function-pointer cast. Allocation and topology code also constructed wire
records through numeric byte slices.

The frontend now calls a generated, typed `repr(C)` function table. Complete
CUDA operations remain in the core; no generic pre/post-hook framework was
introduced. Named protocol records and tickets isolate binary offsets in the
codec. Named ELF readers isolate the separate ELF-format knowledge.

### A Rust round trip passed while the ticket was incompatible with C

Review found that Rust-created unicast tickets populated multicast-only
metadata, and that the decoder accepted nonzero reserved ticket arrays. The
pinned C reader rejects both. A Rust writer and reader agreed with each other
while disagreeing with the required format.

The codec now rejects the C-reserved fields and requires unicast multicast
metadata to be zero. Allocation size and creation properties stay in the
allocation record, not those unicast ticket fields.

A dedicated test compiles the unmodified pinned C ticket reader/writer and
exchanges sealed tickets in both directions. Negative tests modify every
reserved byte and each multicast-only field. ABI guard-page tests additionally
check that an eight-byte incompatible table prefix is rejected without reading
the rest of the table.

### A second interposer caused `RTLD_NEXT` recursion

The original `dlsym` wrapper first found the result after the original caller,
then substituted our wrapper by name. If another interposer appeared after
cuinterpose and asked for `RTLD_NEXT`, substitution jumped backward to
cuinterpose and created a cycle.

The frontend now returns the caller-relative ELF walk's result directly for
`RTLD_NEXT`; it does not substitute a second time. Tests execute both preload
orders and require the tested core, second interceptor, and driver paths to
run exactly once. Multicast blocking tests now use real `RTLD_NEXT` chaining
on their map and add-device paths, rather than bypassing the chain through
the fake driver's private lookup helper.

### Same-named functions from non-CUDA libraries were being intercepted

Provider retention originally returned no qualification decision. Even when
it declined to retain a non-CUDA library, the caller still substituted a
CUDA wrapper for that library's same-named symbol.

Substitution now qualifies the actual provider, checks concrete handle
family/namespace, and verifies the retained mapping. Non-CUDA plugins and
misleading library names keep their own explicit lookup behavior.

### Unrecognized resolver results silently escaped tracking

An exact returned symbol identity is useful for choosing the correct ABI.
However, passing an unrecognized pointer through after a successful tracked
API query creates an unobserved sharing path.

Tracked queries now require a recognized function in the requested API
family. Ambiguous, foreign, or wrong-family results return not-supported
with a null output. Genuine resolver failures and failed query statuses
retain their original semantics. Real-driver symbol identities remain a
physical qualification requirement.

### Private-only CUDA processes did not create control endpoints

Lazy core initialization initially occurred only at a tracked memory call
or debug query. A CUDA process using only native/private allocations could
therefore lack the socket required by Snapshot's complete-participant check.

The frontend now intercepts `cuInit` and ensures core readiness after
successful CUDA initialization or resolver activity. The typed ABI includes
`ensure_ready`, so a fork child also initializes a new generation even when
the frontend already holds the core's function table.

Actual-core tests cover direct and handle-based initialization, resolver-only
activity, a private-runtime fixture, child identity, constructor contention,
and obstructed endpoint startup. They do not prove every real vLLM process
uses these paths.

### Nested resolvers rejected a function already wrapped by cuinterpose

A runtime resolver can delegate to an intercepted driver resolver. The inner
resolver correctly returned our wrapper, but the outer resolver rejected its
provider because it was cuinterpose rather than CUDA.

Postprocessing now recognizes an exact known local wrapper address after
checking the API family. Nested runtime-to-driver tests invoke returned
unicast and multicast v1/v2 functions, including arguments passed on the stack.

### “Our wrapper address” itself could be preempted

The first nested-resolver correction compared against an exported function
reference that ELF could resolve to an earlier preload. An earlier non-CUDA
plugin could therefore be mistaken for our own wrapper.

`frontend/build.rs` applies `-Bsymbolic-functions` only to the frontend
cdylib. Internal references to its defined functions bind locally, while
external CUDA/libc imports and explicit dynamic lookup remain dynamic.
An artifact test rejects dynamic relocations to the frontend's own function
exports. Earlier-plugin and both-order chaining tests check actual behavior.

One test initially assumed an earlier interposer's `RTLD_DEFAULT` result
must equal an explicit CUDA-handle lookup. That assertion was wrong: those
are different lookup scopes. The corrected assertion checks the proper
provider for each, rather than requiring equality.

An independent negative control removed the binding flag in a scratch copy:
both the relocation check and a behavioral chaining test failed.

## 3. Export-cache retirement

### Closing the cache did not wait for a descriptor still being sent

A peer handler duplicates a cached CUDA FD and sends it over `SCM_RIGHTS`.
Removing the cache entry alone does not establish that this transmission
has finished. The cache now leases each transmitted descriptor and closes
the duplicate before waking teardown.

### The first drain implementation disrupted unrelated allocations

The first lease implementation globally stopped admission for every
insertion or removal. Holding an export lease for allocation A while first
exporting unrelated B could reject another valid request for A. Even removing
an uncached private allocation could briefly disrupt exports.

Replacement and removal now retire only the affected entry. Capture-wide
clear drains the whole cache. Tests hold A while inserting, removing, or
retiring B, and exercise an actual socket send failure to prove its lease
is released. Cache keys include resource kind as well as allocation ID.

## 4. Fork-generation isolation

Fork copies memory and descriptors but only preserves the calling thread.
The first Rust prototype refused inherited initialized state rather than
reuse mutexes or CUDA bookkeeping belonging to vanished parent threads.
The implemented reset detaches the child generation without normal Rust
destruction or CUDA cleanup, then initializes a new identity and endpoint.

### Waiting for active operations caused a constructor/loader deadlock

An initial operation gate waited for readers before fork. A DSO constructor
could hold the loader lock while another admitted CUDA call waited for that
lock; the constructor's fork then waited for the CUDA call. Neither could
proceed.

Public fork now atomically claims an idle gate or returns `EAGAIN`; it does
not wait for readers. Recursive fork inside an intercepted operation returns
`EDEADLK`. An unsafe libc bypass child exits rather than continue with
inherited active state.

This is an intentional compatibility restriction. In one independent
continuous-traffic probe, 3,985 of 4,000 forks returned `EAGAIN`; 15 succeeded
and none returned another error. Idle forks and the tested fork+exec path
passed. This is not transparent arbitrary concurrent-fork support.

### Same-thread reentry protection missed cross-thread lazy initialization

A thread-local guard prevented recursive initialization on the same thread,
but `OnceLock::get_or_init` still waited for another thread. A loader-locked
constructor could wait for the initializer while that initializer waited
for the constructor's loader lock.

Lazy core initialization now uses nonblocking admission. A competing or
reentrant call returns transient not-initialized rather than waiting or
publishing a permanent initialization failure.

### Resolver guards were incomplete and duplicated

Some driver/runtime resolver wrappers guarded only lookup, not the actual
resolver invocation. A provider callback could fork without the intended
recursive-fork refusal. Other wrappers accidentally acquired the same
reentrant guard two or three times.

Each public operation now has one outer guard covering invocation and
postprocessing. Tests attempt recursive fork from all seven resolver variants.

### A second fork could close a reused application descriptor

The child closed inherited listener descriptors but left their numbers in a
registry until first shim activity. If it opened an application file using
one of those numbers and forked again first, the grandchild closed the
application file.

The child hook now immediately marks the inherited registry abandoned,
without locking or dropping Rust data. A nested-fork test deliberately reuses
the old listener FD and verifies preservation of both that application FD
and an inherited application ticket.

### Child generation initialization had another loader-lock inversion

After the frontend table was initialized, child generation creation still
used a blocking mutex across listener startup. A constructor and worker
could reproduce the same cross-thread loader-lock cycle inside the actual
core rather than the mock frontend fixture.

Generation initialization uses `try_lock`, and `STATE` is published only
after listener startup succeeds. Listener construction receives endpoint
and identity directly. Actual setup failure is sticky; initialization
contention is transient. The actual-core constructor regression exercises
this distinction.

One attempted resolver-constructor fixture waited for a socket while its
worker had not yet passed CUDA symbol lookup. That fixture never reached
the intended contention point. It was corrected to establish generation
initialization through the debug entry, then reenter the resolver.

### Fork evidence needed stronger assertions

The initial churn test retried release on error 801 but did not assert that
its eventual result was success. It now records unexpected errors and
requires success. Additional tests verify that a saved carrier is unmapped
in the child without CUDA unregister/release, while parent ownership remains.

None of this makes real CUDA use after a CUDA-initialized parent forks
supported. Shim bookkeeping tests cannot establish that NVIDIA's runtime
and driver tolerate that pattern.

## 5. Multicast publication and lifecycle

### Initial multicast forwarding was not reconstruction

The prototype passed multicast calls through and recorded successful
creation as unsupported for capture. Empty restore operations were a
fail-closed placeholder, not parity.

The multicast module now owns logical handles, ticket identity, devices,
v1/v2 bindings, mappings/access, effective extent, capture teardown, and
the four globally ordered restore operations. Blocking runtime collectives
release the state mutex while retaining generation and resource pins.

### Retain-by-address exposed a raw handle during mapping publication

CUDA could establish a multicast mapping before the shim reacquired the
state mutex to publish its record. Retain-by-address checked only completed
records, treated the address as native, and exposed an untracked real handle.

Retain now rejects addresses in pending mappings with not-ready, before
calling CUDA or modifying output. The deterministic regression blocks after
the real map succeeds, probes the first/middle/last byte, and checks driver
reference counts and output sentinels. After publication, retain returns a
logical alias. An independent scratch-copy negative control removed the
guard and reproduced the failure.

### Failed multicast create did not preserve driver output

The wrapper stored the driver result in a local handle and returned an
error without copying the driver-written output to the application. The
pinned C implementation preserves that output.

The wrapper now preserves output after an actual failed driver call, while
leaving it unchanged if symbol resolution failed before CUDA was called.
A test verifies error 110, output `0x456`, and no published object.

### Phase refusal was confused with destructive failure

The initial control handler poisoned the shim on every lifecycle error,
including an out-of-order request that changed nothing.

Preflight and phase-order refusals are now distinguished from execution
failures after entering a destructive operation. In-flight collectives,
unsupported resources, and wrong phase do not by themselves poison an
otherwise usable workload. Prepared/host-saved markers record what was
actually removed or carried, rather than inferring it solely from sharing.

## 6. Two distinct `EAGAIN` investigations

### A test coordinator launch did not handle fork refusal

The C test helper called `fork()` and continued to `waitpid` without checking
for `-1`. That conflicts with the Rust shim's deliberate nonblocking
`EAGAIN` policy and caused intermittent launch failures.

Fresh reference builds apply a checksum-validated, test-only overlay to
that single coordinator launch site. It retries only `EAGAIN` under one
five-second absolute deadline and reports retries. Other errors or deadline
exhaustion fail before `waitpid`. It never repeats an already-started child,
CUDA operation, lifecycle phase, or whole suite.

Five deterministic tests cover recovery, terminal errors, deadline
exhaustion, a slow failed attempt consuming the budget, and child return.
Reference binaries built with this overlay are explicitly described as
adapted fixtures, not wholly unmodified C sources.

### A later `EAGAIN` was a socket timeout, not that launch problem

The final allocation-parity run failed in
`Lifecycle.PrepareAndRestoreAcrossTwoProcesses` after roughly 10.197
seconds. That matched the control receive timeout. No launch-retry diagnostic
or successful carrier phase appeared. Missing carrier/state assertions were
consequences of failed prepare, not evidence of a failed copy.

The evidence establishes a missing control reply, with `recvmsg` reporting
timeout as `EAGAIN`. It did not identify the exact operation or original
failed syscall. Subsequent passing runs do not retroactively classify it.

Investigation found an independently confirmed defect: the listener ignored
handler-thread creation errors and could lose accepted requests under task
pressure. A focused lifecycle run passed 250 times under normal resources;
constraining available tasks reproduced control failures. This supports the
resource-pressure hypothesis but does not prove it caused the original
timeout.

The attempted correction recovered socket ownership on worker-start failure
and handled the same request synchronously. Deterministic `pthread_create`
failure injection exercised ten requests, including lifecycle operations,
with exactly-once execution. Coordinator I/O errors now include endpoint and
operation so a future timeout is diagnosable.

### Synchronous lifecycle dispatch deadlocked reciprocal importers

The first forced-thread-failure test used a participant with no imported
allocation. Its import phases therefore did not need another participant,
and it missed a dependency cycle.

A reviewer created two processes that each own an allocation and import the
other's allocation. After capture and creator load, both RPC worker starts
were forced to fail for `RESTORE_UNICAST`. Both listeners ran restore
synchronously, requested an export from the other participant, and stopped
accepting the requests needed to complete either restore. Both phases timed
out and failed. With normal handler creation, the same topology restored
successfully. Reciprocal multicast imports have the same dependency.

This rejects the synchronous lifecycle fallback even though its connection
ownership was exactly once and the pinned C implementation had a similar
fallback. The replacement must preserve independent export service while
lifecycle work runs, or explicitly refuse execution before mutation when
capacity is unavailable. It must not retry partially executed phases.

The original ten-request test also did not force failure for the final
multicast-binding operation. It establishes narrow dispatch behavior, not
comprehensive failure coverage of every lifecycle operation.

### Prestarted control execution replaces per-request thread creation

The replacement starts one control worker with a bounded eight-request queue
and one listener before publishing the generation. The listener classifies
requests and serves exports without taking the state mutex; every other
operation, including handshake and inspection, goes to the control worker.
Queue exhaustion returns an explicit refusal before mutation. There is no
per-request spawn, synchronous lifecycle fallback, or CUDA phase replay.

This version logged the actual OS error when either mandatory thread could
not start, closed the listener, disconnected and joined an already-started idle
worker, removed its own socket path, and left initialization permanently failed.
The synchronous join was subsequently rejected as described below. Accepted
and queued sockets remain registered in the fork inventory.

The first focused run passed both injected mandatory-thread startup failures.
The queue test then failed because it expected `INSPECT` to succeed after
`SAVE_ALLOCATIONS`. Inspection intentionally refuses outside the active phase;
the test was corrected to require that precise response rather than relax the
production phase guard. The debug phase assertion was likewise corrected from
the wire operation number to the public preparing-state value.

Reciprocal unicast and multicast regressions rendezvous both import workers
immediately before their peer export requests, queue inspection behind each,
and keep `pthread_create` unavailable after startup. This tests actual peer
dependencies, not empty import phases. Both reciprocal cases passed the
focused run with queued inspections returning the expected phase refusal.

The first fresh-reference run passed the Rust coordinator, tracking, unicast
lifecycle, and six multicast cases, then failed the additional `released`
multicast mode's handshake. Its Python helper opened the operation socket,
left it idle, and opened a second socket to discover identity before writing
the first header. The classification listener legitimately waited for the
idle first connection's socket header-read timeout; the nested handshake's shorter
test timeout expired first. The helper now obtains identity before opening
the operation connection. Production coordinators already handshake before
phase connections; no production phase retry or extra worker was added.

After that specific helper correction, all modes passed against the existing
pinned fixtures and then in a new complete fresh-reference build. The original
failed fresh run remains `/tmp/cuinterpose-prestarted-reference.log`; the
corrected fresh run is `/tmp/cuinterpose-prestarted-reference-corrected.log`
in this development session, not a committed/public evidence bundle.

| Validation of the revised dispatcher | Result |
| --- | --- |
| `cargo fmt --all -- --check`; release build | Passed |
| Ordinary Rust unit tests | 22 passed; dedicated interoperability case excluded here |
| Frontend loader / actual-core endpoints | 26 / 19 passed |
| Pinned C ticket interoperability runner | Passed |
| Fresh reference Rust coordinator / tracking / lifecycle / multicast | 14 / 13 / 5 / 6 passed |
| Additional multicast / host-carrier / fork modes | 8 / 22 / 8 passed |
| New RPC modes | Both startup failures, queue refusal, reciprocal unicast, reciprocal multicast passed |
| Test-only coordinator launch adapter | Five cases passed |

The actual-core loader-locked generation constructor test still passes with
two startup threads; its readiness assertions were not disabled or weakened.
No production host-carrier ownership code changed in this dispatch correction.
The optional multi-context pending-copy follow-up remains unimplemented; the
prior review's queued native-mutation probes are not yet committed regressions.

Additional focused assertions confirmed that reciprocal unicast copied each
creator's bytes once in each direction, while multicast objects themselves
carried no allocation bytes. A subsequent combined housekeeping command used
the wrong relative directory for `git diff --check` and stopped before diagram
rendering; those checks were rerun explicitly from the workspace root. This was
a command-path error, not a build or runtime failure.

### Joining startup rollback deadlocked under the loader lock

Review then exercised initialization from an actual DSO constructor, not only
ordinary Python calls. With the second mandatory spawn forced to fail,
initialization joined the already-started control worker. The constructor
caller held the loader lock; the Rust worker could need that same lock during
TLS startup or teardown. Not needing the initializer's operation gate did
**not** make that join safe. Success and first-spawn-failure controls returned,
but second-spawn failure hung until the test timeout.

The correction removed that synchronous join. The failed listener spawn drops
its closure, including the listener and the queue's **only** sender. No
connection was dispatched, so there is no queued handler to orphan. Dropping
the worker handle detaches it; once loader startup can proceed it observes the
disconnected queue and exits. Initialization promptly removes its owned path
and fails permanently without publishing the generation. There is no worker
join inside the constructor and no producer retained to keep `recv` waiting.

The new test uses the actual release frontend/core, a fake driver, and a
`dlopen` constructor. Success and failure of each mandatory spawn are separate
cases. Failure preserves the allocation output and every stats-output byte,
leaves no socket or FD, and remains sticky after the injected cause is removed.
After `dlopen` releases the loader lock, bounded polling verifies kernel worker
thread disappearance. A separately initialized parent keeps its allocation,
identity, and endpoint after each child case.

The first local reproducer used the long absolute evidence-directory path as
the control directory and failed before reaching the constructor: the Unix
socket pathname exceeded its length limit. It was corrected to use a short
temporary socket directory while retaining logs outside `/tmp`. The pre-fix
second-spawn constructor then reproduced the 15-second timeout. After the
correction, all three constructor cases passed.

Header I/O has per-blocking-I/O socket timeouts, not a total request deadline.
`recvmsg` followed by `read_exact` can spend more than the nominal ten seconds
overall when partial input arrives. The documentation now says this explicitly;
the simple listener is not a hostile-client fairness guarantee.

### Latest gate: constructor fix passes, lifecycle handshake remains unresolved

The fresh-reference run after removing the join failed
`Lifecycle.PrepareAndRestoreAcrossTwoProcesses` after approximately 10.199
seconds. This time the contextual error identifies the failed operation:
`Handshake receive failed: Resource temporarily unavailable (os error 11)`.
The other four lifecycle tests passed. This is not evidence that a host-carrier
copy failed, and removing a join on the startup-error path does not establish
the cause of a handshake failure during otherwise successful startup.

The failed suite was **not retried**. Its temporary fixture tree was removed
by the runner, so the unreached cases were run separately using newly extracted
pinned sources and newly built fixtures. That continuation did not rerun
coordinator, tracking, or lifecycle. It passed all remaining cases.

| Latest local validation | Result |
| --- | --- |
| Format / release build | Passed |
| Ordinary Rust unit tests | 22 passed; one interoperability test intentionally excluded and then run separately |
| Loader / actual-core endpoint cases | 26 / 19 passed |
| Rust → C → Rust sealed ticket interoperability | One passed |
| Fresh reference coordinator / tracking | 14 / 13 passed |
| Fresh reference lifecycle | Four passed, one failed (`PrepareAndRestoreAcrossTwoProcesses`, handshake receive timeout) |
| Separately run fresh multicast / extra multicast | 6 / 8 passed |
| Carrier / fork | 22 / 8 passed |
| RPC | Eight passed: five existing modes plus three constructor controls |
| Test-only coordinator-launch adapter | Five passed |

C self-checks passed during fixture construction; those are reference
validation, not Rust execution. Rust artifacts are not ASan-instrumented even
when the C fixtures are; host-side C leak detection remains disabled.
Python's multithreaded-fork deprecation warnings remain visible in the logs.
No GPU, CRIU, vLLM, or cross-node test was run.

Durable session evidence lives under `.cuinterpose-evidence/control-startup/`
outside the repository: pre-fix failures, earlier failed/corrected reference
logs, current gate and continuation logs, reproducible command scripts, and a
source/artifact SHA256 manifest tied to the current commit and working diff.
Historical copied logs are explicitly not sealed evidence for the current
revision. The final independent gate must seal the exact revision after review.
The handshake failure and optional multi-context/queued-native-mutation
coverage gaps remain open; this local result is not approval to merge.

## 7. Allocation ownership and host-carrier failure paths

The following corrections passed the focused independent review/test gate
after the simplification in section 10. The quarantine approach described
below is historical; the current implementation uses fail-stop instead.

### Zero was used as “no CUDA handle”

CUDA can return a valid zero-valued allocation handle. The original unicast
state used zero to represent absence, unlike the newer multicast records.

Unicast backing now uses `Option<u64>` too. `Some(0)` passes through create,
import, mapping, export, properties, retain, save/load, release, and multicast
member replay. Only `None` denotes absence.

### Failed construction leaked unpublished ownership

Some paths acquired driver resources and then failed property lookup or
bookkeeping without releasing the acquired reference. Host restore also
published fresh handles too early for transactional cleanup.

Fresh backing remains private until copying and staging cleanup succeed.
Runtime import/property failures roll back unpublished references. A logical
release failure retains the alias for retry. Failed cleanup of a redundant
retain records the extra ownership and poisons capture eligibility instead
of continuing with an unrecorded reference.

Mapping-only restored allocations release temporary driver references, and
multicast replay retains/relinquishes a temporary member handle when needed.

### Cleanup stopped at the first cleanup failure

Early-return cleanup could skip later stream, mapping, VA, registration, or
context releases. Cleanup now attempts all applicable releases while
preserving the first error, **but only after transfer completion is known**.

Contexts avoid redundant switches. Failed switching releases a retained
primary context; context restoration failure does not skip primary-context
release. Failure to confirm restored host registration triggers
re-registration. Copy timing now measures enqueue/synchronization intervals
rather than the full lifecycle RPC.

### “Try every cleanup” was unsafe when synchronization kept failing

The first cleanup correction retried synchronization once, then destroyed
the stream, unmapped staging, and unregistered the host arena even when the
retry also failed. Neither failed synchronization nor stream destruction
establishes that queued copies stopped referencing memory.

An independent pending-copy provider reproduced:

```text
pending=1 sync_failures=2 unsafe_unmaps=1 unsafe_unregisters=1
```

The original fault tests failed only the first synchronization; their
cleanup retry succeeded, so they missed the unsafe case.

The correction distinguishes drained errors from uncertain completion.
Persistent synchronization failure retains stream, staging mappings/VA,
arena/registration, backing, and context ownership in a process-lifetime
quarantine. It preserves the original error and poisons the generation.
A retained operation lease prevents fork from discarding these potentially
DMA-referenced resources. Queued callers recheck failure after obtaining
the state mutex.

This deliberately holds resources until workload termination. It is not
in-process recovery. D2H and H2D persistent-failure tests check that no
referenced resources are cleaned up, while one-failure-then-success tests
still exercise ordinary cleanup.

### Native-owned work was unnecessarily gated by shim phases

Several wrappers required an active shim phase before determining whether
the call involved any tracked resource. Provably native handles and ranges
now retain pass-through behavior during preparation. Tracked handles,
recorded ranges, and pending multicast mappings remain protected.

## 8. What the validation does and does not establish

The committed loader milestone passed 21 ordinary Rust unit tests, 26 loader
cases, 19 actual-core endpoint cases, and the dedicated ticket
interoperability test. Fresh reference fixtures exercised the Rust
coordinator (14), tracking (13), unicast lifecycle (5), and multicast (6),
plus eight additional multicast and eight fork modes. The five coordinator
launch-adapter cases are test infrastructure, not CUDA tests.

The simplified implementation passed an independent gate with 22 ordinary
unit tests, 23 loader cases, 19 actual-core endpoint cases, one ticket exchange,
14 coordinator cases, 13 tracking cases, five lifecycle cases, six multicast
cases, and additional modes for multicast (eight), carriers (22), fork (six),
and RPC (eight, containing 11 assertions). The coordinator-launch adapter is
gone. The gate used fresh, genuinely unsanitized reference fixtures and passed
on its first supported-path run. Earlier failures remain recorded above;
this success does not establish the cause of the sanitizer-linked timeout.

The Docker build also runs the **C implementation's own tests**. Those
results are never counted as Rust multicast or lifecycle evidence.
Sanitizers instrument the C fixtures, not the Rust libraries, and fixture
leak detection is disabled. The standalone loader tests use an independent
mock core; endpoint and reference lifecycle tests use the actual Rust core.

Local test entry points:

```sh
cd agent/cmd/cuinterpose/rust
export CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER=/usr/bin/gcc
cargo test --workspace --target x86_64-unknown-linux-gnu
python3 frontend/tests/run.py
python3 core/tests/ticket_interop.py
python3 core/tests/reference.py
```

No passing result here establishes actual NVIDIA symbol identities,
arbitrary loader namespaces, concurrent provider unload, real post-CUDA
fork support, CRIU restore, or successful model inference after restore.

## 9. Integration assumptions deliberately not treated as evidence

The composition source is
`schwinns/debug-full-merge-new-interposer` at
`e1a06b2c9a7ac776bae6f91493a4c5d01e02b40b`. It contains later NIXL read/H2D
pipelining and aligned reads not present in the older compression benchmark
branch. Historical standalone transfer results are not evidence that the
new composition captures and restores vLLM successfully.

The older optimized restore path executes coordination from the host
context. Composition must preserve its native-CUDA ordering without
regressing the newer workload namespace and trusted-executable-descriptor
contract. Both Rust libraries must be delivered and remounted, not only the
frontend. The shipped coordinator also needs static-link qualification.

Observed node driver/runtime labels do not settle CustomStorage or
`cuda-checkpoint --launch-job` support. Actual capability probes are still
required on both chosen nodes. Existing workloads and GPU reservations
have not been displaced to obtain a result.

The intended final evidence is a new composed revision, capture on node A,
restore on a distinct compatible node B, and verified post-restore inference.
That work has not yet been performed.

## 10. User-directed fork simplification and narrowed handshake diagnosis

The user explicitly chose the C shim's simpler quiescent-fork contract, while
retaining the run-ai ELF frontend and two-library packaging. The earlier
global-reader gate solved a broader constructor/concurrent-fork problem than
the supported workload requires, and its EAGAIN policy forced a test-only
coordinator-launch adapter. Those are rejected approaches, not inherent Rust
requirements. Their investigation and failed logs above remain history.

The current frontend no longer exports `fork`, and the private host/core ABI
is version 5 without enter/leave callbacks. CUDA/resolver wrappers have no
operation-guard plumbing. Atfork prepare locks initialization, STATE, export
cache, owned-socket registry, and frontend provider references in that order.
Cache transfers drain; socket creation/registration and removal/close share a
short metadata lock. Parent callbacks release locks without modifying state.
The child closes only shim-owned FDs, unmaps a saved carrier without CUDA
unregistration, and abandons the inherited CUDA-bearing generation. Its next
activity creates fresh state/cache locks and a random identity, ignoring the
parent's configured override. Process-lifetime mutexes acquired by the fork
thread are unlocked normally, never overwritten. The socket registry is
cleared before unlock so a nested fork cannot close a reused application FD.

This supports ordinary quiescent fork and pre-CUDA worker creation. Fork
inside interception or DSO initialization, concurrent CUDA or lifecycle
execution, and arbitrary foreign atfork-handler ordering are unsupported.
Blocking multicast calls release STATE; we deliberately do not add another
global counter to make concurrent fork safe around them. Fake-driver tests
can exercise a CUDA-using parent because their CUDA state is a CPU model.
They do not establish that an NVIDIA runtime initialized before fork is usable
in its child, nor override POSIX's async-signal-safety restrictions.

The prior permanent operation lease for unknown DMA completion was removed
with the gate. Successful copies remain asynchronous and batched, as in C.
If a failed copy/synchronization drains on the cleanup synchronization, normal
cleanup preserves the original error. If completion still cannot be
established, the shim writes a fixed diagnostic and calls `_exit(127)` without
normal Rust or CUDA cleanup. The agent must kill the remaining failed process
tree. This explicit fail-stop replaces quarantine records and fork containment,
not safe memory ownership: no potentially DMA-referenced memory is freed.
Persistent D2H/H2D tests now require fatal subprocess exit and fail separately
if any destructive cleanup callback runs.

The prestarted peer listener, eight-slot control queue, and independent control
worker remain. Startup rollback still detaches an already-started idle worker
rather than joining under a loader lock. No new handshake retries, readiness
acknowledgment, or pool was added.

### What the previous HANDSHAKE trace establishes

The targeted investigation reproduced the saved importer handshake timeout
only with ASan/UBSan-linked fixtures in the state→lifecycle sequence (attempts
21, 70, and traced attempt 68). Both importer threads were successfully created
but remained in futex waits with their inherited executable names. Neither
entered its Rust closure; the listener never polled or accepted. The coordinator
connected and sent the full request, then timed out receiving a reply.
Consequently this trace does not implicate header parsing, queue admission,
STATE acquisition by the worker, response serialization, or carrier transfers.

The equivalent truly unsanitized sequence passed 300/300 in that investigation.
The underlying sanitizer/runtime/old-gate interaction is **unknown**; removing
the old gate is not proof of its cause. The earlier untraced failure remains
unclassified. The diagnosis report and original `t-68.*` traces are copied
under `.cuinterpose-evidence/fork-simplification/`; the old
`.cuinterpose-evidence/control-startup/` bundle remains untouched.

The current pinned Makefile has a default sanitizer setting but honors an
explicit `SANITIZE=`. The reference runner now passes that assignment for its
default fresh build and verifies test linkage with readelf. `--sanitized`
requests separately labeled diagnostic coverage. This avoids calling a binary
“unsanitized” merely because its launch omits LD_PRELOAD.

### Validation of the simplified implementation

| Check | Result |
| --- | --- |
| Rust unit tests | 22 passed; the C interoperability test is selected separately |
| Loader/mock-core cases | 23 passed, down from 26 |
| Actual-core endpoint cases | 19 passed |
| Explicit Rust→C→Rust ticket exchange | Passed |
| Fresh unsanitized pinned reference against Rust | Coordinator 14, tracking 13, lifecycle 5, multicast 6 passed |
| Extra fake-driver modes | Multicast 8, carrier 22, fork 6, RPC 8 passed |
| Separate fresh sanitizer diagnostic | Same reference and extra modes passed once; not proof the old intermittent failure is fixed |

The three removed loader modes enforced recursive memory/resolver fork denial
or constructor-time fork EAGAIN. The removed extra fork modes were concurrent
CUDA/fork admission and a redundant child-constructor contention mode.
Same/cross-thread loader constructor reentry, including actual child endpoint
initialization after an ordinary fork, remains tested. The five coordinator
retry-adapter tests and their helper were deleted with the adapter. Thus this
is explicitly reduced fork scope, not a claim of unchanged coverage.

The initial unsanitized run passed; a second passed after making guard release
order explicit and replacing manual in-place guard drops with `Option::take`.
Neither was a retry of a failing suite. The single sanitizer diagnostic passed
separately. C implementation self-tests run during fixture construction are
not counted as Rust tests. Python multithreaded-fork warnings remain visible.
Logs, source/artifact hashes, and line-count baselines are in
`.cuinterpose-evidence/fork-simplification/`. The Rust `src/*.rs` footprint
decreased from 6,928 to 6,670 lines (258 fewer, including embedded unit tests),
while retaining the approved uncommitted carrier/control changes.

Focused independent review approved the simplified implementation. An
independent tester then repeated formatting, workspace unit tests, release
build, loader/endpoint tests, ticket exchange, the fresh unsanitized reference
suite, and `git diff --check`; all passed on the first supported-path run.
The optional sanitizer diagnostic was not part of that independent gate.

The artifact audit confirmed private ABI version 5, exactly 28 frontend dynamic
exports with no `fork` export, and only `cuinterpose_core_init` exported by the
core. The frontend retains `-Bsymbolic-functions` and has no dynamic relocations
to its own exported functions. The independently built release hashes were:

```text
b169c58ffc849ec6c2f0eeea8204650bc07d167bf777ca467828f6e2a502779e  libcuinterpose.so
980beeaf7c67478b351254cb0788081a18e1d6aec17849bdada8240e766648c9  libcuinterpose_core.so
37418aeae5b46808cad620694f002c772c99d04499bdb538ce1bd095c37df9c6  cuinterpose-coordinator
```

The tester recorded its evidence under
`.cuinterpose-evidence/final-simple-gate/` in its disposable validation
workspace, with evidence-manifest hash
`f8230276150fff74578d24923f4c3119bfdb42eaceb04b983e009b9a408e9f74`.
These are validation provenance, not downloadable repository artifacts.

This is not full parity or GPU evidence. Snapshot delivery and namespace
integration, static coordinator packaging, and two-node
capture/restore/post-restore vLLM inference remain the next work.

## 11. First Rust physical-GPU gate: three tests passed

On 2026-09-15 UTC, the unchanged seven-file GPU pytest suite from C reference
`21008b50b93a9879a805665e331e777bb93abf49` ran against the Rust frontend,
core, and coordinator built from
`41dd090a37ec21f3aead6fe5e7d3b1cfff1cb7b7`. The first pytest invocation
completed with **3 passed, 0 failed, 0 errors, and 0 skipped in 19.186 seconds**.
No production source or test assertion was changed, and no CUDA operation,
phase, or suite retry was used.

| Test | Time | Observed coverage |
| --- | ---: | --- |
| `test_checkpoint_restores_multicast_group` | 7.799 s | Real multicast object/device/binding/mapping records in both ranks, BindAddr rebind, native CUDA checkpoint/restore, and post-restore collective/graph replay |
| `test_checkpoint_restores_shared_posix_memory` | 6.601 s | Ticket-backed peer imports and mappings, shared host-carrier save/load, original-address byte verification, and never-exported native-owned VMM property/retain/cleanup checks |
| `test_prepare_is_refused_while_a_raw_import_is_alive` | 3.251 s | Prepare refusal while a raw import remains live, no state-file publication, and continued usable workload state |

Persisted multicast topology contains one multicast object, device attachment,
binding, and mapping per participant. Each participant also has five unicast
allocation and mapping records and two content allocations. The separate
unicast case has six allocation and mapping records per participant, including
two imports and three content allocations. Thus multicast coverage was not
inferred merely from wrapper presence or a successful workload launch.

The POSIX test reported 545,259,520 carrier bytes, aggregate copy throughput
109.82 GB/s, and whole-phase throughput 63.27 GB/s. Its single-GPU pinned-copy
baseline was 55.36 GB/s; the existing 0.8-times-baseline assertion passed
unchanged. The coordinator divides total bytes across ranks by the longest
reported per-rank copy duration. This is an aggregate two-rank number, not
evidence of a twofold per-GPU improvement or a controlled performance benchmark.

### Environment and provenance

The run used the host `schwinns` namespace on nscale-dev, node
`cluster-0967a26d-pool-14bee067-prctr-tx5tk`, with two DRA-allocated B200 GPUs
connected by `NV18`. The GPUs were
`GPU-390c745d-b113-45b5-8d1a-7873a74d8a29` and
`GPU-9702d531-4c6f-99a0-eab6-1f7438365df4`.
The installed driver was 595.58.03; `nvidia-smi` reported CUDA 13.3.
The separately staged `cuda-checkpoint` utility reported 610.43.02 and came
from upstream revision `00d5cce84c628088d6caa203fc4af40c1538b6f7`.
The actual `--launch-job` and native checkpoint APIs worked on this node;
this does not establish CustomStorage or a second node's capabilities.

The image was
`nvcr.io/nvidia/pytorch@sha256:43c018d6a12963f1a1bad85ef8574b5c2a978eec2be0ebcacfb87f69e0d210e1`,
not a Dynamo runtime image. A uv 0.11.28 virtual environment inherited the
image's PyTorch `2.13.0a0+8145d630e8.nv26.06` and installed pytest 8.4.2 and
cuda-bindings 13.3.1. This deliberately reused the image's CUDA-enabled
PyTorch rather than resolving the source pyproject's torch 2.11.0 pin.
The test source files were unchanged and verified against the reference Git
objects. Seed was 41221090, with 256 MiB large carrier allocations per rank.
Three harmless pytest warnings concerned `record_property` with xunit2 JUnit.

The GNU-target Rust artifacts were rebuilt with `/usr/bin/gcc` as linker.
Their hashes exactly matched the independent fake-driver gate in section 10
and were verified inside the pod before and after testing. Both Rust libraries
were staged as siblings. No C production cuinterpose binary was used.

### Setup attempts and cleanup

The prior tester's combined apply/wait command had timed out locally, but its
job had successfully scheduled and was still waiting for a payload sentinel.
Its disposable evidence directory was no longer accessible during takeover;
the payload was therefore reproduced in the parent workspace and verified
against the recorded hashes. That initial failed copy was a setup issue,
not a product or pytest failure.

Before starting any tests, only the owned waiting job was recreated to use
a uv-managed environment and hold its results for collection. The original
two-GPU claim was retained. pip bootstrapped uv; uv installed the test
dependencies. The replacement job ran once. Tar omitted stale Unix socket
paths during evidence collection, as expected; regular files, state records,
JUnit, and logs were preserved. Both the job and its claim were deleted after
evidence collection, without touching other workloads.

The private, parent-workspace evidence is under
`.cuinterpose-evidence/rust-gpu-41dd090/`: source/artifact checksums, launcher,
manifests/events, environment audit, pytest/run logs, JUnit, checkpoint state
files, and decoded topology counts. Successful coordinator stdout is consumed
by the unchanged test harness, so full phase reports are not separately logged;
the tests assert their success and content counts, and print transfer metrics.

This gate restores CUDA state in the same worker processes. It does not run
CRIU, the Snapshot agent/operator, or vLLM, and it does not move the workload
between nodes. Those remain separate required integration tests.

## 12. Native Snapshot integration after `3184500`

The working tree ports tool delivery, annotation shaping, and coordinator
orchestration from C-stack commits `3f12b0c`, `f4963ad`, and `8ca6275`, without
importing a C cuinterpose library or coordinator. Main's host/image/GPU
compatibility metadata and process/artifact identity checks are retained.
The old plan named Go 1.26.6, but the actual baseline pins 1.27.1 everywhere;
this integration does not change those pins or the agent base image.

Delivery copies the NVIDIA executable and both Rust libraries into every
target. Annotation opt-in affects only frontend preloading. Multi-GPU or
unknown DRA counts independently select `--launch-job`. Restore rejects a
prepared artifact without CUDA metadata, requested interposition, delivered
tools, or its state file; the mount helper verifies all three tool files before
attempting namespace or mount syscalls.

### Build failures and corrections

The first pinned Rust builder used Debian bullseye. Its security repository
advertised several package versions whose downloads returned HTTP 404.
Switching the repository transport from HTTP to HTTPS did not fix that.
The builder now uses digest-pinned `rust:1.95.0-slim-bookworm`; the actual
GNU outputs still pass the strict maximum `GLIBC_2.34` gate. This is not a
relaxation of the workload libc baseline. The agent's NGC base remains unchanged.
An ignored local GPU-test virtual environment initially enlarged the Docker
context by gigabytes; `agent/.dockerignore` now excludes virtual environments,
Cargo outputs, and test caches.

Building the coordinator for musl exposed an actual portability defect:
`msghdr.msg_controllen` and `cmsghdr.cmsg_len` are not `usize` on musl. Eleven
compile errors prevented a static binary. Fixed-size ancillary lengths now
convert to the platform field type, and received lengths convert to `usize`
for buffer arithmetic. GNU and musl protocol tests, including SCM_RIGHTS
descriptor transfer, pass. The wire layout and phase semantics did not change.

Ported tests initially used the older manifest constructors without main's
host/GPU compatibility arguments. These were updated rather than removing
the compatibility fields. Two local patch-placement mistakes temporarily
inserted tests inside helper functions; formatting caught them before tests
ran, and their function boundaries were corrected.

### Namespace-root correction

The C-reference prepare path pinned namespace descriptors but did not change
filesystem root or cwd. `setns(CLONE_NEWNS)` alone does not do that, so direct
`/snapshot-control` lookup could still resolve through the agent root.
Prepare now additionally pins `/proc/<pid>/root` and passes `--root` and `--wd`
through that descriptor. Restore does the same, and pins the remaining four
namespace descriptors rather than resolving them later through `nsenter -t`.
The coordinator executable and prepare checkpoint directory remain trusted
open descriptors. No host-side coordinator shortcut was introduced.

### Local checks and remaining gates

The pinned container produced GNU frontend/core libraries and a musl static
coordinator. Artifact checks verify exact dynamic exports, modes `0644` and
`0755`, glibc symbol versions, no CUDA/runtime linkage, and no coordinator
`PT_INTERP` or `DT_NEEDED`. The coordinator's CLI/phase test runs against the
release musl executable, covering prepare, state publication, restore, and
every multicast suboperation through real Unix sockets with an empty
participant. It validates orchestration, not CUDA behavior.

| Command | Result |
| --- | --- |
| `make -C agent cuinterpose-build` | Passed in the pinned container |
| `make -C agent cuinterpose-test` | GNU workspace tests, 23 loader cases, 19 actual-core endpoints, static release CLI test, and 9 musl protocol tests passed |
| `go test ./api/... ./agent/... ./operator/...` | Passed |
| `make -C agent go-build` | Agent and nsrestore compiled |
| `helm lint charts/snapshot` | Passed |
| `helm unittest charts/snapshot` | 6 suites, 15 tests passed |
| `git diff --check HEAD` | Passed |

Logs are in the parent workspace under `.cuinterpose-evidence/integration-*`,
including failed build attempts. No cluster jobs were launched in this task.
The complete agent/operator images, root `make check`, full fake-driver
reference suite against the new artifacts, and native two-node CRIU/vLLM
capture/restore/inference remain validation gates. The three real-GPU results
in section 11 belong to `41dd090`, not these newly packaged artifacts.
CustomStorage/NIXL composition remains separate future work.

## 13. Packaged tests and first native cross-node vLLM attempt

On 2026-09-15 UTC, testing proceeded without a review gate, at the user's
direction. The integration remained an uncommitted diff atop `3184500`.
The complete fake-driver reference suite passed against the **packaged GNU
libraries and static musl coordinator**, selected with the new
`reference.py --artifacts` option rather than the runner's default GNU rebuild.
The artifact audit, all three Go module suites, 41 framework/workload Python
tests, and six Helm suites containing 15 tests passed.

Root `make check` first found one overlong newly added test line. After wrapping
that line, generation, licensing, formatting, tidy, lint, vulnerability checking,
and Helm lint passed. The final clean-tree assertion correctly failed because
this integration has not been committed. Agent tests and lint passed again
after the mount-compatibility fix below.

### Full images and setup corrections

The first agent image command omitted the named `api` build context; the next
omitted `compliance`. Docker consequently tried to pull those names as images.
Using the canonical root Makefile's two named contexts built the full agent
successfully. The operator image also built. Both were pushed under unique
`nvcr.io/nvidian/dynamo-dev/schwinns` tags:

| Image tag | Manifest digest |
| --- | --- |
| `rust-native-agent-3184500-20260915` | `sha256:6f3863d4201254d925c8e1688ab208ace837368cc51dafcf5ee93e6cae07f0eb` |
| `rust-native-operator-3184500-20260915` | `sha256:4c5d37c3abeb03e801821fd7714da30b31544e383b55ddf95ac7f33adbe36d98` |
| `rust-native-agent-3184500-20260915-mountfix` | `sha256:f3009c067c72e043ab66c85a4ba7af677c7e559b3bfa6da8311b01e767a07794` |

Image inspection verified both library locations and mode `0644`, and both
coordinator locations and mode `0755`. The Rust artifact hashes match section
12's packaged build. Running `cuda-checkpoint --version` in the local
non-GPU container failed to load `libcuda.so.1`; this was not treated as a
runtime capability result. The shipped tool subsequently performed native CUDA
capture successfully on the source node.

The test reused the guide's pinned vLLM 0.27.1 image and existing framework
pytest assertions. A private adapter selected TP2, two DRA GPUs, the existing
NFS model cache, and the actual Go pod-contract shaping functions for source
tool delivery and preload. It changed `tensor_parallel_size` to two in the
guide program but did not weaken inference, checkpoint, or restore assertions.
The source ran on `tx5tk` and the destination was explicitly `l9nsv`.
The reusable framework test now accepts `SNAPSHOT_E2E_RESTORE_NODE`, asserts
it differs from the source, and verifies actual destination placement.

The owned agent installation was updated with `OnDelete` scheduling. Only the
idle test-node agents were replaced; the existing `s2877` agent and GLM workload
were not restarted. Original agent/operator templates were saved first.
An initial merge patch omitted rather than nulled `affinity`, leaving the old
`l9nsv` exclusion in place; explicitly clearing that field enabled its agent.

### Successful source capture, followed by a real integration refusal

Run `rust-native-vllm-d9c1cf` generated before capture, slept, and completed
Rust prepare, native CUDA checkpoint, and CRIU dump. Capture took **46.737 s**:
CUDA checkpoint 2.881 s, cuinterpose prepare 0.465 s, CRIU dump 42.927 s.
The coordinator inspected four participants and 1,060 records, with zero raw
imports and unsupported creations, and saved 136 allocations totaling
2,109,734,912 bytes.

Each of the two workers recorded 257 unicast allocations and mappings,
including 67 imported allocations and 68 content allocations, plus four
multicast objects, devices, bindings, and mappings. The two other participants
had no topology records. vLLM reported FlashInfer attention with the TRTLLM
decode backend and its MNNVL allreduce/norm fusion workspace. Thus real
unicast and multicast coverage was established before capture.

The source GPUs were `GPU-390c745d-b113-45b5-8d1a-7873a74d8a29` and
`GPU-9702d531-4c6f-99a0-eab6-1f7438365df4`. The destination selected
`GPU-fd77376f-599d-a407-48c4-e89927e80d3f` and
`GPU-f9f66bb6-1527-586f-d438-aca8e5fecba0`. Both nodes used driver 595.58.03.
The destination image pull took about 214 seconds; no CUDA/CRIU restore
occurred during that wait.

Once the placeholder started, the agent refused restore:

```text
gate=inspect reason="mount: source /tmp/snapshot-cuda, target missing"
```

The new-main compatibility check runs before restore installs Snapshot's own
tools mount. The C-derived integration had not accounted for that ordering.
The correction removes **only** `/tmp/snapshot-cuda` from the pre-existing
mount requirement when the manifest records `cudaTools.delivered: true`.
The subsequent mandatory mount step still checks every tool file and fails
if installation fails. Tests confirm an unmanaged tools mount or any missing
workload mount still fails compatibility inspection. No compatibility bypass
annotation was used.

The first pytest invocation failed after 474.46 seconds. Its visible Pod
condition was overwritten with `SnapshotPending` by a host-side controller;
the Rust destination agent log retains the concrete compatibility refusal.
The failed JUnit and logs are preserved, not replaced by a later success.

### Corrected rerun blocked by destination DiskPressure

The fixed agent was built, pushed, and passed the focused regression and agent
suite. Before a second pytest invocation could start, kubelet evicted the
replacement `l9nsv` agent with `DiskPressure` and
`Init:ContainerStatusUnknown`. The node's condition changed at
2026-09-15 05:09:31 UTC. The launcher's readiness check failed because the
evicted agent had already been replaced; **no second workload capture or
restore test ran**. We did not delete node data, remove other images, or evict
other workloads to force progress.

The original agent/operator templates were verified unchanged apart from our
patches and restored exactly. The old `tx5tk` agent image was restored and
ready; `s2877` remained untouched. Both test GPU claims, the refused restore
Pod, and the private app ConfigMap were deleted. The successful source
checkpoint remains for diagnosis:

```text
PodSnapshot: rust-native-vllm-d9c1cf-snapshot
PodSnapshotContent: podsnapshotcontent-42f64f2f-d68a-4d47-b442-930832c458eb
Content UID: 331b35e0-e6ac-4e0f-9863-0cd43da5d029
Artifact: /checkpoints/artifacts/331b35e0-e6ac-4e0f-9863-0cd43da5d029/containers/main
```

Evidence, manifests, state, source hashes, image digests, test adapters, logs,
failed JUnit, node pressure, and rollback checks are retained privately in
`.cuinterpose-evidence/rust-native-e2e/`. There is **no successful cross-node
restore or post-restore inference result yet**. Resolve destination disk
pressure or choose another safe compatible node before resuming the two-node
vLLM test.

## 14. Native GLM 5.2 TE8 cross-node test passed

On 2026-09-15 UTC, the requested GLM test ran without another review or broad
preflight gate. It used `nvidia/GLM-5.2-NVFP4`, vLLM 0.27.1, tensor parallel
size eight, expert parallelism enabled, and spawn workers. `moe_backend='auto'`
selected `FLASHINFER_TRTLLM` automatically; no MoE backend override was applied.
The source used all eight B200 GPUs on `tx5tk`; the destination used all eight
on `s2877`. Both nodes ran driver 595.58.03.

The existing framework pytest assertions passed: pre-capture generation,
snapshot Ready, source deletion, actual placement on a distinct node,
RestoreSucceeded, post-restore ready sentinel, fresh HTTP `/generate`, and no
placeholder cold-start output. JUnit reports **one passed, zero failures,
errors, or skips in 1,887.244 seconds**. Two marker-registration warnings arose
because the private adapter's pytest root is outside the e2e project config.
The test uses a directly created source Pod and PodSnapshot, not SnapshotJob.

The run was `rust-glm52-te8-8f73f1`. Pre-capture output was
`I am ready to assist you.`; the restored process generated
`I have a question for you.`; the fresh HTTP response was
`(No need to mention the worker's`. The example caps outputs at eight tokens:
these nonempty responses establish inference continuity, not answer quality.

### Exact build and topology

The integration remained uncommitted atop `3184500`. Agent digest was
`sha256:f3009c067c72e043ab66c85a4ba7af677c7e559b3bfa6da8311b01e767a07794`;
operator digest was
`sha256:4c5d37c3abeb03e801821fd7714da30b31544e383b55ddf95ac7f33adbe36d98`.
The workload image was
`vllm/vllm-openai:v0.27.1-ubuntu2404@sha256:dafea057f24b7d42716331a48e2db4e1f204f877a3aa759cb7e4c37e64ca2eee`.
Both test-node agents were replaced with the Rust mount-fix build before launch.
The GNU frontend/core and static musl coordinator were the packaged section-12
artifacts; no C production shim or coordinator was substituted.

Rust inspected ten participants and 8,624 records. Eight workers each held
527 unicast allocation and mapping records, including 149 imports and 134
content allocations. Each worker also had six multicast objects, devices,
bindings, and mappings. Two participants had no topology records.
Capture saved 1,072 creator allocations totaling 15,904,800,768 bytes, with no
live raw imports or unsupported creations. Restore handshake, content load,
unicast replay, multicast replay, and final topology validation all succeeded.

| Phase | Time |
| --- | ---: |
| Complete capture | 1,099.479 s |
| Rust prepare | 1.685 s |
| Native CUDA checkpoint | 37.816 s |
| CRIU dump | 1,059.624 s |
| Complete external restore | 124.883 s |
| CRIU restore | 52.455 s |
| Native CUDA restore | 70.412 s |
| Rust reconstruction | 1.481 s |

### Cache omission, correction, and observed wakeup delay

The initial source `rust-glm52-te8-e570b9-source` mounted the model cache but
omitted persistent compiler/JIT/autotune paths. This was a test-configuration
mistake. At the user's question, its driver was interrupted before capture,
logs were retained, and existing completed vLLM cache files were copied with
no overwrite into the shared cache. The source was deleted and a new test
invocation started; no CUDA lifecycle failure was retried or hidden.

Both source and restore now mount `/model-cache` and the existing FlashInfer
cache subpath. Environment explicitly points vLLM/torch.compile, TorchInductor,
Triton, DeepGEMM, CUDA, XDG, FlashInfer JIT, and FlashInfer autotune caches under
`/model-cache/glm52-cache`. Cache keys changed with the image/configuration, so
mounting them did not eliminate compilation. FlashInfer tuned 22 new entries
and saved them under the shared `vllm/flashinfer_autotune` directory.

The app used `pause_generation(); sleep()` (default level one), then full
`wake_up()` after restore. It did **not** wake only weights before capture.
Each rank backed about 55.35 GiB of model weights into CPU memory, contributing
to the large CRIU dump. This run used native CUDA storage and CRIU, not
CustomStorage/NIXL/compression or the keep-weights optimization.

External restore completed at 06:06:22.954 UTC, but the vLLM ready sentinel
arrived at 06:09:15.655 UTC. Seven GPUs had about 128 GiB allocated while the
last still had about 7 GiB. Focused Python stacks showed the engine waiting
for wakeup RPC responses; later stacks showed the workers back in their
message loops. All eight GPUs ultimately reached 128,094 MiB and inference
passed without a production change. The exact reason for the last worker's
longer delay was not established. A native py-spy unwind failed with
`UNW_EBADREG`; ordinary Python stack sampling succeeded. The diagnostic
binary was staged only in the temporary test-agent container.

The checkpoint is retained as `rust-glm52-te8-8f73f1-snapshot`, content
`podsnapshotcontent-1172346f-a9c0-46da-a6b0-e5cdca2b2696`, content UID
`ca68f5ec-08bd-4bad-bae2-baa052797452`. Private evidence is under
`.cuinterpose-evidence/rust-glm52-te8-e2e/`, including the adapter/launcher,
both attempts' logs, JUnit, original/patched deployment templates, GPU claims,
manifest/state/topology, inference sentinels, timings, and cleanup logs.
The restored Pod, both test GPU claims, and private app ConfigMap were deleted
after evidence collection. Original agent/operator templates were restored
only after comparison with the test's patches; original agents were Ready on
both test nodes. The previously suspended 50-hour-old GLM DGD worker remains
at zero replicas rather than being restarted without direction.
CustomStorage/NIXL composition remains a separate, untested next step.
