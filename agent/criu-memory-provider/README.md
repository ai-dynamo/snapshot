# CRIU memory provider

`criu-memory-provider` is a static C++ library with a C API.  It is the
server side of CRIU external memory: it turns checkpoint bytes into sparse
file descriptors, then hands those FDs to CRIU over its external-memory
socket.

It builds `build/libcriu_provider.a`.

This library deliberately has a narrow job.  It understands CRIU images,
plans the memory layout, owns the FDs it returns to CRIU, and serves the CRIU
protocol.  It is not a daemon or a storage backend.  It has no PageBroker,
Snapshot, Kubernetes, S3, credentials, cache, or network code.

## The split of responsibility

The caller owns the checkpoint and restore transaction.  PageBroker is the
caller in this repository, but another caller can use the same library.

| The caller does | This library does |
| --- | --- |
| Stores and publishes checkpoint files. | Reads CRIU metadata and makes a plan. |
| Decides whether provider restore is available. | Validates the plan. |
| Reserves resources and stages the exact source bytes. | Makes and fills sparse memfds. |
| Starts CRIU and gives it one provider-socket endpoint. | Serves CRIU on the other endpoint. |
| Completes or aborts the larger transaction. | Releases its FDs after CRIU `COMMIT` or `ABORT`. |

The library never fetches bytes.  By the time `session_prepare()` returns,
the caller must have made every requested source range available locally.
After that point the library makes no `read_range()` calls.

## What a caller must provide

The public contract is in [`include/criu_provider.h`](include/criu_provider.h).
The handles are opaque.  A caller must keep the plan and callback context
alive until the matching session is destroyed.  The library copies the callback
table when the session is created.

### Restore source callbacks

`criu_provider_source_ops` has two callbacks:

- `read_range(context, image, offset, buffer, length)` copies bytes from an
  already-ready local image into `buffer`.  It is used only by
  `criu_provider_session_prepare()`.  Return `0` on success and a negative
  errno-style value on failure.
- `open_ready_image(context, image, flags)` opens an already-ready local
  metadata image and returns its FD.  It is used for `READY_LOCAL` images.
  The library closes the returned duplicate after it sends it to CRIU.

The callback must use `logical_image` as a logical basename, not as a path
provided by an untrusted peer.  It must not do remote I/O after the caller has
declared the restore ready.

### Dump callbacks

`criu_provider_dump_ops` lets the caller own the output destination:

- `open_output_image()` creates or opens one writable image FD for CRIU.
- `commit()` publishes the completed checkpoint only after CRIU sends
  `COMMIT` and the library has flushed every output FD.
- `abort()` discards the caller's staging output after `ABORT`, a socket
  failure, or an error while serving dump requests.

The library keeps the first FD for each output image and gives CRIU duplicates
of it.  It never publishes a dump itself.

## Checkpoint: make the plan

After CRIU has finished a full raw dump into a local staging directory:

```c
criu_provider_plan *plan;
int rc = criu_provider_plan_from_checkpoint(staging_dir, &plan);
if (rc == 0) {
        rc = criu_provider_plan_write(plan,
                ".../criu-provider_plan.json");
        criu_provider_plan_destroy(plan);
}
```

`criu_provider_plan_write()` writes a temporary file, flushes it, renames it,
and syncs the containing directory where supported.  A caller should treat a
plan-generation failure as a reason to use ordinary staged restore, not as a
reason to discard an otherwise valid checkpoint.

The indexer reads CRIU metadata only.  It creates objects for eligible private
anonymous VMAs, anonymous shared memory, checkpointed memfds, and a residual
object for each `pages-N.img`.  The residual object preserves page bytes for
VMAs that this provider does not serve directly.

## Restore: normal order

```text
load plan
  -> inspect requirements and enumerate source ranges/images
  -> reserve resources and stage source bytes plus ready-local images
  -> create session
  -> prepare all provider FDs
  -> create SOCK_SEQPACKET pair
  -> give CRIU one endpoint; serve on the other
  -> CRIU COMMIT or ABORT
  -> finish the caller's larger transaction
```

In API form:

1. `criu_provider_plan_load()` reads `criu-provider_plan.json`.
2. `criu_provider_plan_requirements()` reports resource estimates.
3. `criu_provider_plan_enumerate_source_ranges()` tells the caller exactly
   which source bytes to stage.  `criu_provider_plan_enumerate_images()` tells
   it which named images must be available and their roles.
4. Call `criu_provider_session_create()`, then
   `criu_provider_session_prepare()` once.  Preparation creates every sparse
   FD, sizes it with `ftruncate()`, copies the planned bytes, and leaves holes
   as holes.
5. Create a Unix `SOCK_SEQPACKET` socket pair.  Give CRIU one FD as its
   external-memory provider.  Call `criu_provider_session_serve()` with the
   other FD; it blocks until CRIU finishes or the socket fails.
6. Destroy the session and then the plan when the caller no longer needs them.

`criu_provider_session_prepare()` must finish before CRIU starts.  It is an
error to call it twice.  `session_destroy()` is safe after preparation fails
and closes every FD still owned by the library.

## What CRIU receives

The server implements CRIU's `extmem.proto` over Unix `SOCK_SEQPACKET` and
uses `SCM_RIGHTS` to send FDs.

| CRIU request | Result |
| --- | --- |
| `INIT` | Activates the session. |
| `WAIT_READY` | Succeeds only after preparation; preparation is already complete here. |
| `OPEN_IMAGE` for a provider FD image | A duplicate of the prepared image FD. |
| `OPEN_IMAGE` for a ready-local image | A duplicate supplied by `open_ready_image()`. |
| `OPEN_IMAGE` for a deliberate local fallback | `-ENOTSUP`, so CRIU uses its local image directory. |
| `OPEN_IMAGE` for an unknown name | `-EPROTO`; it never opens an unplanned file. |
| `GET_VMA` or `GET_SHARED` | A duplicate only for an exact planned object match. |
| `COMMIT` or `ABORT` | Closes provider-owned FDs. |

An unmatched VMA or shared-memory request is `-EPROTO`.  A restore-side
`ABORT` returns `-ECANCELED` after the acknowledgement.  The caller's commit
is separate from CRIU's `COMMIT`: CRIU has finished with the provider FDs, but
the caller may still have other restore work to complete.

Private VMA FDs are unsealed.  Checkpointed memfd FDs are also returned
without new seals: CRIU applies the saved seals after `WAIT_READY`, in its own
normal ordering.

## Dump: normal order

Create a plan with `criu_provider_dump_plan_create(page_size, &plan)`.  It
contains the revision-specific CRIU image rules.  Add explicit images with
`criu_provider_dump_plan_add_image()` when needed, create the dump session,
then give CRIU one end of a `SOCK_SEQPACKET` pair and run
`criu_provider_dump_session_serve()` on the other.

For an image marked `DUMP_PROVIDER_OUTPUT`, the library asks
`open_output_image()` for a writable FD and sends CRIU a duplicate.  Images
marked local or local fallback return `-ENOTSUP`, so CRIU uses its ordinary
local output directory.  On `COMMIT`, the library `fsync()`s every output FD,
then calls `commit(plan)`.  On failure it calls `abort()` instead.

## `criu-provider_plan.json`

The sidecar is UTF-8 JSON, not protobuf binary.

[`proto/criu_provider_plan.proto`](proto/criu_provider_plan.proto) is a
private typed schema used by Protobuf's existing JSON reader and writer.  It
is not a public C API, a CRIU socket protocol, or a file format that another
provider must share.

A shortened plan looks like this.  It uses logical image basenames, not host
paths.  Protobuf JSON writes 64-bit values as strings, so offsets and sizes
are quoted deliberately.

```json
{
  "format_major": 1,
  "page_size": "4096",
  "images": [{
    "name": "pages-1.img", "size": "8192", "role": "PAGES",
    "restore_mode": "RESTORE_PROVIDER_FD",
    "dump_mode": "DUMP_PROVIDER_OUTPUT"
  }],
  "chunks": [{
    "image": "pages-1.img", "source_offset": "0",
    "stored_length": "4096", "decoded_length": "4096",
    "encoding": "RAW",
    "placements": [{"object_key": "vma:1:0", "destination_offset": "0"}]
  }]
}
```

The JSON plan records:

- plan format and host page size;
- logical image name, size, role, restore mode, and dump mode;
- source image ranges and their destination placements;
- private VMA, shared-memory, and residual page-image objects;
- saved memfd-seal metadata and resource estimates; and
- the exact CRIU image-name rules allowed to fall back to local files.

Plans use logical basenames only.  Loading rejects unknown JSON fields,
invalid format versions, duplicate image/object/rule keys, traversal names,
overflow, non-raw chunks, and ranges outside the source image or destination
FD.

`criu_provider_requirements` deliberately distinguishes:

- `stored_bytes`: bytes the caller must read;
- `materialized_bytes`: bytes copied into sparse FDs;
- `logical_fd_bytes`: sum of FD sizes, including holes;
- `metadata_bytes`: non-page image bytes;
- `workspace_bytes`: provider copy workspace; and
- `fd_count`: provider-owned FD count.

Do not reserve physical memory from `logical_fd_bytes`; sparse holes do not
consume that amount of memory.

## Supported V1 input

V1 accepts a full raw CRIU checkpoint with:

- private anonymous VMAs, except stacks, vdso/vvar/vsyscall, `MAP_GROWSDOWN`,
  and `MADV_WIPEONFORK` mappings;
- anonymous shared memory;
- checkpointed non-hugetlb memfds; and
- residual `pages-N.img` data for the rest.

It rejects parent/pre-dump chains, compressed page blocks, hugetlb memfds,
malformed images, missing required images, and future image features it does
not understand.  Rejection means this provider acceleration cannot be used;
it does not mean CRIU's checkpoint itself is invalid.

## Directory map

```text
include/criu_provider.h          Public C API
proto/extmem.proto               CRIU external-memory socket messages
proto/criu-images/               CRIU image schemas used by the indexer
proto/criu_provider_plan.proto   Private schema for JSON plan I/O
src/api.cpp                      Public API, JSON plan I/O, handles
src/plan.cpp                     Plan validation and image-name rules
src/image_index.cpp              CRIU metadata reader and plan builder
src/materializer.cpp             Sparse memfd creation and byte placement
src/protocol.cpp                 Restore-side extmem server
src/dump.cpp                     Dump-side extmem server and finalization
tests/provider_test.cpp          Focused unit tests
```

`extmem.proto` and `criu-images/*.proto` are CRIU schemas.  They are separate
from the plan schema: one describes the CRIU socket protocol; the others
describe CRIU checkpoint metadata.

## Build and test

The build requires Linux, a C++20 compiler, `protoc`, Protobuf C++ headers and
library, and GoogleTest for tests.

```sh
make -C agent/criu-memory-provider
make -C agent/criu-memory-provider test
```

The test suite covers plan JSON and validation, raw image indexing, sparse
holes, source-range enumeration, image rules, and dump image selection.  The
restore socket test is skipped only where the test sandbox blocks Unix socket
traffic.
