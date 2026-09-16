<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# C frontend tests

From `agent/cmd/cuinterpose`, run:

```sh
python3 frontend/tests/run.py --artifacts build
```

Linux/amd64, GCC, Python with MessagePack, and matched packaged artifacts are
required; no CUDA SDK, GPU, or root access is needed. `--loader-only` uses an
independent mock core before the Rust core is assembled.

Small C shared libraries model the driver, runtime, and versioned backend ABI.
Fresh processes exercise direct calls, all seven resolvers, legacy/v2 argument
forwarding, query failures, non-CUDA passthrough, provider lifetime, lazy loading,
ABI rejection, and constructor reentry. Actual-core cases verify endpoint
activation, focused fork identity reset, and sticky startup failure. These
fixtures are not CUDA device or checkpoint/restore qualification.

Lookup uses glibc `dlvsym` to bootstrap `dlsym`. It does not preserve the
original caller's `RTLD_NEXT` scope through another interposer, chain arbitrary
`dlsym` replacements, or interpose explicit `dlvsym` calls. Separate loader
namespaces are not CUDA-interposition targets. Tests do not promise these
unsupported composition behaviors.
