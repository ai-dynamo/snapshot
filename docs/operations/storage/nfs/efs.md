<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Amazon EFS

See [NFS guidance](README.md). The configuration example is not yet available.

## Prerequisites

TODO: EFS filesystem, mount targets, CSI driver, and network requirements.

## I/O mode

Start with buffered I/O on EFS. Use `O_DIRECT` only if restore tests with your
workload show better performance.

[AWS's EFS performance guidance](https://docs.aws.amazon.com/efs/latest/ug/performance-tips.html)
states that each request with `O_DIRECT` requires a round trip to EFS.
`O_DIRECT` bypasses the client cache and read-ahead. This can reduce performance,
especially with small reads or one read at a time.

We recommend buffered I/O based on this AWS guidance. AWS does not prohibit
`O_DIRECT`.

### Buffered I/O and read-ahead

AWS recommends 15 MB of NFS read-ahead. With `amazon-efs-utils` 1.33.2 or later,
the EFS mount helper automatically sets read-ahead to 15 times `rsize`.
See the "Optimizing the NFS read_ahead_kb size" section in the AWS guidance above.

## Configuration example

TODO: StorageClass/PVC, mount options, and Snapshot I/O settings.

## Validation and limits

Test cold restores (data outside the client cache) and warm restores (data in
the client cache) separately. Check that inference works after each restore.

Record these settings and results:

- I/O mode
- Mount options in use
- NFS client version
- EFS throughput mode
- Restore times

We have not tested Snapshot restore performance on EFS for this guide.

TODO: Tested configuration and measured results.
