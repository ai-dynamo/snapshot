<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NFS

Use this guidance for shared NFS storage, then select your backend setup.
See [Storage best practices](../README.md) for other backends.

## Check the storage limits

Check the read throughput available to each restore node. Include the storage
service limit, network speed, and other clients that use the same storage.
More connections cannot increase throughput beyond these limits.

Check the NFS version and client support before you select mount options.
Use the backend examples below.

## Direct I/O (`O_DIRECT`)

Direct I/O bypasses the client page cache. Check that the filesystem supports
it, then compare restore times with buffered I/O.

Set `config.criu.imageIoMode: direct` for CRIU images. This is an application
setting, not an NFS mount option. It does not select the GPU payload I/O mode.
See the [configuration reference](../../../reference/api.md#agent-config-config).

Snapshot saves `config.criu.imageIoMode` when it takes the snapshot. Restore
uses that saved mode; changing the Helm value does not change an existing
snapshot. To compare direct and buffered I/O, take a separate snapshot in
each mode, then restore each one.

## Buffered I/O

Set `config.criu.imageIoMode: writeback` for buffered CRIU image I/O. Buffered
reads can use the client page cache and read-ahead. Allow memory for that cache.

## Mount options

| Option | What to check |
| --- | --- |
| `vers`, `proto` | NFS version and transport supported by the client and server |
| `rsize`, `wsize` | Maximum NFS read/write request sizes; larger requests can reduce overhead |
| `nconnect` | Number of transport connections; the supported range depends on the client |

Inspect the mount in the node agent container, where the snapshot path is
resolved. Follow the [restore-node checks](../slow-restore.md#3-check-the-restore-node).

## Examples

- [VAST](vast.md)
- [Amazon EFS](efs.md)

## Validate a configuration

Use the [common measurement procedure](../slow-restore.md#5-repeat-the-real-restore).
