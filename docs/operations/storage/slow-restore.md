<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Why is my restore slow?

## 1. Find the slow phase

Read the `Restore timing summary` from the node agent that handled the restore.
Compare CPU restore, GPU restore, and staging times. Measure application
readiness and the first inference request separately.
See [restore timing guidance](../storage.md#diagnose-slow-gpu-restores).

## 2. Check the storage configuration

Read the [NFS guidance](nfs/README.md), then the page for your backend:
[VAST](nfs/vast.md) or [EFS](nfs/efs.md). For local storage, read
[Local NVMe and RAID0](nvme.md).
For object storage, check [S3 Region and instance bandwidth](s3.md).
Check storage throughput limits and direct versus buffered I/O before tuning.

## 3. Check the restore node

Record the Snapshot image versions, Linux kernel, GPU driver, and NFS client.
Compare them with the [compatibility requirements](../../reference/compatibility.md).
Run these commands on the restore node:

```bash
uname -r
nvidia-smi --query-gpu=name,driver_version --format=csv
```

For GPU CustomStorage, also check the
[loaded CUDA user-mode driver](../../reference/compatibility.md#check-the-customstorage-driver)
in the workload and PageBroker. The host driver version alone is not enough.

Select the node agent Pod on the restore node. Run `findmnt` in its container
so path lookup uses the agent's mount namespace and root:

```bash
kubectl --context "<context>" --namespace "<snapshot-namespace>" \
  exec "<node-agent-pod>" --container agent -- \
  /usr/bin/findmnt --target "<snapshot-path>" \
  --output TARGET,SOURCE,FSTYPE,OPTIONS
```

For VAST, follow the [client and network checks](nfs/vast.md). Check the running
client, storage NICs, and connections before increasing transfer buffers.

## 4. Measure storage with fio

[fio](https://fio.readthedocs.io/en/master/fio_doc.html) generates read/write
requests and reports bandwidth, IOPS, and latency. Match the file, I/O mode,
request size, and concurrency to the restore path.

This example reads an existing file, with one buffered request at a time.
Run it through the same storage mount as the node agent:

```bash
fio --name=restore-read --filename=<snapshot-file> \
  --readonly --allow_file_create=0 --rw=read --bs=1M \
  --ioengine=sync --iodepth=1 --direct=0 --invalidate=0 --group_reporting
```

This test keeps existing file-cache contents. Record whether the file is cold
or warm. For an asynchronous direct-read test, use `--ioengine=libaio`,
`--direct=1`, and a suitable `--iodepth`. Check the achieved depth in fio's
output. Compare results only when the test conditions match.

### Why fio can be faster than restore

For normal file reads, fio reuses a pool of destination buffers. Once those
pages are resident, later reads usually avoid first-touch allocation faults.
It does not allocate fresh destination pages for every file block.
See the [fio 3.41 buffer setup](https://github.com/axboe/fio/blob/fio-3.41/backend.c#L1327-L1405)
and [buffer reuse](https://github.com/axboe/fio/blob/fio-3.41/io_u.c#L812-L838).

Memory pressure can evict buffer pages and cause more faults. File-cache
misses are separate from destination-buffer faults; `direct=1` does not
remove the latter.

A restore must also populate the workload's memory and restore process and
GPU state. Use fio to check the storage path; its bandwidth alone does not
predict the full restore time.

## 5. Repeat the real restore

Change one setting at a time. Keep the workload and snapshot fixed. Record
phase times, cache state, mount options, and versions for each run.
Check inference after every restore. Use the
[Snapshot benchmarks](../../development/benchmarks-guide.md) for repeatable
workload measurements.
