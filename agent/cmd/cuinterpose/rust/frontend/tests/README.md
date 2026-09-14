<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Standalone Rust front-end tests

From the cuinterpose Rust workspace:

```sh
python3 frontend/tests/run.py
```

The runner requires Linux/amd64, Rust, Python 3, and `/usr/bin/gcc`. It builds
only the Rust `cuinterpose` front end and compiles independent C fixtures in
a temporary directory. No CUDA toolkit, GPU, Python packages, or root access
is required. To test an existing artifact:

```sh
python3 frontend/tests/run.py --frontend /path/to/libcuinterpose.so
```

The default GNU target linker is `/usr/bin/gcc`, overridable through
`CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER`. This avoids accidentally
linking against a Nix compiler's incompatible glibc. This is not a production
glibc-baseline check.

The driver fixture exports **actual CUDA symbol names**. Its resolver returns
addresses of its versioned definitions, and `-Bsymbolic-functions` prevents
the fixture's own pointers from resolving to the preloaded shim. Thus
`dladdr` identifies `cuMulticastBindMem_v2`, not a differently named mock.
No production mapping of fake names is needed.

The core fixture is deliberately a small C implementation of the private
versioned host/core ABI. It returns the host-resolved driver function pointer
without checkpoint logic. This checks lazy discovery, ABI handshake,
forwarding arguments, and error returns independently of the incomplete Rust
core. A passing result does **not** validate the Rust core, host carriers,
multicast reconstruction, native CUDA, CRIU, or vLLM.

Each case runs in a fresh process. Coverage includes direct dynamic binding,
concrete driver/runtime handles, `RTLD_DEFAULT`, driver-selected legacy/v2
multicast signatures (including stack arguments), resolver self-lookup,
runtime resolver variants, missing/error/query-status results, unavailable
and incompatible cores, RTLD_LOCAL provider retention after application
`dlclose`, and original-caller `RTLD_NEXT` across two additional DSOs.
Two incompatible-core cases expose only an eight-byte prefix at the end of
a readable page, followed by a `PROT_NONE` guard page. They independently
exercise wrong-version and current-version/wrong-size rejection without
reading beyond the prefix. The Rust core unit suite checks the same boundary
for a short host table.

The `RTLD_NEXT` case uses globally preloaded objects. It does not establish
correctness for local lookup scopes, `dlmopen`, concurrent provider unload,
arbitrary other `dlsym` interposers, fork, or constructor-time reentrancy.
These tests do not instrument the Rust library with ASan.
