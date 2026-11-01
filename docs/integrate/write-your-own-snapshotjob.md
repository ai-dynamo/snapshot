<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Write Your Own `SnapshotJob`

`spec.podTemplate` only needs the workload's own container spec — image,
command, resources, and any volumes it mounts. The controller injects the
`/snapshot-control` volume and mount, `SNAPSHOT_CONTROL_DIR`, the
`ready-for-snapshot` readiness probe, and the seccomp profile before creating the
source pod.

Each recipe ships a ready-to-apply `SnapshotJob`. Its shape is:

```yaml
apiVersion: nvidia.com/v1alpha1
kind: SnapshotJob
metadata:
  name: vllm-qwen3-0-6b-snapshot
spec:
  podSnapshotTemplate:
    targetContainers:
      - main
  podTemplate:
    spec:
      containers:
        - name: main
          # The workload's own container spec: image, command, args,
          # resources, and the volumes it mounts, such as the app.py ConfigMap.
```

For a multi-GPU workload, add this annotation to `spec.podTemplate.metadata.annotations`
of your own `SnapshotJob`. The controller then prepares the source pod for multi-GPU
checkpointing. See [Multi-GPU Workloads](workload-contract.md#multi-gpu-workloads):

```yaml
spec:
  podTemplate:
    metadata:
      annotations:
        nvidia.com/snapshot-multi-gpu: enabled
```

The framework recipes do not need `/dev/net/tun`. CRIU can log a
nonfatal TUN capability-probe error when the device is absent. See
[TUN probe errors](../troubleshooting/agent.md#criu-reports-unable-to-create-tun).
