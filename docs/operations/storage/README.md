<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Storage best practices

Configuration and tuning guidance by storage backend. For installation settings
and staging memory, see [Storage configuration](../storage.md).

Before tuning GPU transfers, check the
[CustomStorage driver requirement](../../reference/compatibility.md#check-the-customstorage-driver).

## Slow restores

Start with [Why is my restore slow?](slow-restore.md) to check timings, client
versions, mount options, and storage performance with fio.

## Backends

- [NFS](nfs/README.md) — direct and buffered I/O, mount options, and examples.
- [Local NVMe and RAID0](nvme.md) — striping, node placement, and measurement.
- [Amazon S3](s3.md) — Region, instance bandwidth, and parallel reads.
