<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Amazon EFS

Tune an existing EFS-backed PVC for Snapshot.
See [Storage best practices](../README.md) for other backends.

## Setup

For the default `pageBroker.restoreMode: direct`, start with buffered CRIU
image I/O on EFS. Set this Helm value before taking the snapshot:

```yaml
config:
  criu:
    imageIoMode: writeback
```

This selects buffered I/O for CRIU images read from EFS. With
`pageBroker.restoreMode: staged`, PageBroker first copies CPU images into
tmpfs; CRIU then reads tmpfs, so this value does not select the EFS read path.
GPU payloads use a separate I/O path.

[AWS's EFS performance guidance](https://docs.aws.amazon.com/efs/latest/ug/performance-tips.html)
states that each request with `O_DIRECT` requires a round trip to EFS.
`O_DIRECT` bypasses the client cache and read-ahead. This can reduce
performance, especially with small reads or one read at a time.

## Tuning

### Buffered I/O and read-ahead

AWS recommends about 15 MB of NFS read-ahead. On Linux 5.4 or later, the EFS
mount helper in `amazon-efs-utils` 1.33.2 or later sets it to 15 times `rsize`.
This requires `optimize_readahead`, which is enabled by default. With
`rsize=1048576`, that is 15360 KiB. Check the value on the restore node where
EFS is mounted. Run this shell block inside the node agent container, using
the execution context from the [restore-node checks](../slow-restore.md#3-check-the-restore-node):

```bash
EFS_MOUNT="<efs-mount>"
EFS_DEVICE=$(findmnt --raw --noheadings --target "$EFS_MOUNT" \
  --output MAJ:MIN)
cat "/sys/class/bdi/${EFS_DEVICE}/read_ahead_kb"
```

If the value is lower, check the mount helper version and how the volume was
mounted. Follow the "Optimizing the NFS read_ahead_kb size" section in the
AWS guidance above. Read-ahead helps buffered reads; it does not tune
`O_DIRECT` reads.

### Throughput and mount options

Check the EFS throughput mode before changing client settings. Provisioned
throughput limits and Bursting credits can limit the storage path. AWS
recommends Elastic throughput with General Purpose mode for high-throughput
workloads. See [EFS performance](https://docs.aws.amazon.com/efs/latest/ug/performance.html).

Use 1 MiB read/write request sizes (`rsize=1048576`, `wsize=1048576`), as in
[AWS's recommended mount settings](https://docs.aws.amazon.com/efs/latest/ug/mounting-fs-nfs-mount-settings.html).
EFS does not support `nconnect`; use the mount helper and read-ahead settings
above to tune buffered reads.

## Verify

Use the [common measurement procedure](../slow-restore.md#5-repeat-the-real-restore).
Also record the EFS throughput mode, `amazon-efs-utils` version, and effective
`read_ahead_kb`.
