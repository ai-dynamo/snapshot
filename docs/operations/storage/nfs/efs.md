<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Amazon EFS

See [NFS guidance](README.md).

## I/O mode

Start with buffered CRIU image I/O on EFS. Set this Helm value:

```yaml
config:
  criu:
    imageIoMode: writeback
```

This selects buffered I/O for CRIU images. GPU payloads use a separate I/O path.

[AWS's EFS performance guidance](https://docs.aws.amazon.com/efs/latest/ug/performance-tips.html)
states that each request with `O_DIRECT` requires a round trip to EFS.
`O_DIRECT` bypasses the client cache and read-ahead. This can reduce
performance, especially with small reads or one read at a time.

### Buffered I/O and read-ahead

AWS recommends about 15 MB of NFS read-ahead. On Linux 5.4 or later, the EFS
mount helper in `amazon-efs-utils` 1.33.2 or later sets it to 15 times `rsize`.
This requires `optimize_readahead`, which is enabled by default. With
`rsize=1048576`, that is 15360 KiB. Check the value on the restore node where
EFS is mounted:

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

## Throughput and mount options

Check the EFS throughput mode before changing client settings. Provisioned
throughput limits and Bursting credits can limit the storage path. AWS
recommends Elastic throughput with General Purpose mode for high-throughput
workloads. See [EFS performance](https://docs.aws.amazon.com/efs/latest/ug/performance.html).

Use 1 MiB read/write request sizes (`rsize=1048576`, `wsize=1048576`), as in
[AWS's recommended mount settings](https://docs.aws.amazon.com/efs/latest/ug/mounting-fs-nfs-mount-settings.html).
EFS does not support `nconnect`; use the mount helper and read-ahead settings
above to tune buffered reads.

## Validation and limits

Test cold restores (data outside the client cache) and warm restores (data in
the client cache) separately. Check that inference works after each restore.

Record these settings and results:

- I/O mode
- Mount options in use, including `rsize` and `wsize`
- NFS client version
- Linux kernel version
- `amazon-efs-utils` version
- EFS throughput mode
- Effective `read_ahead_kb`
- Restore times
