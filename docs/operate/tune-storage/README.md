<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Tune Storage

How fast a snapshot restores depends on the storage it is read from. This
section helps you choose a storage type for the snapshot store and tune it for
restores.

- To compare the options, start with [Choose Storage](#choose-storage), then open
  the page for your choice.
- If a restore is already slow, go to
  [Why Is My Restore Slow?](#why-is-my-restore-slow).

To install the snapshot store and size staging memory, see
[Configure Storage](../configure-storage.md).

## Choose Storage

Each storage option below uses one of two `storage.type` values. NFS (VAST or
Amazon EFS) and local NVMe with RAID0 are both set up as a PVC, so set
`storage.type: pvc` for them. Amazon S3 uses `storage.type: s3`. For the settings
behind each value, see
[The Snapshot Volume](../configure-storage.md#the-snapshot-volume) and
[Other Backends](../configure-storage.md#other-backends).

> [!NOTE]
> The published Snapshot [benchmarks](../../overview/performance.md) validate
> the VAST NFS setup. The EFS, local NVMe/RAID0, and S3 pages provide starting
> recommendations without published Snapshot benchmark results.

| Storage | Shared across nodes | Restore on a different node | Durability | Setup effort |
| --- | --- | --- | --- | --- |
| [NFS: VAST or EFS](nfs/README.md) | Yes | Yes | Provided by the storage service | Shared PVC and NFS client |
| [Local NVMe and RAID0](local-nvme-raid0.md) | No | No, node-local in this setup | RAID0 has no redundancy; EC2 instance store is temporary | Local array, PV/PVC, and node placement |
| [Amazon S3](amazon-s3.md) | Yes | Yes | Provided by S3 | Bucket, credentials, and network access |

For NFS, start with [NFS](nfs/README.md). It covers what applies to both VAST and
EFS: storage limits, direct and buffered I/O, and mount options. Then open the
[VAST](nfs/vast-nfs.md) or [EFS](nfs/amazon-efs.md) page for your setup.

## Why Is My Restore Slow?

Use [Troubleshoot Slow Restores](../../troubleshooting/slow-restores.md) to identify the slow phase,
check the active storage path, and compare fio with a real restore.
