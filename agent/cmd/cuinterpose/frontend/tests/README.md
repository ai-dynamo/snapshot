<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# C frontend tests

From `agent/cmd/cuinterpose`, run:

```sh
python3 frontend/tests/run.py --artifacts build --loader-only
```

Linux/amd64, GCC, cbindgen, CUDA headers, and a built frontend are required.
`--loader-only` uses an independent mock core before the Rust core is assembled.
Omit it once matched backend artifacts and Python MessagePack are available
to run the actual-core cases as well. No GPU or root access is needed.

Small C shared libraries model the driver, runtime, and versioned backend ABI.
Fresh processes exercise direct calls, all seven resolvers, legacy/v2 argument
forwarding, query failures, non-CUDA passthrough, provider lifetime, lazy loading,
ABI rejection, concurrent cold loading, and constructor reentry. The mock
handshake registers a frontend without resolving CUDA callbacks or starting
runtime services; its dispatch table is immutable. The concurrent case holds
16 callers in the handshake before any can publish, and constructor overlap
requires both callers to succeed without a frontend-wide loading lock.
Same-thread constructor reentry is refused without poisoning later calls.
Thread-scoped loader faults verify that a failed private load reuses a published
backend, failure without a winner stays sticky, and CUDA retention failure
blocks CUDA without breaking unrelated or excluded-namespace symbol lookups.
Actual-core cases verify endpoint
activation, focused fork identity reset, and sticky startup failure. These
fixtures are not CUDA device or checkpoint/restore qualification.

Lookup uses glibc `dlvsym` to bootstrap `dlsym`. It does not preserve the
original caller's `RTLD_NEXT` scope through another interposer, chain arbitrary
`dlsym` replacements, or interpose explicit `dlvsym` calls. Separate loader
namespaces are not CUDA-interposition targets. Tests do not promise these
unsupported composition behaviors.
