<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Check the CUDA user-mode driver

For version requirements, see [Compatibility](../reference/compatibility.md).

Check the user-mode library loaded by the workload, the node agent's CUDA
helper, and PageBroker on both the checkpoint and restore nodes.
`nvidia-smi` reports the kernel-mode driver. A container can load a different
`libcuda.so` through `LD_LIBRARY_PATH`.

On the node, inspect each process's loaded library:

```bash
sudo sed -n '/libcuda\.so/s/^[^/]*//p' /proc/<host-pid>/maps | sort -u
```

Use matching user-mode driver libraries for the workload, node agent's CUDA
helper, and PageBroker. On a compatible older kernel-mode driver, the
`cuda-compat-13-4` package provides the newer libraries. Put its directory
first in `LD_LIBRARY_PATH`, followed by all existing entries. For PageBroker,
keep `/usr/local/lib` among those entries. Installing the package alone
does not select it. See [NVIDIA's forward-compatibility guide](https://docs.nvidia.com/deploy/cuda-compatibility/forward-compatibility.html).
