<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Rust cuinterpose experiment

This workspace is an incomplete, main-based port. It builds a run-ai-style
`libcuinterpose.so` front end, a separate `libcuinterpose_core.so`, and
`cuinterpose-coordinator`. Unicast bookkeeping and host-carrier code exist;
multicast reconstruction and fork handling are not implemented. Do not treat
a successful build or loader test as GPU, CRIU, or vLLM qualification.

## Implementation boundaries

| Crate | Owns |
| --- | --- |
| `frontend` | CUDA/resolver exports, checked allocation-free ELF bootstrap, caller-relative lookup, provider retention, and lazy core loading |
| `abi` | C layouts and the single `memory_api!` signature inventory generating wrappers and the typed core table |
| `core` | Allocation state, CUDA operations, host carriers, control listener, sealed-ticket transport, and leased peer-export descriptors |
| `protocol` | C v2 binary layouts, named records and tickets, checked codecs, socket framing, and descriptor transport |
| `coordinator` | Participant discovery, named-field topology validation, phase barriers, and state-file publication |

The private host/core ABI is version 2, checked by version and size.
`cuinterpose_core_init` is the core's only dynamic export. It returns a
`repr(C)` table of typed C function pointers; memory calls do not look up
untyped core functions by name. The host's real-symbol resolver remains a
C-ABI callback because ELF lookup produces untyped addresses. The front end
loads the sibling core with `RTLD_LAZY | RTLD_LOCAL` on the first memory call.
The sibling libraries are trusted code: a matching version and size promises
valid non-null callbacks with the declared signatures. Prefix checks reject
incompatible tables; they do not validate arbitrary foreign function pointers.

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
```

An explicit system linker avoids the host's Nix compiler linking against a
newer incompatible glibc. Release qualification still needs a controlled
glibc baseline and dynamic-symbol/dependency inspection.

Unit tests include wire known-byte compatibility fixtures, malformed ELF
tables, panic poisoning, sealed memfds, and export-cache retirement races.
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
fork-without-exec, local `RTLD_NEXT` scopes, arbitrary loader namespaces, or
real multicast reconstruction.
