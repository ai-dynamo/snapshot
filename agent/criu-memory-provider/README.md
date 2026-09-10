# CRIU memory provider

This builds `build/libcriu_provider.a`, a library for implementing CRIU's
external-memory provider.  It creates checkpoint plans, prepares sparse FDs,
and serves CRIU's Unix `SOCK_SEQPACKET` protocol.

## Using the library in a provider

You write the backend around this library.  It owns checkpoint storage,
staging, publishing, resource limits, and starting CRIU.  PageBroker is one
example backend.

You implement these callbacks:

- restore: `write_ranges()` puts checkpoint bytes directly into the library's
  FDs; `open_ready_image()` opens staged metadata images;
- dump: `open_output_image()` to return a writable staging FD, plus `commit()`
  and `abort()` for the backend transaction.

The split is:

- the backend owns storage, transactions, staging, and starting CRIU;
- this library owns plan validation, sparse FDs, and the provider protocol;
- CRIU requests and maps the FDs, falls back to local images when allowed, and
  applies saved memfd seals.

For checkpoint:

```text
create dump session -> give CRIU one socket endpoint -> serve dump session
-> in the COMMIT callback, index staging and write criu-provider_plan.json
-> publish staging
```

For restore:

```text
load the local plan -> create and prepare session
-> write_ranges supplies the requested bytes -> stage ready-local images
-> give CRIU one socket endpoint -> serve restore session
```

The backend supplies bytes through callbacks.  It does not give the library
storage URLs, credentials, or backend-specific objects.  The library never
fetches data itself.

## C API

The complete API is [`include/criu_provider.h`](include/criu_provider.h).  It
uses opaque C handles, so a backend can call it from C++, Rust, Go, or C.
Functions return `0` on success and a negative errno value on failure.  An FD
callback returns a non-negative FD or a negative errno value.

| API | What the backend does | What happens |
| --- | --- | --- |
| `criu_provider_plan_from_checkpoint()` | Pass a finished local checkpoint directory. | Reads CRIU metadata and builds a plan. |
| `criu_provider_plan_write()` / `_load()` | Write or load `criu-provider_plan.json`. | Stores or validates the JSON plan. |
| `_requirements()`, `_enumerate_source_ranges()`, `_enumerate_images()` | Reserve resources and stage listed inputs. | Reports exactly what preparation needs. |
| `criu_provider_session_prepare()` | Write planned ranges into borrowed restore FDs. | Creates the restore FDs and calls `write_ranges()` once. |
| `criu_provider_session_serve()` | Pass a `SOCK_SEQPACKET` FD. | Serves the prepared FDs to CRIU. |
| `criu_provider_dump_session_*()` | Pass writable-output, commit, and abort callbacks. | Serves CRIU dump output; flushes FDs, then calls commit or abort. |

Keep the plan and callback context alive until its session is destroyed.  A
session owns its prepared and output FDs; destroying it closes any that remain.

### Restore callbacks

`criu_provider_restore_ops` has:

- `write_ranges()`: write every supplied source range into its destination FD;
- `open_ready_image()`: return an FD for a `RESTORE_READY_LOCAL` image when
  the session has no prepared FD for that image.

`criu_provider_session_prepare()` creates the restore FDs and calls
`write_ranges()` once with every source and destination range.  The callback is
synchronous.  Destination FDs and range strings are borrowed: use them before
returning, but do not retain or close them.  Each range includes the source
image's plan role.  The library owns the FDs' size, layout, and lifetime.  The
backend may fetch the ranges while preparation is in progress.

`open_ready_image()` returns a newly opened FD.  Ownership passes to the
library, which closes it after sending a duplicate to CRIU.

### Dump callbacks

`criu_provider_dump_ops` has:

- `open_output_image()`: return a newly opened writable FD for one CRIU image;
- `commit()`: finalize provider output and create the plan after CRIU `COMMIT`;
  and
- `abort()`: discard staging after `ABORT` or failure.

Ownership of an output FD passes to the library.  It flushes all output FDs
before calling `commit()` and closes them when the session finishes.  If
`commit()` fails, it calls `abort()`.  The backend creates the final plan in the
commit callback.  Publishing the larger checkpoint transaction remains a
separate backend step after the session completes.

## Normal flows

Checkpoint:

```text
CRIU dump -> backend staging directory -> dump-session commit callback
-> plan_from_checkpoint -> write criu-provider_plan.json -> publish
```

Restore:

```text
load plan -> prepare the session; backend writes the listed ranges
-> socketpair -> CRIU <-> session_serve -> COMMIT or ABORT
```

Preparation creates one correctly sized sparse memfd per planned VMA, shared
object, and residual page image.  Holes stay holes.  Ready-local metadata stays
in backend-owned staging and is opened only when CRIU requests it.

### Possible backend extension: FD pool

Today the library calls `memfd_create()` and closes those FDs itself.  A future
backend API may instead lease empty, exclusive FDs from a pool.  The library
would still size, retain, and send each FD, then return the lease after CRIU
`COMMIT` or `ABORT`.

## `criu-provider_plan.json`

The sidecar is UTF-8 JSON.  The private
[`proto/criu_provider_plan.proto`](proto/criu_provider_plan.proto) file is
only the schema used by Protobuf's JSON reader/writer; it is not a public
protocol or a binary plan format.

The plan names logical images, source ranges, FD placements, objects, image
roles, and resource estimates.  It rejects unknown JSON fields, path
traversal, duplicate keys, overflow, non-raw chunks, and out-of-range data.

V1 supports full raw checkpoints: private anonymous memory, anonymous shared
memory, non-hugetlb memfds, and residual page-image data.  Parent/pre-dump
chains, hugetlb memfds, and malformed images are not accepted for provider
restore.  The checkpoint itself remains valid and can use ordinary staged
restore.

## CRIU requests

`OPEN_IMAGE` returns a duplicate of a prepared FD for
`RESTORE_PROVIDER_FD`.  For `RESTORE_READY_LOCAL`, it calls
`open_ready_image()`.  `RESTORE_LOCAL` and
`RESTORE_LOCAL_FALLBACK` return `-ENOTSUP`, so CRIU uses its local image
directory.  Unknown image names return `-EPROTO`.

`GET_VMA` and `GET_SHARED` return a duplicate for an exact plan match.  An
object that is not in the plan returns `-ENOTSUP`, so CRIU uses its normal
restore path.  A known object whose address or length disagrees with the plan
returns `-EPROTO`.  `COMMIT` and `ABORT` release provider-owned FDs.

## Directory map

```text
include/        public C API
proto/          CRIU schemas and private JSON-plan schema
src/api.cpp     C API and plan I/O
src/plan.cpp    plan validation and image rules
src/image_index.cpp
                CRIU metadata reader and plan builder
src/materializer.cpp
                sparse FD creation and backend write handoff
src/protocol.cpp restore server
src/dump.cpp     dump server
tests/           library tests
```

## Build and test

```sh
make -C agent/criu-memory-provider
make -C agent/criu-memory-provider test
```
