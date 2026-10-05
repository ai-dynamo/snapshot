<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# C frontend tests

From `agent/cmd/cuinterpose`, run:

```sh
python3 frontend/tests/run.py --artifacts build
```

The suite requires Linux/amd64, at least two visible NVIDIA GPUs, the installed
CUDA 13.0+ driver and CUDA 13 runtime/headers, GCC, binutils, Python MessagePack, and
matched frontend/core artifacts. It fails if these requirements are unavailable;
it does not substitute CUDA implementations or skip the GPU checks. Set
`--cuda-include /usr/local/cuda/include` if headers are not in `/opt/cuda/include`.
The driver and runtime libraries must be on the system library search paths.
The combined `make test-gpu` gate requires a CUDA 13.1+ driver for its multicast
v2 behavior tests.

Stage `core_abi.h` with the two libraries in `build/` to run without Rust tooling.
If that header is absent, the runner generates it using cbindgen and the local
Rust workspace. It builds only small test clients, never CUDA providers.

Fresh processes exercise direct allocation and byte copies on both GPUs,
explicit-handle lookup, all seven procedure resolvers, native version/alias
selection, missing symbols, provider lifetime, and missing-core refusal. The
ABI client opens the actual Rust core and checks invalid prefixes, null inputs,
concurrent idempotent registration, and rejection of a different resolver. Two
additional processes hide a required symbol or the optional multicast symbols
from that real-libcuda resolver to check startup policy.
The only provider fixture is a generic ELF plugin with a private dependency;
it checks caller scope for `dlsym(RTLD_DEFAULT)` and `dlsym(RTLD_NEXT)` and exports
no CUDA functions.

Endpoint cases run against the actual Rust core and NVIDIA driver. They cover
activation after CUDA initialization, constructor initialization, concurrent
cold startup, fork/exec ownership, stale socket replacement, conflicting paths,
permissions, and out-of-order lifecycle requests. Driver-owned threads are not
counted as shim workers. These cases do not qualify checkpoint/restore byte
preservation or multicast; those belong to the GPU behavior suite.

Synthetic driver return codes, runtime-version variants, invented procedure
addresses, fake backend tables, and forced loader-allocation/retention failures
are no longer tested here. Ordinary concurrency remains covered; deterministic
injected loader races and backend-constructor reentry have no replacement in
this suite. Hardware and toolkit variants must be exercised with their actual
installed drivers and runtimes.

The frontend finds glibc's `dlsym` with `dlvsym` and passes `RTLD_DEFAULT` and
`RTLD_NEXT` lookups to it as tail calls, so glibc searches the original caller's
scope. It inspects only lookups with an explicit handle. It does not chain
arbitrary `dlsym` replacements or interpose explicit `dlvsym` calls.
Separate loader namespaces are not CUDA-interposition targets. Tests do not
promise these unsupported composition behaviors.
