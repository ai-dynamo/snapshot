<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Cuinterpose: checkpoint and restore of shared CUDA memory

Cuinterpose adds support for CUDA memory shared between processes on one node: POSIX-exported virtual memory allocations, supported legacy memory-IPC calls, and multicast objects. After restore, application pointers must still address the same bytes, and processes that shared an allocation must again refer to the same physical allocation.

Saving each process independently is not enough to reconstruct these relationships. A saved CUDA descriptor does not name a fresh allocation on the destination node. Multicast objects also depend on their member allocations and device attachments. Without support for those resources, native checkpoint can refuse the workload; merely restoring independent copies would not restore sharing.

Cuinterpose records the relationships while the application runs. Before native checkpoint, it copies shared allocation bytes to host memory and removes the corresponding CUDA mappings and handles. After native restore, it creates new shared allocations, restores their bytes, and reconnects the processes.

**Host carriers are the shim's only storage implementation.** A host carrier is a buffer inside the creator process containing its shared allocation bytes. CRIU saves that buffer with the process's other host memory. Never-shared allocations remain the native CUDA checkpoint implementation's responsibility. PageBroker and native CustomStorage are separate work: the shim has no PageBroker connection, storage selector, or allocation-session protocol.

## Components

```mermaid
flowchart TB
    Agent["Snapshot agent"]
    subgraph Pod["Workload Pod"]
        subgraph Container["Checkpoint target container"]
            Root["Supervisor process"]
            subgraph A["Worker process A"]
                AppA["Application threads"]
                ShimA["cuinterpose shim A"]
                SocketA["Control socket A"]
                AppA --> ShimA
                ShimA --- SocketA
            end
            subgraph B["Worker process B"]
                AppB["Application threads"]
                ShimB["cuinterpose shim B"]
                SocketB["Control socket B"]
                AppB --> ShimB
                ShimB --- SocketB
            end
            Root --> AppA
            Root --> AppB
            Coordinator["cuinterpose-coordinator"]
            Coordinator <-->|"commands and replies"| SocketA
            Coordinator <-->|"commands and replies"| SocketB
            SocketA <-->|"on-demand peer FD requests"| SocketB
        end
        Control["Per-container directory in snapshot-control emptyDir"]
        SocketA --- Control
        SocketB --- Control
    end
    Agent -->|"launch in target namespaces"| Coordinator
```

### Cuinterpose shim

The shim is loaded inside each CUDA process, not run as a sidecar. Its C frontend, `libcuinterpose.so`, receives intercepted calls. Its Rust backend, `libcuinterpose_core.so`, owns allocation state, mappings, cached export descriptors, and host carriers. CUDA calls execute inside the process that owns the CUDA contexts.

Each shim listens on `/snapshot-control/cuinterpose-<namespace-pid>.sock`. The same Unix socket accepts coordinator commands and peer requests for export descriptors. Peer connections are opened when needed; there is no permanent all-to-all connection set.

The backend separates CUDA API policy from resource ownership:

| Module | Responsibility |
| --- | --- |
| `handlers.rs` | Argument validation, native versus tracked decisions, and API-level orchestration. |
| `driver.rs` | Real CUDA calls and temporary context switching. |
| `runtime/` | Generation installation, sticky failure, registered sockets, fork cleanup, and control workers. |
| `memory/mod.rs` | The resource registry, virtual handle ownership, and tracked address ranges. |
| `memory/vmm.rs` | Unicast backing adoption, retain/map bookkeeping, and access permissions. |
| `memory/sharing.rs` | Shareable-handle encoding, exact-PID peer requests, cached exports, and imports. |
| `memory/ipc.rs` | Malloc reservations, IPC-handle encoding, repeated opens, and synchronized release. |
| `memory/multicast.rs` | Multicast lifetime, unlocked collective calls, bindings, and reconstruction. |
| `memory/checkpoint.rs` | Inspection, local phase validation, checkpoint mutation, and completion. |
| `memory/host_carrier.rs` | Saving and loading shared allocation bytes. |

Handlers do not call other CUDA handlers. They share memory operations that own
their bookkeeping. Operations that release the registry lock for a
blocking CUDA call use one process-wide `unlocked_driver_calls` counter. Peer FD service
uses only the export cache lock, never the registry lock.

The control worker routes requests and sends replies; checkpoint code owns phase
transitions and fail-stop decisions. After sending a successful LOAD reply, the
worker reports completion to checkpoint code so it can release the host carrier.

### Cuinterpose coordinator

The Rust `cuinterpose-coordinator` is a short-lived executable. One instance runs before native capture; another runs after native restore. It reads state entries from all shims, checks that creators and importers agree, and starts each capture or restore phase. It does not call CUDA or copy allocation bytes.

For each phase, it sends requests to the participants concurrently and waits for every reply before starting the next phase. This matters for multicast calls that need other ranks to make progress.

There are no identity or rendezvous messages. Preparation sends four coordinator
requests to each process: `BEGIN_CHECKPOINT`, `PREPARE_MULTICAST`, `SAVE_ALLOCATIONS`,
and `PREPARE_UNICAST`. Restore sends eight: an initial `INSPECT`,
`LOAD_ALLOCATIONS`, `RESTORE_UNICAST`, the four ordered multicast operations,
and a final `INSPECT`. Restoring an imported allocation or multicast object also
causes one peer export request to its creator; that data-dependent traffic is
the actual FD handoff, not discovery.

### Snapshot agent

The agent finds CUDA processes, verifies that shim sockets agree with the workload annotation, and launches the coordinator in the target namespaces. It then sequences native CUDA checkpoint/restore and CRIU. After restore, it releases the application's restore-complete wait only when native CUDA and cuinterpose reconstruction both succeed.

### Host-carrier module

The Rust `host_carrier` module copies shared creator allocations into one host arena per process and copies them back during restore. It registers the arena as pinned host memory, groups allocations by CUDA context, maps a temporary consecutive device-address range, and uses asynchronous copies on one stream per context. It waits for those copies before reporting completion. The temporary device mappings are not application mappings.

## How calls are intercepted

The frontend handles direct CUDA symbol calls, `dlsym`, `cuGetProcAddress*`, and the CUDA runtime's `cudaGetDriverEntryPoint*` queries. A resolver query calls the real resolver first. Only a successful lookup of a supported API is replaced, using the actual returned symbol to choose the correct calling convention.

The frontend obtains glibc's real `dlsym` with `dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34")`. On first relevant CUDA activity it loads the adjacent Rust backend with `RTLD_LAZY | RTLD_LOCAL`. This is glibc-based lookup, not a custom ELF loader. The frontend's linker export list exposes only its intended CUDA/resolver functions; `-Bsymbolic-functions` keeps its own internal function references local.

The private C ABI contains `FrontendAbi` and `BackendAbi` tables. Cbindgen generates the table declarations, while NVIDIA's `cuda.h` supplies C CUDA types and cudarc supplies the corresponding Rust definitions. Backend driver calls use the frontend resolver; cudarc's loader and buffer/context wrappers are not used. Rust objects and ownership do not cross the ABI.

Loading the backend and starting its runtime are separate operations:

| Operation | Concurrency and lifetime |
| --- | --- |
| ABI registration (`cuinterpose_core_init`) | Copies the frontend table and returns an immutable backend table. Repeated or concurrent registrations must agree on the resolver and origin PID. It starts no workers and makes no frontend callbacks. |
| Frontend publication | Concurrent callers may each `dlopen` the backend; glibc serializes its construction. Atomic publication retains one process-lifetime library reference and closes redundant references. Same-thread constructor reentry returns `CUDA_ERROR_NOT_INITIALIZED` without poisoning a later call. |
| Runtime startup | CUDA callbacks and `ensure_cuinterpose_initialized` prepare private candidates, then install one process generation, control socket, and worker pair. Concurrent callers reuse the installed runtime; same-thread preparation reentry returns `CUDA_ERROR_NOT_INITIALIZED` without poisoning it. |

There is no frontend-wide loading lock. The backend's generation lock remains
necessary for unique runtime resources and quiescent-fork coordination.
Obtaining the ABI table alone does not mean runtime services are ready.

### Backend runtime installation

```mermaid
flowchart TD
    A["Prepare private tracking state and channels"] --> B["Spawn workers parked on empty channels"]
    B --> C["Acquire installation mutex"]
    C --> D{"Failed runtime / healthy winner / no winner?"}
    D -->|failed| E["Return sticky failure"]
    D -->|healthy winner| F["Reuse installed runtime"]
    D -->|no winner| G["Bind, configure, activate listener; publish state"]
    E --> H["Unlock, then discard private candidate"]
    F --> H
    G --> H
```

Thread creation can register Rust TLS destructors with glibc's loader. A CUDA
caller in a DSO constructor already holds that loader lock. Preparation therefore
owns no installation mutex: that caller can prepare its own candidate instead
of waiting for another thread blocked in loader-sensitive startup. Installation
never waits for a worker-running acknowledgment. Readiness means successful
thread creation, not proof that a worker has already been scheduled.

Only the installer binds the canonical endpoint. One nonblocking listener
handoff activates the peer worker, which owns the control queue's only sender.
Discarding a candidate disconnects that chain so both workers eventually exit;
callers never join them. Cleanup, unlinking, logging, and candidate destruction
happen outside the installation mutex. There can temporarily be two private
workers per contender, but only one installed pair.

Installed failure is checked before installed state. A failed private candidate
may reuse an already-installed healthy winner; it never waits for an unfinished
candidate. Without a winner, a real preparation or installation error remains
sticky. No call bypasses tracking to reach CUDA after initialization failure.

The bounded commit path is audited for the pinned Rust/Linux/glibc implementation:

- Backend ELF eager binding prevents first-use PLT lookup while holding the mutex.
- The preparation guard initializes non-destructible backend TLS before commit.
  Rust futex mutexes and panic-count TLS do not register loader destructors.
- The capacity-one activation channel is preallocated. `try_send` never enters
  blocking-send context initialization; wakeups use non-Drop TLS and futex
  unparking. Receiver context setup happens before its waker lock is held.
- Bind/listen, nonblocking setup, chmod, and descriptor registration make no
  frontend callbacks or formatted/logging calls. The descriptor registry can
  grow its Vec using ordinary glibc allocation; arbitrary allocator/libc
  interposers that call the loader are outside this assumption.

Fork remains supported only with CUDA/lifecycle calls and active RPC quiescent.
Retiring private workers have no generation reference or socket. This does not
promise safe arbitrary fork during preparation or reclaim every inherited
allocation belonging to a vanished thread.

Rust panics at backend entry points terminate the process without unwinding through the C ABI. The shim cannot continue after a panic that may have interrupted a state-changing operation. This does not make invalid application pointers, foreign C++ exceptions, or allocator aborts recoverable.

| Intercepted APIs | What the shim does |
| --- | --- |
| `cuInit` | Initializes the shim alongside CUDA. |
| `cuMemCreate`, `cuMemRelease`, `cuMemRetainAllocationHandle` | Tracks supported VMM allocations and translates application-visible virtual allocation handles to the CUDA driver handle. |
| `cuMemMap`, `cuMemUnmap`, `cuMemSetAccess` | Records address ranges, allocation offsets, and access permissions. |
| `cuMemGetAllocationGranularity`, `cuMemGetAllocationPropertiesFromHandle` | Preserves CUDA queries while resolving tracked handles where required. |
| `cuMemExportToShareableHandle`, `cuMemImportFromShareableHandle` | Replaces raw export FDs with virtual shareable handles and imports the creator's real allocation through a peer request. |
| `cuMemAlloc_v2`, `cuMemFree_v2`, `cuMemGetAddressRange_v2` | Implements synchronous device malloc with VMM backing, frees it, and reports the application's requested range. |
| `cuIpcGetMemHandle`, `cuIpcOpenMemHandle`, `cuIpcOpenMemHandle_v2`, `cuIpcCloseMemHandle` | Implements supported memory IPC using the same VMM records and peer export service, without native memory-IPC calls. |
| `cuMulticastCreate`, `cuMulticastAddDevice`, `cuMulticastBindMem*`, `cuMulticastBindAddr*`, `cuMulticastUnbind`, `cuMulticastGetGranularity` | Tracks multicast objects, device membership, bindings, and their reconstruction. |

Modern proc-address aliases for malloc/free/address-range queries are handled without treating historical 32-bit ELF entry points as modern APIs.

Explicit `dlvsym` calls and CUDA libraries loaded in separate linker namespaces bypass this interception. Arbitrary chains of other `dlsym` interposers are not supported: forwarded `RTLD_NEXT` lookup is relative to this frontend, not the original caller.

## Workload operation before capture

### Process identity and allocation ownership

The namespace PID is the shim process identity and directly determines its
socket name: `/snapshot-control/cuinterpose-<namespace-pid>.sock`. A separate
random 128-bit allocation ID identifies each tracked allocation or multicast
object. Together, the creator namespace PID and allocation ID identify an
allocation across processes.

The agent supplies one `--process <namespace-pid>` argument for every expected
CUDA process. Because the coordinator runs inside the target PID and mount
namespaces, it can connect to each exact socket without scanning the control
directory or asking shims to identify themselves. Its first capture request is
`BEGIN_CHECKPOINT`; restore also requires the supplied namespace PID set to match the
keys saved in `cuinterpose.state`.

CRIU preserves namespace PIDs, allocation IDs, state records, virtual allocation
handles, and virtual handle bytes in process memory. A forked child gets its own
namespace PID and listener, while restore recreates the captured namespace PID.

For example, suppose A creates a 2 MiB allocation and B imports it:

| | Worker A | Worker B |
| --- | --- | --- |
| Namespace PID | `41` | `42` |
| Allocation ID | `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa` | The same ID |
| Role for this allocation | Creator | Importer |
| Application mapping | `0x700000000000` | `0x710000000000` |

The addresses can differ: each process has its own address space. The shared allocation ID says that both mappings refer to the same backing. After restore, those addresses and IDs stay the same, but the driver handles and export FDs are newly created.

An allocation is *exportable* when its creation properties allow a shareable handle. That does not mean it needs a host carrier. It becomes *shared* after a successful tracked export/import or a binding into tracked multicast. Once marked shared, closing a virtual shareable handle or removing a binding does not change it back to private.

| Allocation state | Who saves and restores its bytes? |
| --- | --- |
| Ordinary nonexportable `cuMemCreate` | Native CUDA. |
| Tracked POSIX-capable allocation never shared | Native CUDA; cuinterpose leaves its driver handles and application mappings intact. |
| Shim-managed malloc never shared | Native CUDA, even though its backing can be exported. |
| Shared creator allocation | The creator shim's host carrier. |
| Imported shared allocation | No second content copy; the importer reconnects to the creator's restored allocation. |
| Tracked allocation used by multicast | The creator's host carrier, even if there was no unicast virtual shareable handle export. |

Only pinned, device-located, supported exportable creator allocations marked shared enter the host carrier. A shared allocation whose application handles were released but whose mapping remains can recover a temporary driver handle before copying. Never-shared allocations are excluded from that recovery as well as from teardown.

### VMM export and import

For POSIX export, the shim caches a real CUDA export FD and returns a memfd
virtual shareable handle instead. The application passes that FD through
its existing communication mechanism. The virtual shareable handle has one
fixed 24-byte layout:

| Bytes | Value |
| --- | --- |
| 4 | `CUI\x02` |
| 4 | Creator namespace PID, little-endian |
| 16 | Allocation ID |

The last two fields form an `AllocationReference`. A virtual shareable handle
contains neither a socket path nor a unicast/multicast discriminator. The
importing shim derives the creator's exact socket path from its namespace PID.

On import, B sends A the allocation reference:

```yaml
version: 2
body:
  kind: export
  allocation:
    id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    creator_pid: 41
```

A sends a duplicate of its cached FD using `SCM_RIGHTS` and replies with either
`unicast_export` or `multicast_export`. Only the multicast reply includes
`CUmulticastObjectProp`: CUDA exposes no equivalent query after multicast
import. A unicast importer obtains the authoritative `CUmemAllocationProp`
directly from CUDA after importing the FD. B records the resulting local
handle. Re-exporting B's import still names A. Neither the virtual shareable
handle nor the request contains a durable CUDA FD number.

```mermaid
sequenceDiagram
    participant A as Application A
    participant SA as Cuinterpose shim A
    participant B as Application B
    participant SB as Cuinterpose shim B
    participant CUDA as CUDA driver
    A->>SA: Export virtual allocation handle
    SA->>CUDA: Export real FD if not cached
    CUDA-->>SA: Real CUDA FD
    SA-->>A: Virtual shareable handle FD
    A->>B: Pass virtual shareable handle through application IPC
    B->>SB: Import virtual shareable handle
    SB->>SA: Request creator and allocation ID over UDS
    SA-->>SB: Cached FD via SCM_RIGHTS
    SB->>CUDA: Import real FD
    CUDA-->>SB: Local driver handle
    SB-->>B: Virtual allocation or multicast handle
```

A POSIX import whose FD is not a virtual shareable handle returns `CUDA_ERROR_NOT_SUPPORTED` before calling CUDA. Non-POSIX imports and unsupported exportable creates are also rejected before acquiring backing. The shim never records unsupported resources for a later checkpoint refusal.

### Memory IPC adapter

Synchronous `cuMemAlloc_v2` is implemented with POSIX-capable VMM backing from allocation time. The shim rounds the backing size to CUDA's required granularity, reserves and maps a virtual address, and records the requested size separately. It does not move a live native malloc allocation when the application later exports it.

`cuIpcGetMemHandle` returns a virtual IPC memory handle in CUDA's fixed 64-byte value. Unlike a virtual shareable handle, it is not an FD. For a malloc-backed allocation with the example's identities, its decoded fields would look like this:

| Field | Bytes | Example value |
| --- | --- | --- |
| `version` | 8 | ASCII `CUIPC002` |
| `creator_pid` | 4 | `41` |
| `allocation` | 16 | `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa` |
| `reserved` | 20 | All zero |
| `requested` | 8 | `2097152` |
| `extent` | 8 | `2097152` |

The integer fields use little-endian encoding. This example assumes the request already meets the device's allocation granularity; otherwise `extent` is larger than `requested`. `cuIpcOpenMemHandle*` derives A's socket from the creator namespace PID, uses the same peer FD service as VMM import, and maps the allocation in B.

Repeated opens in the supported context return the same address and increment a reference count. The final close removes the imported mapping. `cuMemFree_v2` synchronizes the owning context before removing a malloc mapping; a synchronization failure leaves that mapping intact. An imported pointer must be closed, not freed. Foreign native IPC handles are rejected.

The adapter never calls native CUDA memory-IPC functions. It therefore does not need a CUDA checkpoint jobfile to reconstruct this sharing.

### Threads and synchronization

Application CUDA wrappers run on the calling application thread. On runtime startup (not ABI registration), two additional threads start in that process:

| Thread | Work |
| --- | --- |
| `cuinterpose-peer` | Accepts socket connections and classifies requests. Handles peer export requests using the descriptor cache without the main allocation-state lock or CUDA calls. |
| `cuinterpose-control` | Processes inspection and lifecycle commands one at a time. It enters the recorded CUDA context when a lifecycle operation needs driver calls. |

The peer thread queues control commands; it does not wait for their CUDA operations. A bounded queue refuses excess control requests. This separation lets an importer obtain an FD even while the creator's control thread is busy reconstructing another object.

Allocation and mapping changes normally hold the shim's state mutex. Application multicast calls that can block waiting for other devices, and IPC synchronization, release that mutex around the driver call. One `unlocked_driver_calls` counter prevents checkpoint entry until they have returned and recorded their results. The application must synchronize object destruction against calls using that object; the shim has no per-object pins or busy counts. During restore the application stays parked, so reconstruction holds the state mutex and updates records directly. The peer listener holds the export-cache mutex through each socket send. Cache removal, checkpoint teardown, and fork take the same mutex, so they wait for that send to finish. Sends use the socket timeout; a slow receiver can delay cache mutations until the send finishes or fails.

`ProcessState` owns one memblock registry keyed by allocation ID. Each `Memblock`
is either a unicast `Allocation` or a `MulticastObject`; virtual handles and
address mappings refer to that same registry. Export publication, handle release,
and existing-memblock imports share the registry's lifetime bookkeeping.
`MallocRegion` additionally tracks malloc/IPC reservations, requested sizes, and
open counts. A reservation's lifetime is distinct from its current mapping.

The export cache stores an FD and its reply metadata under each allocation ID.
Multicast replies include the creation properties needed by importers to record
and reconstruct the object. This copy lets peer service run without taking the
CUDA state mutex, including while another thread holds that mutex during import.

Inspection projects live memblocks into serializable `Record` values. The
checkpoint manifest maps each namespace PID directly to its records; live CUDA
handles and transient lifecycle bookkeeping remain inside the shim.

These simplified paths show the main actions, not error cleanup:

```text
malloc(size):
    create supported VMM backing and a virtual allocation handle
    reserve and map a virtual address in the current context
    record requested size, backing size, context and mapping
    return the virtual address

export(allocation):
    lock allocation state
    create and cache a real export FD if needed
    create the virtual shareable handle and mark successful sharing
    unlock allocation state
    return the virtual shareable handle

import(virtual_shareable_handle):
    lock allocation state
    resolve the original creator and allocation
    request its cached FD through the peer socket
    import the FD, or reuse an existing local allocation record
    record the virtual allocation or multicast handle and successful sharing
    unlock allocation state
    return the virtual handle, or map an address for memory IPC
```

Fork handlers lock metadata, close inherited shim descriptors in the child, and discard its inherited CUDA records. The child's next CUDA activity creates a listener named with its own namespace PID. This does not make arbitrary CUDA use after a multithreaded fork safe; CUDA and shim activity must be quiescent at fork. Prefer spawn/exec.

## Capture

The application must first stop submitting work, finish outstanding GPU work, and arrange to stay parked through restore. The coordinator does not pause application threads. In particular, **shim preparation runs before native CUDA lock** because saving and removing shared allocations requires working CUDA calls.

The agent requires the exact namespace-PID socket for every discovered CUDA
process in an opted-in workload. It starts the coordinator with those namespace
PIDs. The coordinator sends `BEGIN_CHECKPOINT` to each shim and checks creators,
ranges, access permissions, and complete multicast groups before changing driver
state. Under the process mutex, checkpoint entry requires `Active` and zero
unlocked driver calls, returns the inspection records, and changes the phase to
`Checkpointing`. Application memory APIs then refuse mutation. `INSPECT` remains
a read-only query; it does not authorize destructive preparation.

Every application CUDA call must have returned, outstanding GPU work must have
completed, and the participant set must remain fixed before checkpoint entry.
All sharing peers must be interposed and included in the checkpoint group;
creators must remain available. Neither the counter nor checkpoint entry parks
application threads or drains GPU work.

One coordinator issues each phase once, in order. There are no lifecycle retries,
rollback, or reconnect-and-resume semantics. A lost reply or timeout makes the
attempt unusable; the agent terminates the affected workload. A driver error
before an ordinary application operation changes tracked state can be returned
to the caller. Failure after a state-changing operation, or during destructive
capture/restore, terminates the process.

The process phase and stable ownership determine each phase's complete work.
Allocations, mappings, multicast objects, and bindings carry no per-object
checkpoint progress flags. Shared creator allocations own host-carrier contents;
importers reconnect to their creator. A failed phase never resumes halfway.

```mermaid
sequenceDiagram
    participant App as Application threads
    participant Agent as Snapshot agent
    participant Coord as Cuinterpose coordinator
    participant Shims as All process shims
    participant CUDA as CUDA driver
    participant CRIU as CRIU
    participant Files as Checkpoint files
    App->>App: Finish work and remain parked
    Agent->>Coord: Start prepare in target namespaces
    Coord->>Shims: BEGIN_CHECKPOINT on each namespace-PID socket
    Shims-->>Coord: Namespace PID and state records
    Coord->>Coord: Validate all participants
    Coord->>Shims: PREPARE_MULTICAST
    Shims->>CUDA: Drop exports, unmap, unbind, release multicast
    Shims-->>Coord: All participants finished
    Coord->>Shims: SAVE_ALLOCATIONS
    Shims->>CUDA: Copy shared creator bytes to host carriers
    Shims-->>Coord: All copies finished
    Coord->>Shims: PREPARE_UNICAST
    Shims->>CUDA: Drop exports, unmap and release shared allocations
    Shims-->>Coord: All participants finished
    Coord->>Files: Write cuinterpose.state
    Coord-->>Agent: Prepare succeeded and exit
    Agent->>CUDA: Lock every process, then checkpoint each
    Agent->>CRIU: Dump process tree and host memory
    CRIU->>Files: Save images including host carriers
```

Each arrow to “All process shims” means concurrent requests followed by a wait for every participant. The diagram combines those requests to keep the ordering readable.

Capture removes the outer objects before their dependencies:

1. `PREPARE_MULTICAST` removes multicast exports, mappings, bindings, and handles.
2. `SAVE_ALLOCATIONS` saves the shared creator bytes while unicast allocations still exist.
3. `PREPARE_UNICAST` drains peer export requests, removes shared mappings, and releases shared driver handles.

Virtual allocation handles, including virtual multicast handles, remain in CPU memory for CRIU alongside mappings, allocation references, and carrier addresses. Private allocations remain native CUDA state. If a destructive phase or later checkpoint step fails, the source must be terminated rather than resumed with sharing removed.

For the 2 MiB example, `SAVE_ALLOCATIONS` copies A's allocation into A's host arena. B saves no second copy. CRIU captures that arena as process memory; there is no separate `aaaaaaaa….bin` file. If A also has a never-shared allocation, that allocation stays on the native CUDA path instead of entering the arena.

## Restore

Before CRIU, the agent restores the tool paths and control directory, removes
the exact socket names for the namespace PIDs in the manifest, and validates
the artifact format. CRIU recreates the process tree, namespace PIDs, shim
records, and host carriers. Native CUDA restore reconstructs private memory and
process state, then unlocks CUDA so the shims can make driver calls. The
application remains parked until all shim reconstruction succeeds.

```mermaid
sequenceDiagram
    participant Agent as Snapshot agent
    participant CRIU as CRIU
    participant CUDA as CUDA driver
    participant Coord as Cuinterpose coordinator
    participant Creators as Creator shims
    participant Importers as Importer shims
    participant App as Parked application
    Agent->>Agent: Check format and restore mounts and socket directory
    Agent->>CRIU: Restore process tree and host carriers
    Agent->>CUDA: Restore native state, then unlock CUDA
    Agent->>Coord: Start restore in target namespaces
    Coord->>Creators: INSPECT on each namespace-PID socket
    Coord->>Importers: INSPECT on each namespace-PID socket
    Coord->>Coord: Match captured namespace PID set and validate state
    Coord->>Creators: LOAD_ALLOCATIONS
    Creators->>CUDA: Create shared backing and copy host bytes to GPU
    Creators->>CUDA: Restore creator mappings, access and export FDs
    Creators-->>Coord: Creator allocations ready
    Note over Coord,Importers: Every participant completes LOAD_ALLOCATIONS
    Coord->>Importers: RESTORE_UNICAST
    Importers->>Creators: Request fresh allocation FDs
    Creators-->>Importers: Send FDs over peer UDS
    Importers->>CUDA: Import, map original addresses and restore access
    Importers-->>Coord: Imports ready
    Note over Coord,Importers: Every participant completes RESTORE_UNICAST
    Coord->>Creators: RESTORE_MULTICAST_CREATORS
    Creators->>CUDA: Create multicast objects and cache export FDs
    Note over Coord,Importers: Wait for all participants
    Coord->>Importers: RESTORE_MULTICAST_IMPORTERS
    Importers->>Creators: Request fresh multicast FDs
    Importers->>CUDA: Import multicast objects
    Note over Coord,Importers: Wait for all participants
    Coord->>Creators: RESTORE_MULTICAST_DEVICES
    Coord->>Importers: RESTORE_MULTICAST_DEVICES
    Creators->>CUDA: Attach recorded devices
    Importers->>CUDA: Attach recorded devices
    Note over Coord,Importers: Wait for the complete device team
    Coord->>Creators: RESTORE_MULTICAST_BINDINGS
    Coord->>Importers: RESTORE_MULTICAST_BINDINGS
    Creators->>CUDA: Bind members, map addresses and restore access
    Importers->>CUDA: Bind members, map addresses and restore access
    Note over Coord,Importers: Wait for all participants
    Coord->>Creators: INSPECT
    Coord->>Importers: INSPECT
    Coord->>Coord: Compare with captured records
    Coord-->>Agent: Restore succeeded and exit
    Agent->>App: Publish restore-complete sentinel
    App->>App: Resume
```

The creator/importer labels describe roles, not disjoint sets of processes: one process can own one allocation and import another. Every participant receives every lifecycle operation, even when it has no work in that operation.

`LOAD_ALLOCATIONS` creates new backing, copies host-carrier bytes back, restores creator mappings and permissions, and makes new export FDs available. After sending its successful LOAD reply, the shim releases the carrier arena. `RESTORE_UNICAST` reconnects importers to those new allocations at their original virtual addresses. Neither operation recreates never-shared allocations.

`RESTORE_MULTICAST` consists of four ordered wire operations:

| Operation | Work | Why wait before continuing? |
| --- | --- | --- |
| `RESTORE_MULTICAST_CREATORS` | Recreate objects from their recorded properties and publish export FDs. | Importers need the new creator FDs. |
| `RESTORE_MULTICAST_IMPORTERS` | Obtain those FDs and import the objects. | All participants need local handles before starting device attachment. |
| `RESTORE_MULTICAST_DEVICES` | Replay device attachments. | Bind operations can wait for the whole device team. |
| `RESTORE_MULTICAST_BINDINGS` | Replay `BindMem`/`BindAddr`, map the original addresses, and restore permissions. | The application must not resume with incomplete mappings. |

Capture and restore reverse the dependency order, not the number of messages. Capture can remove each process's existing multicast references in one operation. Restore needs creator-to-importer handoffs and whole-team device attachment before binding.

## Checkpoint files and compatibility

`cuinterpose.state` is a **binary, versioned MessagePack file**. Its body is a
map from namespace PID directly to that process's records. Socket paths are derived
from those keys and are not persisted. Control messages and `cuinterpose.state` are limited to 32
MiB to bound allocations from socket frame prefixes and checkpoint files; they
contain metadata, never allocation contents. Here is a shortened decoded view
for the two-worker example.
Allocation-property and handle-count fields are omitted, and binary allocation
IDs are shown as hex strings. Field names and nesting match the serialized data:

```yaml
version: 2
body:
  41:
    - allocation:
        allocation:
          id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
          creator_pid: 41
        content: true
        size: 2097152
    - mapping:
        allocation:
          id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
          creator_pid: 41
        address: 0x700000000000
        size: 2097152
        offset: 0
        access:
        - [1, 0, 3]
  42:
    - allocation:
        allocation:
          id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
          creator_pid: 41
        content: false
        size: 2097152
    - mapping:
        allocation:
          id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
          creator_pid: 41
        address: 0x710000000000
        size: 2097152
        offset: 0
        access:
        - [1, 1, 3]
```

The outer namespace-PID key identifies the process. The repeated allocation reference
identifies both the allocation and its creator without a separate `creator:
true/false` field. `content: true` means A owns the content copy, not that bytes
appear in this file. `offset: 0` maps from the start of the allocation. In the
access tuples `(location type, location ID, flags)`, type `1` means a CUDA device and flags `3` mean
read/write: A grants GPU 0 access, and B grants GPU 1 access. Addresses are
shown in hex for readability; they are encoded as integers.

Multicast adds records to the same process state. For example, this complete
`multicast_binding` record says that GPU 0 binds the first 2 MiB of allocation
`aaaaaaaa…` into the start of multicast object `bbbbbbbb…` using `BindMem`'s v1
ABI:

```yaml
multicast_binding:
  allocation:
    id: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    creator_pid: 41
  source:
    memory:
      allocation:
        id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        creator_pid: 41
      offset: 0
  size: 2097152
  offset: 0
  flags: 0
  version: v1
  device: 0
```

The member offset is nested under `source`; the outer offset is into the
multicast object. Separate `multicast`, `multicast_device`, and
`multicast_mapping` records describe the object, attached devices, and virtual
mappings. This binding alone is not a complete multicast checkpoint.

The coordinator sorts records and publishes the state through a temporary file,
file `fsync`, atomic rename, and directory `fsync`. On restore it checks the
namespace PID set, derives the exact socket paths, rebuilds sharing, then
compares a fresh inspection against the captured records. Allocation bytes come
from CRIU's host-carrier images, not this file.

The corresponding section of `manifest.yaml` is ordinary YAML:

```yaml
cuinterpose:
  requested: true
  prepared: true
  format: 2
```

`requested` records workload opt-in; `prepared` records successful coordinator preparation and state publication. `format: 2` identifies the agent's artifact contract. The MessagePack envelope is also version `2`. A prepared checkpoint must also have CUDA process metadata and readable cuinterpose state. Missing or different formats and old `cuda-checkpoint-job` artifacts are rejected before CRIU.

The private frontend/backend ABI is version **1**. The MessagePack protocol and state envelope, virtual shareable handle, and virtual IPC memory handle are version **2**. Older draft artifacts, including shim PageBroker artifacts, are not migrated or silently interpreted as host-carrier checkpoints.

The shim libraries themselves are part of the checkpointed process. Their files must be available at the original paths, and the coordinator must understand their protocol. Ship a matching frontend, backend, and coordinator set; the format checks are not permission to substitute arbitrary library builds.

## Installation, namespaces, and limits

A workload opts in with:

```yaml
metadata:
  annotations:
    nvidia.com/cuinterpose: enabled
```

Snapshot mounts `cuda-checkpoint`, `libcuinterpose.so`, and `libcuinterpose_core.so` at fixed paths under `/tmp/snapshot-cuda` in every checkpoint target. The annotation only adds the shim first in `LD_PRELOAD`. It does not replace the workload command. There is no `--launch-job` wrapper, jobfile staging, or GPU-count-based wrapping decision.

The coordinator joins the target's mount, UTS, IPC, network, and PID namespaces. It does not join the target's user or cgroup namespace. The agent opens trusted executable, namespace, and checkpoint-directory descriptors before entering the target, rather than looking up the coordinator binary through the workload's filesystem.

The control volume is a Pod-local `emptyDir`, with a separate `subPath` for each target container. Source and restore checks reserve the volume name, path, and environment settings. Socket files have mode `0600` and are not network listeners. An ordinary process in another Pod cannot reach them through its mounts. Processes intentionally given the same directory and filesystem credentials can; the workload container is not isolated from its own processes. Node root and privileged agents remain trusted.

Host-memory capacity must cover approximately one copy of each creator's shared bytes, plus bookkeeping and temporary mappings. CUDA rounds allocations to its required granularity, so backing size can exceed requested malloc size. Host carriers use pinned memory during copying and increase CRIU image size. Importers do not save another content copy.

The implementation targets Linux/amd64 and glibc 2.34 or newer for the preload libraries. The coordinator is statically linked with musl. CUDA entry points are resolved at runtime; the driver and GPUs must support the VMM and multicast APIs used by the workload. Snapshot's existing privilege, CRIU, device, and source/destination compatibility requirements still apply.

Supported memory IPC requires fully interposed peers and the documented single owning-context behavior. Event IPC, memory-pool IPC, managed/async/pitched allocation families, historical 32-bit allocation entry points, and general cross-context peer-access emulation are not supported by this adapter. Removing jobfile support does not make unannotated native-IPC workloads checkpointable.

Exactly POSIX-FD exportable VMM and multicast objects are supported. FABRIC, mixed exportable types, non-POSIX multicast objects (including handle type zero), and foreign VMM imports are rejected at the API call before CUDA creates or imports anything. Private unicast VMM remains native-owned. Incomplete multicast groups, missing creators or participants, and failed copies stop capture or restore.
