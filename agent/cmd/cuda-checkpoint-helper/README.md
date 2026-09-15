# CUDA checkpoint transfer core

This directory contains the transfer-neutral CUDA checkpoint operation core. The first slice defines the integrity, configuration, cancellation, and transfer-backend contracts shared by later CUDA operation code. The durable storage-manifest contract is consumed behind the existing PageBroker boundary. The private, co-versioned daemon protocol frames requests and responses between PageBroker and its CUDA helper without exposing that wire format to Snapshot. These contracts do not add a Snapshot-local daemon or coordinator or select PageBroker's production data plane.

## Ownership boundary

This C++ core preserves the lower-level transfer contracts consumed by PageBroker's CUDA execution path. These lower-level transfer contracts deliberately do not import PageBroker protobuf types. The PageBroker-owned CUDA adapter translates the existing PageBroker transaction API into these contracts so wire-format concerns do not leak into driver and I/O code. The core preserves these contracts:

- transfer configuration is bounded before pinned memory or work is allocated;
- extent content can be incrementally hashed with SHA-256;
- every newly written extent is recorded with its SHA-256 digest;
- restore verifies that digest during its only storage read;
- malformed, overlapping, incomplete, or duplicate layouts fail closed;
- manifest lifecycle assumes a PageBroker-owned directory per CUDA participant beneath the transaction-exclusive staging root; concurrent operations may not share one participant directory;
- failure of one extent cancels sibling work through a shared token; and
- the no-backend implementation reports unavailability without silently falling back to a different storage path.

## CUDA dependency

The transfer interface uses the CustomStorage types introduced by CUDA 13.4. The validation target copies `cuda.h` from the digest-pinned CUDA 13.4 development image, while its compiler remains on the existing CUDA 13.0 agent base. This checks the header boundary without changing the shipped runtime or introducing a NIXL dependency.

## Validation

`make test` always runs the Go suite. When a C++20 compiler and the OpenSSL development headers and library are available, it also runs the standalone digest, manifest, transfer-configuration, cancellation, and daemon-protocol framing tests. Missing C++ prerequisites are fatal in CI and optional for local Go-only development.

`make test-cuda-helper` is the strict local target for these C++ contract tests. The Docker target below also compiles the unavailable-backend adapter against the pinned CUDA 13.4 header:

```text
docker build --target cuda-transfer-contracts-builder agent/
```
