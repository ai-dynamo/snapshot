<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Local NVMe and RAID0

## When to use RAID0

RAID0 stripes data across several drives and presents one volume. Consider it
when one NVMe drive limits restore throughput. Applications can use one
filesystem without splitting files across drives themselves.

More drives do not guarantee a matching increase in restore speed. CPU memory,
PCIe bandwidth, GPU transfers, and I/O concurrency can become the next limit.
Measure the actual restore as well as storage bandwidth.

## Storage and scheduling

RAID0 is node-local. It does not make snapshots available on other nodes.
Snapshot still needs a PVC and the placement described in
[Storage configuration](../storage.md). Components that mount a local claim
must run on its node, and a restore must have access to the snapshot there.

RAID0 has no redundancy. Keep another copy of snapshots that must survive a
drive failure. On EC2, instance-store data is also lost when the instance stops
or terminates. See [Linux RAID arrays](https://docs.kernel.org/admin-guide/md.html)
and [EC2 instance storage](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ssd-instance-store.html).

## Check an existing array

Run these commands on the restore node. Check that the snapshot path is on the
array, rather than the root filesystem:

```bash
lsblk --output NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS
cat /proc/mdstat
findmnt --target <snapshot-path> --output TARGET,SOURCE,FSTYPE,OPTIONS
```

## Measure the result

Follow [Why is my restore slow?](slow-restore.md). Compare fio and restore
results with the same I/O mode, request size, concurrency, and cache conditions.
Test direct I/O on the array as well as buffered I/O.

Record drive count, RAID level, stripe chunk size, filesystem, and versions.
If you compare RAID0 with software sharding, keep the drive count and workload
fixed. They distribute data at different layers; their gains are not additive
by default.

If fio is fast but restore is slow, check the CPU and CUDA phase times before
adding drives. A storage test does not measure the full GPU restore path.

TODO: Tested local-PVC configuration and RAID0 versus single-drive results.
