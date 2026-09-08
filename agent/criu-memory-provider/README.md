# CRIU memory provider

This builds `build/libcriu_provider.a`, the provider side of CRIU external
memory.  It reads CRIU checkpoint metadata, prepares sparse FDs, and serves
them to CRIU over Unix `SOCK_SEQPACKET`.

## Using the library in a provider

You write the backend around this library.  It owns checkpoint storage,
staging, publishing, resource limits, and starting CRIU.  PageBroker is one
example backend.

You implement these callbacks:

- restore: `read_range()` to return already-local checkpoint bytes, and
  `open_ready_image()` for a local image FD when needed;
- dump: `open_output_image()` to return a writable staging FD, plus `commit()`
  and `abort()` for the backend transaction.

The library implements CRIU image reading, plan creation and validation,
sparse-FD creation and filling, and the dump and restore `SOCK_SEQPACKET`
protocol.  You do not implement the CRIU protocol or fill the provider FDs.

For checkpoint:

```text
create dump session -> give CRIU one socket endpoint -> serve dump session
-> on COMMIT, create and write criu-provider_plan.json -> publish staging
```

For restore:

```text
load plan -> enumerate and stage its bytes -> create and prepare session
-> give CRIU one socket endpoint -> serve restore session
```

The backend gives the library only local bytes and FDs through callbacks. It
does not give it storage URLs, credentials, or backend-specific objects. The
library never fetches data.

## C API

The complete API is [`include/criu_provider.h`](include/criu_provider.h).  It
uses opaque C handles, so a backend can call it from C++, Rust, Go, or C.

| API | What the backend does | What happens |
| --- | --- | --- |
| `criu_provider_plan_from_checkpoint()` | Pass a finished local checkpoint directory. | Reads CRIU metadata and builds a plan. |
| `criu_provider_plan_write()` / `_load()` | Write or load `criu-provider_plan.json`. | Stores or validates the JSON plan. |
| `_requirements()`, `_enumerate_source_ranges()`, `_enumerate_images()` | Reserve resources and stage listed inputs. | Reports exactly what preparation needs. |
| `criu_provider_session_*()` | Pass restore callbacks and a `SOCK_SEQPACKET` FD. | Prepares restore FDs and serves CRIU. |
| `criu_provider_dump_session_*()` | Pass writable-output, commit, and abort callbacks. | Serves CRIU dump output; flushes FDs, then calls commit or abort. |

Keep the plan and callback context alive until its session is destroyed.

### Restore callbacks

`criu_provider_source_ops` has:

- `read_range()`: copy an already-local source range into the supplied buffer.
  Return `0` or a negative errno-style error.
- `open_ready_image()`: return an FD for a `RESTORE_READY_LOCAL` image when
  the session has no prepared FD for that image.

`criu_provider_session_prepare()` performs all `read_range()` calls.  It must
finish before CRIU starts; after it returns, the library does no more reads.

### Dump callbacks

`criu_provider_dump_ops` has:

- `open_output_image()`: return a writable FD for one CRIU image;
- `commit()`: finish the backend's output transaction after CRIU `COMMIT`; and
- `abort()`: discard staging after `ABORT` or failure.

The library flushes its output FDs before calling `commit()`.  If `commit()`
fails, it calls `abort()`.  The library does not publish a checkpoint itself.

## Normal flows

Checkpoint:

```text
CRIU dump -> backend staging directory -> dump-session commit callback
-> plan_from_checkpoint -> write criu-provider_plan.json -> publish
```

Restore:

```text
load plan -> stage listed bytes -> session_prepare
-> socketpair -> CRIU <-> session_serve -> COMMIT or ABORT
```

`session_prepare()` creates one correctly sized sparse memfd per planned VMA,
shared object, residual page image, and provider-owned metadata image.  Holes
stay holes.

### Possible backend extension: FD pool

Today the library calls `memfd_create()` and closes those FDs itself.  A future
backend API may instead lease an empty, exclusive FD from a pool.  The library
would still reset, size, fill, retain, and send that FD; after CRIU `COMMIT` or
`ABORT`, it would return the lease to the backend instead of closing it.  The
backend should not fill provider FDs: placement and sparse-hole correctness
remain library responsibilities.

## `criu-provider_plan.json`

The sidecar is UTF-8 JSON.  The private
[`proto/criu_provider_plan.proto`](proto/criu_provider_plan.proto) file is
only the schema used by Protobuf's JSON reader/writer; it is not a public
protocol or a binary plan format.

The plan names logical images, source ranges, FD placements, objects, image
roles, and resource estimates.  It rejects unknown JSON fields, path
traversal, duplicate keys, overflow, non-raw chunks, and out-of-range data.

V1 supports full raw checkpoints: private anonymous memory, anonymous shared
memory, non-hugetlb memfds, and residual page-image data.  It rejects
parent/pre-dump chains, compressed pages, hugetlb memfds, and malformed images.
A rejected plan means use ordinary staged restore; it does not invalidate the
checkpoint.

## CRIU requests

`OPEN_IMAGE` returns a duplicate of a prepared FD for
`RESTORE_PROVIDER_FD`.  For `RESTORE_READY_LOCAL`, it uses a prepared FD when
one exists, otherwise `open_ready_image()`.  `RESTORE_LOCAL` and
`RESTORE_LOCAL_FALLBACK` return `-ENOTSUP`, so CRIU uses its local image
directory.  Unknown image names return `-EPROTO`.

`GET_VMA` and `GET_SHARED` return a duplicate only for an exact plan match;
otherwise they return `-EPROTO`.  `COMMIT` and `ABORT` release provider-owned
FDs.

## Directory map

```text
include/        public C API
proto/          CRIU schemas and private JSON-plan schema
src/api.cpp     C API and plan I/O
src/plan.cpp    plan validation and image rules
src/image_index.cpp
                CRIU metadata reader and plan builder
src/materializer.cpp
                sparse FD creation and byte copying
src/protocol.cpp restore server
src/dump.cpp     dump server
tests/           library tests
```

## Build and test

```sh
make -C agent/criu-memory-provider
make -C agent/criu-memory-provider test
```
