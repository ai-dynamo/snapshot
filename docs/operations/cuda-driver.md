<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Check the CustomStorage driver

Check the user-mode library loaded by the workload and PageBroker on both
the checkpoint and restore nodes. `nvidia-smi` reports the host driver; a
container can load a different `libcuda.so` through `LD_LIBRARY_PATH`.

On the node, inspect each process's loaded library:

```bash
sudo sed -n '/libcuda\.so/s/^[^/]*//p' /proc/<host-pid>/maps | sort -u
```

The output retains ` (deleted)` if the loaded library's file was removed.

Use matching user-mode driver libraries for the workload, node agent's CUDA
helper, and PageBroker. On a compatible older kernel-mode driver, the
`cuda-compat-13-4` package provides the newer libraries. Select them explicitly
with `LD_LIBRARY_PATH` in those containers; installing the package alone does
not select it. See [NVIDIA's forward-compatibility guide](https://docs.nvidia.com/deploy/cuda-compatibility/forward-compatibility.html).
