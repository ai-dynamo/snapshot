<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Local NVMe and RAID0

Use this setup for snapshots on a node-local array. This local-PVC example
places the operator and node agent on one storage node. Schedule source Pods
and restore Pods on that node through their workload manifests.
See [Storage best practices](../README.md) for shared backends.

## When to use it

RAID0 stripes data across several drives and presents one volume. Consider it
when one NVMe drive limits restore throughput. Applications can use one
filesystem without splitting files across drives themselves.

More drives do not guarantee a matching increase in restore speed. CPU memory,
PCIe bandwidth, GPU transfers, and I/O concurrency can become the next limit.
Measure the actual restore as well as storage bandwidth.

## Setup

For installation values, see [Storage configuration](../../storage.md).

For a single-node local-PVC deployment, create a local PV whose `local.path`
points to the mounted array. Set its `nodeAffinity` to the storage node and
bind a matching `ReadWriteOnce` PVC in the Snapshot release namespace. Use
that claim instead of the chart's default RWX claim:

```yaml
storage:
  type: pvc
  pvc:
    create: false
    name: snapshot-local
operator:
  nodeSelector:
    kubernetes.io/hostname: "<storage-node>"
daemonset:
  nodeSelector:
    kubernetes.io/hostname: "<storage-node>"
    nvidia.com/gpu.present: "true"
```

Replace the claim name and hostname with your values.
If that node is tainted, set `operator.tolerations` to match its taints.

Workload Pods do not mount the local PVC; their node agent reads and writes
the snapshot files. This is why they must run on the storage node.

RAID0 has no redundancy. Keep another copy of snapshots that must survive a
drive failure. See [Linux RAID arrays](https://docs.kernel.org/admin-guide/md.html).

### EC2 instance store versus EBS

Both instance-store and EBS volumes can appear as `/dev/nvme*`. Check the
device model before choosing disks:

```bash
lsblk --output NAME,MODEL,SERIAL,SIZE,MOUNTPOINTS
```

Instance-store data survives a reboot, but is erased when the instance stops,
hibernates, or terminates. EBS volumes persist across stop/start; deletion on
termination depends on the volume's `DeleteOnTermination` setting. See
[instance-store lifetime](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instance-store-lifetime.html)
and [EBS NVMe devices](https://docs.aws.amazon.com/ebs/latest/userguide/nvme-ebs-volumes.html).

## Tuning

### Check an existing array

Run these commands on the restore node. Check that the snapshot path is on the
array, rather than the root filesystem:

```bash
lsblk --output NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS
cat /proc/mdstat
findmnt --target <snapshot-path> --output TARGET,SOURCE,FSTYPE,OPTIONS
```

## Verify

Follow the [common measurement procedure](../slow-restore.md#5-repeat-the-real-restore).
Test direct I/O on the array as well as buffered I/O.

Also record drive count, RAID level, stripe chunk size, and filesystem.
