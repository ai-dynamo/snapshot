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
the Rust frontend and core and compiles independent C fixtures in
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
forwarding arguments, and error returns independently of the Rust
core. Those mock-core cases do **not** validate host carriers,
multicast reconstruction, native CUDA, CRIU, or vLLM.

Each case runs in a fresh process. Coverage includes direct dynamic binding,
concrete driver/runtime handles, `RTLD_DEFAULT`, driver-selected legacy/v2
multicast signatures (including stack arguments), resolver self-lookup,
runtime resolver variants, missing/error/query-status results, unavailable
and incompatible cores, RTLD_LOCAL provider retention after application
`dlclose`, and original-caller `RTLD_NEXT` across two additional DSOs.
VMM and multicast chains run in both preload orders and count exactly one
call in each of the frontend's core, the second interceptor, and the driver.
Provider cases preserve non-CUDA same-named functions, dependencies selected
through the wrong provider family, misleading `libcuda.so.fake` names, and
explicit `dlmopen`-namespace handles without injecting the base shim.
Anonymous, unknown-alias, cross-family and non-CUDA resolver results are refused
for tracked APIs through all seven resolvers. Known v1/v2 identities remain
callable; untracked results and real errors/query statuses are preserved.
Nested runtime cases delegate through the public intercepted driver resolver
and invoke the returned unicast, multicast v1/v2, and resolver functions.
Postprocessing accepts exact known shim wrapper addresses only in the requested
API family; an unrelated shim wrapper remains an error. The anonymous/foreign
identity negatives run through the nested path as well.
An earlier preloaded non-CUDA plugin cannot preempt the frontend's own
wrapper addresses: foreign resolver results are refused, and genuine CUDA
results still select and execute the frontend rather than the plugin.
These cases run with direct and nested resolvers. The runner also rejects
dynamic relocations to the frontend's own exported functions.
The frontend's package-local `build.rs` passes `-Bsymbolic-functions` only
when linking its cdylib. This binds internal references to its definitions,
not imports or explicit caller-relative lookup, and is checked alongside
both legitimate interceptor orders.
Two incompatible-core cases expose only an eight-byte prefix at the end of
a readable page, followed by a `PROT_NONE` guard page. They independently
exercise wrong-version and current-version/wrong-size rejection without
reading beyond the prefix. The Rust core unit suite checks the same boundary
for a short host table.

Fork is not interposed. A constructor holds the loader lock while another thread enters
its first CUDA call; concurrent core reentry must
return not-initialized without poisoning the eventual initialization.
The fixture waits for the worker's futex syscall rather than using a fixed
sleep. Same-thread core-constructor reentry is covered separately. These use
the independent mock core; `core/tests/reference.py` covers Rust core generation
reset, including nested-fork FD reuse and saved host carriers.

After the 23 mock-core loader cases, 19 separate endpoint cases use the actual
Rust core. They cover directly linked `cuInit`, explicit/default lookup,
failed initialization, a private-runtime path, all seven resolver-only paths,
and a tracked query without calling the returned VMM function. Parent/child
handshakes verify generation identity. An actual-core post-fork DSO constructor
reenters a resolver while another thread initializes the generation; it must
return transient not-initialized rather than deadlock or expose partial state.
All four runtime forms additionally query a tracked API through the intercepted
driver resolver in both parent and child. A blocked endpoint path verifies
sticky startup failure propagates through both resolver layers with null output.
Python's multithreaded-fork warnings remain visible: the fixture contains no
real CUDA state and does not qualify post-CUDA fork.

The `RTLD_NEXT` case uses globally preloaded objects. It does not establish
correctness for local lookup scopes, CUDA operation across `dlmopen` namespaces,
concurrent provider unload,
arbitrary other `dlsym` interposers, general post-CUDA fork, or arbitrary
constructor-time reentrancy.
These tests do not instrument the Rust library with ASan.
