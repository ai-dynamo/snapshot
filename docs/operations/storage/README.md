<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Storage best practices

Choose and tune storage for Snapshot. For installation values and staging
memory, see [Storage configuration](../storage.md).

## Choose a backend

The published Snapshot [benchmarks](../../development/benchmarks.md) validate
the VAST NFS setup. The EFS, local NVMe/RAID0, and S3 pages provide starting
recommendations without published Snapshot benchmark results.

| Backend | Shared across nodes | Restore on a different node | Durability | Setup effort |
| --- | --- | --- | --- | --- |
| [NFS: VAST or EFS](nfs/README.md) | Yes | Yes | Provided by the storage service | Shared PVC and NFS client |
| [Local NVMe and RAID0](local/README.md) | No | No, node-local in this setup | RAID0 has no redundancy; EC2 instance store is temporary | Local array, PV/PVC, and node placement |
| [Amazon S3](s3.md) | Yes | Yes | Provided by S3 | Bucket, credentials, and network access |

For NFS, select the [VAST](nfs/vast.md) or [EFS](nfs/efs.md) setup after
reading the common I/O guidance.

## Why is my restore slow?

Use [Why is my restore slow?](slow-restore.md) to identify the slow phase,
check the active storage path, and compare fio with a real restore.
