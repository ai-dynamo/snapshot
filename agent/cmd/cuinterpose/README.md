<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Same-node POSIX CUDA VMM checkpoint and restore

This interposer implements checkpoint and restore for CUDA VMM allocations
shared between processes on one node with
`CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR`.

## Contract

Enable delivery on the SnapshotJob source Pod template:

```yaml
metadata:
  annotations:
    nvidia.com/cuda-interposer: enabled
```

Snapshot's PodContract shaper copies the shim and `cuda-checkpoint` from the
configured agent image into an `emptyDir`, mounts it at
`/tmp/snapshot-interposer`, prepends the shim to `LD_PRELOAD`, and launches
every checkpoint target through the injected
`cuda-checkpoint --launch-job`. During restore, the node agent mounts the same
tool directory from `/snapshot-binaries/cuinterposer` at that path. The
restore placeholder has no interposer init container and does not preload the
shim; CRIU restores the already-interposed process tree.

The shim requires glibc 2.34 or newer, covering Ubuntu 22.04 (glibc 2.35) and
later. It resolves the real `dlsym` through one
`dlvsym(..., "GLIBC_2.34")` bootstrap. The Makefile rejects any higher glibc
requirement. The coordinator is statically linked and does not inherit this
workload glibc constraint.

The annotation enables the complete capture contract, including the launch-job
wrapper. Workload users do not supply `cuda-checkpoint` or add another wrapper.

The snapshot agent keys off the live shim Unix sockets at
`/snapshot-control/cuinterposer-<namespace-pid>.sock`, not `/proc/<pid>/environ`.
Python `setproctitle` (vLLM/SGLang) can clobber procfs environment while the
sockets remain. No sockets skips prepare. A partial or invalid set of sockets
fails closed. Restore runs the coordinator only when the checkpoint artifact
contains `cuinterposer.state`.

Legacy CUDA IPC remains driver-owned and is unsupported by this shim.

`DYN_SNAPSHOT_PARTICIPANT_ID` may provide a stable 32-character lowercase
hex participant ID. Otherwise, the shim creates one when the process starts.

The orchestrator must externally quiesce the application at the checkpoint
boundary. Allocation sharing, import, mapping, access updates, kernel work,
communication-library setup, and teardown must not be in flight.

During ordinary execution:

- CUDA generic allocation handles for POSIX-capable allocations tracked by the
  shim are tagged logical tokens; the corresponding real driver handles remain
  internal.
- Untracked CUDA generic allocation handles remain real driver handles.
- A POSIX-capable allocation becomes checkpoint-managed when its ticket fd
  is exported.
- POSIX exports return sealed ticket FDs containing the creator,
  allocation, endpoint, and authorization identities.
- A ticket import resolves through the creator's local Unix endpoint. A
  transient raw CUDA FD is passed with `SCM_RIGHTS`, imported,
  closed immediately, and never returned to the application.
- A raw external POSIX import is passed directly to CUDA and is not tracked by
  the shim.

Handle ownership is explicit:

| Source | Application receives | Managed table | Driver-handle owner |
| --- | --- | --- | --- |
| POSIX-capable create, tracked retain, or ticket import | Tagged logical token | Logical token to driver handle | Shim |
| Raw external import or other pass-through | Real driver handle | None | Application |
| Checkpoint carrier | Nothing | No separate entry | Shim |

`posix.c` owns the sealed ticket format and remote creator exchange.
`symbols.c` owns `dlsym`, `cuGetProcAddress`, and the replacement table.
`interpose.c` owns CUDA interception, allocation state, and local raw exports.

Before native CUDA checkpoint, the snapshot agent asks the native coordinator
to validate the complete participant topology. The original `cuMemCreate`
participant is always the saver and must still have a live creator handle or
mapping (the creator-anchor invariant). Prepare then keeps one unmapped
creator-local carrier, removes managed mappings without freeing VA
reservations, and releases importer handles. Native CUDA alone saves and
restores allocation bytes.

On restore, native CUDA first restores creator allocations. CUDA processes are
then unlocked so the shim can replay VMM topology while the application stays
behind the restore-complete sentinel. The coordinator restores every creator
and returns to listening before any importer requests a fresh export. The shim
remaps every allocation at its saved VA, restores effective access and required
handle translations, and performs final topology validation. Any failed
validation fails closed. There is no rollback after checkpoint preparation
mutates CUDA state.

The native coordinator atomically writes `cuinterposer.state`, its opaque durable
topology sidecar, in the checkpoint directory. Go only orders native VMM
prepare and restore; it does not serialize or inspect the topology. The
sidecar contains no raw FDs or allocation contents.

## Interception and symbol resolution

The shim covers direct Driver API symbols and these resolver paths:

- explicit `dlsym()` lookups from `libcuda.so` and `libcudart.so`;
- `cuGetProcAddress`, `cuGetProcAddress_v2`, and `_ptsz`;
- `cudaGetDriverEntryPoint` and `cudaGetDriverEntryPointByVersion`, including
  the `_ptsz` exports present in CUDA 13.

CUDA resolvers must first return success, a valid entry, and a successful query
status when present. The shim then substitutes only symbols in its existing
replacement table, using the requested CUDA version to select the ABI.
Chaining with another `dlsym()` interposer and preserving original-caller
`RTLD_NEXT` lookup scope are unsupported.

## Testing

Run the native interposer integration test from the repository root:

```bash
uv run --project agent/cmd/cuinterpose/tests \
  pytest agent/cmd/cuinterpose/tests/test_cucheckpoint.py -v
```

## Qualification

Disjoint GPU qualification is limited to source GPUs 0/1 restored onto
destination GPUs 4/5 with user-mode `libcuda` 615.65 and kernel RM 595.58.03.

## Limitations and future extensions

> [!WARNING]
> Raw external POSIX imports are passed through and are not tracked or
> reconstructed. Every mapping and handle from such an import must be unmapped
> and released before checkpoint prepare, as the GMS saver/sleep flow does. If
> retained, native checkpoint may fail, or restore may later see stale or
> incomplete sharing; the shim cannot validate this.

The shim reserves generic handles whose top 16 bits are `0xd94d` for logical
tokens. If CUDA returns a raw pass-through handle in that range, the shim
releases it and returns `CUDA_ERROR_INVALID_HANDLE`.

### Potentially silent gaps

The following gaps can bypass bookkeeping or silently preserve stale state:

- An un-interposed raw export or import call bypasses bookkeeping.
- A missing or unreadable `LD_PRELOAD` path is skipped by the loader with a
  warning on stderr but no failure, so the workload runs uninterposed and exits
  0. A shim that is present but below the glibc floor above instead fails
  loudly, exiting 1 before `main`.
- Raw FD aliases created with `dup` or `fcntl` are invisible.
- Raw FDs cached, queued, or transferred with `SCM_RIGHTS` outside the adapter
  can later import stale pre-restore state.
- An unshimmed broker or helper makes the sharing topology incomplete.
- An uncovered CUDA runtime symbol-resolution path can bypass wrappers.
- An old generic handle retained by an uncovered consumer can be stale.
- NVIDIA-specific `ioctl`, `fstat`, or `poll` semantics on a ticket FD are
  unsupported.
- Checkpoint during sharing or communicator construction is unsupported.

Applications must finish setup and externally quiesce the complete process
group first.

Children created with `fork()` and no subsequent `exec()` lazily receive a fresh
random process-local participant identity, a child-PID control socket and thread,
and empty allocation, handle, and mapping bookkeeping on their first intercepted
VMM operation. This supports the fork-before-CUDA-initialization lifecycle used
by vLLM. An explicitly configured parent participant ID is never reused by a
forked child.

Forking after the shim has tracked VMM allocations, handles, or mappings is
unsupported. The child deliberately discards its inherited copies without
freeing them or calling CUDA; inherited tracked CUDA state is not reconciled or
preserved.

### Fail-closed limitations

The coordinator fails closed for a missing creator anchor, incomplete
participants or topology, more than eight access descriptors, access ranges
that partially overlap a tracked mapping, and reconstruction or final
validation failures. One access call may cover multiple complete mappings.
There is no rollback after checkpoint preparation mutates CUDA state.

### Future compatibility

FABRIC/IMEX handles, non-POSIX allocation sharing, raw-FD compatibility, and
multi-node rendezvous are not implemented. The durable sidecar preserves
allocation type, location, requested-handle type, mapping, access, and creator
topology needed for those future compatibility seams. Creator endpoints and
authorization tokens remain transient live/ticket data and are not sidecar
fields. The sidecar intentionally contains neither raw FDs nor allocation
bytes.
