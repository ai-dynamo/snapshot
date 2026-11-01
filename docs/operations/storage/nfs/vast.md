<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# VAST NFS

## 1. Check the client

Install a compatible VAST NFS client on each node that checkpoints or restores.
Check the running client on the node:

```bash
vastnfs-ctl status
```

The VAST client provides `remoteports`, `localports`, and `spread_reads`.

## 2. Choose the NFS version

Start with NFSv3 for large snapshot files. It supports `spread_reads`, which
distributes reads from one file across existing connections. Combine it with
`remoteports` to use several VAST server addresses.

NFSv4.1 also works, but without `spread_reads`: each file stays on one server
address and can use multiple connections to that address. If your installation
requires NFSv4.1, set `vers=4.1` and omit
`spread_reads`. See [VAST's protocol comparison](https://kb.vastdata.com/docs/7-vast-client-nfs-multipathing).

## 3. Select NICs and connections

Count the NICs that connect the restore node to the VAST storage network.
GPU-interconnect and other network interfaces do not add storage bandwidth.
The VAST client uses one local interface by default. `nconnect` adds TCP
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

## 4. Set mount options

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

Keep normal cache sharing when several mounts access the same writable export.
The [Snapshot benchmark setup](../../../development/benchmarks-guide.md#reference-setup)
also used `nosharecache` and `nosharetransport` for mount isolation; those
options are omitted here.

## 5. Check the active path

Inspect the mount in the node agent container using the
[restore-node checks](../slow-restore.md#3-check-the-restore-node).
On the node, inspect the active VAST connections and storage NIC traffic:

```bash
vastnfs-ctl rpc-clients "<node-mount>"
ip -s link show dev "<storage-nic>"
```

Check traffic counters on each selected NIC before and after a restore. If
only one NIC carries traffic, check `localports` and routing before adding
connections. See [VAST client diagnostics](https://vastnfs.vastdata.com/docs/4.5/usage/vastnfs-ctl.html).

## 6. Select I/O mode and measure

Start with `config.criu.imageIoMode: direct` for CRIU images. Compare with
`writeback` if repeated restores on the same node can reuse the page cache.
See [NFS I/O modes](README.md).

Run fio and a real restore through the configured mount. VAST notes that
`spread_reads` can be slower for a single-threaded direct reader with one
request in flight. Use the concurrency of the actual restore when comparing
settings. See [VAST's benchmark guide](https://kb.vastdata.com/docs/how-to-benchmark-vast-data-with-fio)
and [Why is my restore slow?](../slow-restore.md).
