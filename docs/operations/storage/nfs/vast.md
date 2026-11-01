<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# VAST NFS

Configure an existing VAST-backed PVC for large snapshot files. This page
covers storage NICs and mount options validated with Snapshot in the
published [benchmarks](../../../development/benchmarks.md).
See [Storage best practices](../README.md) for other backends.

## Setup

Install a compatible VAST NFS client on each node that checkpoints or restores.
The client provides `remoteports`, `localports`, and `spread_reads`.

### Choose the NFS version

Start with NFSv3 for large snapshot files. It supports `spread_reads`, which
distributes reads from one file across existing connections. Combine it with
`remoteports` to use several VAST server addresses.

NFSv4.1 also works, but without `spread_reads`: each file stays on one server
address and can use multiple connections to that address. If your installation
requires NFSv4.1, set `vers=4.1` and omit
`spread_reads`. See [VAST's protocol comparison](https://kb.vastdata.com/docs/7-vast-client-nfs-multipathing).

## Tuning

### Select NICs and connections

Count the NICs that connect the restore node to the VAST storage network.
GPU-interconnect and other network interfaces do not add storage bandwidth.
The VAST client uses one local interface by default. `nconnect` adds transport
connections; `localports` selects additional local interfaces.

For TCP, VAST recommends starting with eight connections per 100 Gbit/s:

| Storage network bandwidth | Starting `nconnect` |
| --- | --- |
| 100 Gbit/s | 8 |
| 200 Gbit/s | 16 |
| 400 Gbit/s | 32 |

For NFSv3 with two storage NICs, add `localports=<nic0>~<nic1>` to the mount
options. Use interfaces with IP addresses and routes to the selected VAST
server addresses. Configure source-based routing and ARP as described in
[VAST's client network setup](https://vastnfs.vastdata.com/docs/4.5/usage/mount-params.html).

### Set mount options

This example uses NFSv3 over TCP on a 400 Gbit/s storage path. Put the options
on your NFS PV or provisioning StorageClass:

```yaml
mountOptions:
  - vers=3
  - proto=tcp
  - rsize=1048576
  - wsize=1048576
  - nconnect=32
  - spread_reads
  - remoteports=<first-vip>-<last-vip>
```

Use the range form only for contiguous VIPs assigned to your storage view.
For separate addresses, use `remoteports=<vip1>~<vip2>`. Adjust `nconnect` for
the storage link and add `localports` for multiple NICs. The 1 MiB request
sizes reduce overhead on large reads and writes.

Leave `nosharecache` and `nosharetransport` out of this starting configuration:

- `nosharecache` gives each mount its own file cache. If the same file is
  writable through several mounts on one node, those caches can disagree.
  See the [NFS manual](https://man7.org/linux/man-pages/man5/nfs.5.html).
- `nosharetransport` gives each mount its own network connections. It is useful
  when a benchmark must use connections separate from other mounts.

The published benchmark enabled both flags. To reproduce that run, use its
[complete mount options](../../../development/benchmarks-guide.md#reference-setup).

### Select I/O mode

Start with `config.criu.imageIoMode: direct` before taking a snapshot.
For comparison with `writeback`, take a separate snapshot in each mode.

VAST notes that `spread_reads` can be slower for a single-threaded direct
reader with one request in flight. Use the concurrency of the actual restore
when comparing settings. See [VAST's benchmark guide](https://kb.vastdata.com/docs/how-to-benchmark-vast-data-with-fio).

## Verify

Inspect the mount in the node agent container using the
[restore-node checks](../slow-restore.md#3-check-the-restore-node).
On the node, check traffic on each storage NIC:

```bash
ip -s link show dev "<storage-nic>"
```

Check traffic counters on each selected NIC before and after a restore. If
only one NIC carries traffic, check `localports` and routing before adding
connections.

Use the [common measurement procedure](../slow-restore.md#5-repeat-the-real-restore).
