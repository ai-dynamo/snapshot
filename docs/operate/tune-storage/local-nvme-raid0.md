<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Local NVMe and RAID0

Use this page to store snapshots on drives that are local to one node, such as
NVMe drives combined with RAID0. The storage is on one node, so the operator and
node agent run on that node, and the source pod and restore pods must run there
too. The page covers when RAID0 helps, the local PV and PVC setup, the
difference between EC2 instance store and EBS, and how to check the array. These
are starting recommendations, and there are no published Snapshot benchmark
results for them.

For storage that is shared across nodes, see [Tune Storage](README.md).

## When to Use It

RAID0 stripes data across several drives and presents one volume. Consider it
when one NVMe drive limits restore throughput. Applications can use one
filesystem without splitting files across drives themselves.

More drives do not guarantee a matching increase in restore speed. CPU memory,
PCIe bandwidth, GPU transfers, and I/O concurrency can become the next limit.
Measure the actual restore as well as storage bandwidth.

## Setup

For installation values, see [Configure Storage](../configure-storage.md).

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

### EC2 Instance Store versus EBS

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

### Check an Existing Array

Run these commands on the restore node. Check that the snapshot path is on the
array, rather than the root filesystem:

```bash
lsblk --output NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS
cat /proc/mdstat
findmnt --target <snapshot-path> --output TARGET,SOURCE,FSTYPE,OPTIONS
```

## Verify

After you change a setting on this page, check the result with a real restore. Follow [Repeat the Real Restore](../../troubleshooting/slow-restores.md#repeat-the-real-restore) and compare the restore time with the time before the change.
Test direct I/O on the array as well as buffered I/O.

Also record drive count, RAID level, stripe chunk size, and filesystem.
