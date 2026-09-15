<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Cuinterpose

Cuinterpose reconstructs shared CUDA VMM and multicast around native CUDA/CRIU.
The preload frontend is C; its lazily loaded core and standalone coordinator
are C++20. See the [design](../../../docs/development/cuinterpose.md) for
interception, ownership, capture/restore, and isolation.

## Build and test

From the repository root:

```sh
make -C agent cuinterpose-test
make -C agent cuinterpose-build
```

The digest-pinned Bookworm builder installs GCC 12, nlohmann/json 3.11.2 and
CLI11 2.1.2. The two libraries and static coordinator are exported into
`agent/cmd/cuinterpose/build/`. Always ship a matched artifact set.

For native development, install `g++`, `make`, `nlohmann-json3-dev`,
`libcli11-dev`, `python3-msgpack`, and binutils, then run `make test-native`
in this directory. Strict compiler warnings, ELF export/runtime checks, C++
ownership and protocol tests, independent Python coordinator peers, and the
complete process-isolated fake-driver suite run without a GPU or CUDA SDK.
The C fixtures intentionally provide an independent CUDA ABI and real loader/
pthread fault injection; they are not a second implementation.

The backend requires glibc 2.34+, GLIBCXX 3.4.30+, and CXXABI 1.3.13+ in the
workload. It uses the normal shared GNU exception runtime. The frontend has no
C++ dependency; the coordinator is fully static and has no ELF interpreter.
The image includes matching glibc sources and coordinator linkable objects
under `/legal/cuinterpose/dependencies` for LGPL relinking.
Private ABI version 5 and MessagePack wire/state/ticket version 4 are checked
independently of C++ layout.

## Physical-GPU qualification

Stage a fresh matched build and NVIDIA's checkpoint executable:

```sh
python3 agent/cmd/cuinterpose/tests/headless/stage_gpu.py DEST \
  --artifacts agent/cmd/cuinterpose/build \
  --cuda-checkpoint /path/to/cuda-checkpoint
```

The layout is `DEST/tests/gpu`, `DEST/build`, and `DEST/bin/cuda-checkpoint`.
Run the suite's pytest entrypoint under `cuda-checkpoint --launch-job`, using
the environment described in `tests/gpu/conftest.py`. Require all three cases
to pass with zero skips: shared/private content and unicast reconstruction,
multicast collective/graph replay, and live raw-import refusal.

Headless tests do not qualify physical bytes, real CUDA collectives, CRIU, or
PyTorch runtime coexistence. Full Snapshot qualification additionally requires
cross-node capture, restore, and post-restore inference with freshly built
agent images. The standalone suite does not test Go namespace entry.

## Implementation boundaries

| Source | Responsibility |
| --- | --- |
| `core_abi.h`, `frontend/` | Shared C ABI and glibc interception |
| `cpp/protocol.*` | Typed wire records, bounded library codec, FD transport |
| `cpp/state.*`, `driver.hpp` | Logical handles, maps, CUDA inventory and lifecycle |
| `cpp/process.cpp`, `control.cpp`, `export_cache.cpp` | Generations, fork reset, independent peer service |
| `cpp/host_carrier.cpp` | Canonical shared creator bytes and explicit CUDA cleanup |
| `cpp/multicast.cpp` | Collective-safe tracking and reconstruction |
| `cpp/coordinator.cpp` | CLI, topology validation, global barriers and durable state |

Workloads must be quiescent before prepare and parked through restore.
Never-shared allocations remain native-owned; unsupported exportable types
and live raw imports fail preflight. Fork discards inherited shim bookkeeping;
it does not promise CUDA safety after arbitrary multithreaded fork. Prefer
spawn/exec. FABRIC, PageBroker GPU content transport, save-all policies, and
selectable content backends are outside this implementation.
