<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Check the CUDA User-Mode Driver

This page shows how to find which CUDA user-mode driver (`libcuda.so`) each
Snapshot process loads, and how to select a newer one. For the version
requirements, see [Prerequisites](../get-started/prerequisites.md).

## Which Library Each Process Loads

`nvidia-smi` reports the kernel-mode driver. The user-mode driver can differ for
each process:

- The workload loads the library that its `LD_LIBRARY_PATH` selects.
- PageBroker loads the library that the `pagebroker` container's
  `LD_LIBRARY_PATH` selects. By default, this is the host driver's library.
- `cuda-checkpoint-helper` runs only during a checkpoint or a restore. It uses
  the `agent` container's `LD_LIBRARY_PATH`. At restore, it runs inside the
  restore Pod, so the library path is resolved in that Pod.

## Check the Library

On the node, find the host PIDs. For the workload, list the processes that use a
GPU:

```bash
nvidia-smi --query-compute-apps=pid,process_name --format=csv
```

For PageBroker:

```bash
pgrep -f '^/usr/local/bin/pagebroker'
```

For each PID, print the loaded library:

```bash
sudo sed -n '/libcuda\.so/s/^[^/]*//p' /proc/<host-pid>/maps | sort -u
```

The version is in the file name. For example, `libcuda.so.615.71.09` is
user-mode driver 615.71.09.

`cuda-checkpoint-helper` exits after each operation, so it has no PID to
inspect. Check the `agent` container's library path instead:

```bash
kubectl -n <snapshot-namespace> get pods -o wide -l app.kubernetes.io/component=snapshot-agent
kubectl -n <snapshot-namespace> exec <agent-pod> -c agent -- printenv LD_LIBRARY_PATH
```

At restore, the helper searches those directories inside the restore Pod, and
loads the first `libcuda.so` it finds. List each directory in the restore Pod's
destination container:

```bash
kubectl -n <workload-namespace> exec <restore-pod> -c <restore-container> -- ls <directory>
```

The command needs a running container. If a failed restore stopped the
container, check the restore Pod's spec instead: each directory must come from
the image or from a volume mounted in the destination container.

The workload, PageBroker, and the `agent` container must use the same version.

## Select a Newer User-Mode Driver

On a compatible older kernel-mode driver, the `cuda-compat-13-4` package
provides user-mode driver 615. It installs the libraries in
`/usr/local/cuda-13.4/compat`. Installing the package alone does not select it.

Alternatively, download a compatible NVIDIA Data Center GPU driver runfile from
NVIDIA's website. Extract it with `sh <driver>.run -x`, then copy the user-mode
libraries listed in
[Forward Compatibility](https://docs.nvidia.com/deploy/cuda-compatibility/latest/forward-compatibility.html#manually-installing-from-runfile)
to a directory accessible to each container. This does not install the
kernel-mode driver. Use that directory instead of `/usr/local/cuda-13.4/compat`
in the commands below.

In each container below, make that directory available, in the image or through
a volume. Then put it first in `LD_LIBRARY_PATH`, followed by the existing
entries:

- The workload container. Set it in the Pod spec:

  ```yaml
  env:
    - name: LD_LIBRARY_PATH
      value: /usr/local/cuda-13.4/compat:<existing entries>
  ```

- The `pagebroker` container. Keep `/usr/local/lib`, which holds PageBroker's
  own libraries, including `libnixl.so`. Removing it can prevent PageBroker
  from starting.
- The `agent` container, for `cuda-checkpoint-helper`. At restore, the directory
  must also exist in the restore Pod.

The chart has no setting for the `agent` and `pagebroker` environment. Find the
agent DaemonSet, and set it there, for example:

```bash
kubectl -n <snapshot-namespace> get daemonset -l app.kubernetes.io/component=snapshot-agent
kubectl -n <snapshot-namespace> set env daemonset/<agent-daemonset> -c pagebroker \
  LD_LIBRARY_PATH=/usr/local/cuda-13.4/compat:/usr/local/lib
```

The DaemonSet then restarts its Pods. A later `helm upgrade` may remove the
change, so check it after each upgrade.

Forward compatibility applies only to some GPUs. See
[Forward Compatibility](https://docs.nvidia.com/deploy/cuda-compatibility/latest/forward-compatibility.html).

## If It Fails

If PageBroker meets the CustomStorage minimum but the workload does not, a
checkpoint stops responding in `cuCheckpointProcessLock`. When it times out, it
fails with `CUDA_ERROR_OPERATING_SYSTEM`. See
[checkpointing GPU state](../troubleshooting/checkpoints.md#checkpointing-gpu-state).
