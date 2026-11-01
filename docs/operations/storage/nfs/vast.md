<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# VAST NFS example

See [NFS guidance](README.md).

## Prerequisites

Use a compatible VAST NFS client on each restore node. The options below apply
to NFSv3 over TCP. `spread_reads` and `remoteports` require the VAST client;
do not copy them to the EFS configuration.

TODO: CSI driver and StorageClass/PVC example.

## Configuration example

### Mount options

Start from the mount options in the
[Snapshot benchmark setup](../../../development/benchmarks-guide.md#reference-setup):

```yaml
mountOptions:
  - vers=3
  - proto=tcp
  - rsize=1048576
  - wsize=1048576
  - nconnect=32
  - spread_reads
  - remoteports=<first-vip>-<last-vip>
  - nosharecache
  - nosharetransport
```

Replace the address range with the VAST storage addresses for your installation.
This is a mount-options fragment, not a complete StorageClass or PV.

| Option | Purpose |
| --- | --- |
| `nconnect=32` | Use up to 32 transport connections |
| `spread_reads` | Allow reads from one file to use multiple connections |
| `remoteports` | Connect to multiple VAST server addresses |
| `rsize=1048576`, `wsize=1048576` | Allow NFS requests up to 1 MiB |
| `nosharecache`, `nosharetransport` | Use separate cache and transports for this mount, as in the benchmark |

VAST documents this combination in its
[benchmark guide](https://kb.vastdata.com/docs/how-to-guide-for-benchmarking-vast-data).
`spread_reads` applies to NFSv3, not NFSv4. More connections are not always
faster; select the count for your network and workload.

### I/O mode

Compare direct and buffered I/O as described in the [NFS guide](README.md).
VAST notes that `spread_reads` can hurt single-threaded direct reads with only
one request in flight. Test the full restore path before you select a mode.

## Validation and limits

Check the mount options in use on the restore node. A PV declaration alone does
not prove that the client uses those options.

The linked Snapshot benchmarks used 32 connections and four server addresses.
They do not isolate the benefit of either option. This example does not claim
that 32 connections are best for every installation.
