# CUDA checkpoint transfer core

This directory starts the transfer-neutral CUDA checkpoint operation core.
The first slice defines the integrity, configuration, cancellation, and
transfer-backend contracts shared by later CUDA operation code. It does not
deploy a helper, select a production backend, define the storage manifest, or
add a Snapshot-local coordinator.

## Ownership boundary

Snapshot owns workload orchestration, target discovery, ordering with CRIU,
and the final checkpoint or restore result. PageBroker is the production owner
of node-local CUDA execution and artifact staging. The code in this slice is a
C++ foundation used behind that PageBroker boundary.

The transfer interface intentionally does not depend on PageBroker protobufs
or on a concrete data plane. A later PageBroker engine supplies the production
adapter while preserving these contracts:

- transfer configuration is bounded before pinned memory or work is allocated;
- extent content can be incrementally hashed with SHA-256;
- failure of one extent cancels sibling work through a shared token; and
- the no-backend implementation reports unavailability without silently
  falling back to a different storage path.

## CUDA dependency

The transfer interface uses the CustomStorage types introduced by CUDA 13.4.
The validation target copies `cuda.h` from the digest-pinned CUDA 13.4
development image, while its compiler remains on the existing CUDA 13.0 agent
base. This checks the header boundary without changing the shipped runtime or
introducing a NIXL dependency.

## Validation

`make test` always runs the Go suite. When a C++20 compiler and the OpenSSL
development headers and library are available, it also runs the standalone
digest, transfer-configuration, and cancellation tests. Missing C++
prerequisites are fatal in CI and optional for local Go-only development.

`make test-cuda-helper` is the strict local target for these C++ contract
tests. The Docker target below also compiles the unavailable-backend adapter
against the pinned CUDA 13.4 header:

```text
docker build --target cuda-transfer-contracts-builder agent/
```
