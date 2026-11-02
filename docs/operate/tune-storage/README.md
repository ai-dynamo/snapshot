<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Tune Storage

Choose and tune storage for Snapshot. For installation values and staging
memory, see [Configure Storage](../configure-storage.md).

## Choose Storage

NFS (VAST or EFS) and local NVMe/RAID0 use a PVC (`storage.type: pvc`). Amazon S3
uses `storage.type: s3`. See [Configure Storage](../configure-storage.md) for the
two supported types.

The published Snapshot [benchmarks](../../overview/performance.md) validate
the VAST NFS setup. The EFS, local NVMe/RAID0, and S3 pages provide starting
recommendations without published Snapshot benchmark results.

| Storage | Shared across nodes | Restore on a different node | Durability | Setup effort |
| --- | --- | --- | --- | --- |
| [NFS: VAST or EFS](nfs/README.md) | Yes | Yes | Provided by the storage service | Shared PVC and NFS client |
| [Local NVMe and RAID0](local-nvme-raid0.md) | No | No, node-local in this setup | RAID0 has no redundancy; EC2 instance store is temporary | Local array, PV/PVC, and node placement |
| [Amazon S3](amazon-s3.md) | Yes | Yes | Provided by S3 | Bucket, credentials, and network access |

For NFS, select the [VAST](nfs/vast-nfs.md) or [EFS](nfs/amazon-efs.md) setup after
reading the common I/O guidance.

## Why Is My Restore Slow?

Use [Troubleshoot Slow Restores](../../troubleshooting/slow-restores.md) to identify the slow phase,
check the active storage path, and compare fio with a real restore.
