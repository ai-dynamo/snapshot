# CRIU memory provider

This directory builds `libcriu_provider.a`.

It is a small server-side library for CRIU external memory.  It understands a
raw CRIU checkpoint, makes the sparse file descriptors CRIU needs, and speaks
CRIU's external-memory protocol over a Unix `SOCK_SEQPACKET` socket.

It is not a daemon, a CLI, or a storage system.  It does not know about
PageBroker, Snapshot, Kubernetes, S3, credentials, or cache policy.

## Who does what

The caller owns checkpoint storage and the lifecycle around CRIU.  In this
repository that caller is PageBroker.

At checkpoint time, PageBroker gives the library a completed local checkpoint
directory.  The library reads CRIU metadata and writes
`criu-provider.plan` beside the images.  The plan is optional acceleration
metadata: if making it fails, PageBroker can still publish and restore the
ordinary checkpoint.

At restore time, PageBroker loads the plan, reads its source ranges, fetches
or stages those bytes, and creates a provider session.  The library fills its
own sparse memfds before CRIU starts.  PageBroker then gives one end of a
`SOCK_SEQPACKET` pair to CRIU and runs the library on the other end.

```text
checkpoint images + plan
        |
        v
PageBroker stages the required bytes
        |
        v
providerlib creates complete sparse FDs
        |
        v
CRIU asks providerlib for FDs over extmem
```

The important boundary is that `read_range()` must read already-ready local
bytes.  It must not fetch after PageBroker says restore is ready.

## Public API

[`include/criu_provider.h`](include/criu_provider.h) is the whole public API.
It is C so callers from C++, Rust, Go, or another language do not need to
depend on C++ classes or protobuf types.

The normal restore sequence is:

1. `criu_provider_plan_load()` loads `criu-provider.plan`.
2. `criu_provider_plan_requirements()` and
   `criu_provider_plan_enumerate_source_ranges()` tell the caller what to
   reserve and stage.
3. `criu_provider_session_create()` receives `read_range()`.
4. `criu_provider_session_prepare()` creates and fills every provider-owned
   FD.  No reads should happen after this returns.
5. The caller passes CRIU's socket endpoint to CRIU and calls
   `criu_provider_session_serve()` on its endpoint.
6. CRIU sends `COMMIT` or `ABORT`; the library closes its FDs.  The caller
   still completes or aborts its larger restore transaction separately.

The dump sequence is similar.  Start with `criu_provider_dump_plan_create()`,
add any explicit images with `criu_provider_dump_plan_add_image()`, and give
the dump session `open_output_image()`, `commit()`, and `abort()` callbacks.
The library serves CRIU's dump requests and calls `commit()` only after CRIU
sends `COMMIT`.  It calls `abort()` on `ABORT`, socket failure, or a protocol
failure.

## The plan

`criu-provider.plan` is JSON.  The private
[`proto/criu_provider_plan.proto`](proto/criu_provider_plan.proto) file is
only its typed schema for Protobuf's existing JSON reader/writer.  It is not a
wire protocol, public API, or binary plan format.

The plan records:

- logical CRIU image names and their roles;
- byte ranges to stage from each image;
- placements into private VMA, shared-memory, and residual page-image FDs;
- resource estimates; and
- the CRIU image names that are deliberately allowed to fall back to local
  files.

The library rejects malformed plans, path traversal, duplicate keys, integer
overflow, and ranges outside an image or destination FD.

V1 is for raw, full checkpoints.  It rejects parent/pre-dump chains,
compressed pages, and hugetlb memfds.  That does not make the checkpoint bad:
the caller should use its ordinary staged restore path instead.

## Files here

```text
include/criu_provider.h       C API
proto/extmem.proto            CRIU's external-memory socket messages
proto/criu-images/            CRIU image schemas needed by the indexer
proto/criu_provider_plan.proto private JSON schema for the plan
src/api.cpp                   C API, plan file I/O, session construction
src/plan.cpp                  plan validation and image-name matching
src/image_index.cpp           checkpoint metadata reader and plan builder
src/materializer.cpp          sparse memfd creation and byte placement
src/protocol.cpp              restore-side extmem server
src/dump.cpp                  dump-side extmem server and finalization
tests/provider_test.cpp       focused library tests
```

`extmem.proto` and `criu-images/*.proto` are copies of the CRIU schemas that
this library needs.  They are separate from the plan schema: CRIU uses them
for its own image files and provider socket messages.

## Build and test

From this directory:

```sh
make
make test
```

The library is written to `build/libcriu_provider.a`.  `make test` exercises
plan JSON, raw image indexing, sparse-hole materialization, restore protocol
handling, and dump image selection.  The socket test may be skipped in a
sandbox that blocks Unix socket traffic.
