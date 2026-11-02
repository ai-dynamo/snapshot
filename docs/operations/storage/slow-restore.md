<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Why is my restore slow?

Use this walkthrough to find the slow phase before changing storage settings.
See [Storage best practices](README.md) for backend setup.

## 1. Find the slow phase

Read the `Restore timing summary` from the node agent that handled the restore.
Use the largest phase to choose the next check:

| Summary phase key | Check next |
| --- | --- |
| `criu_restore` | Check CPU image size, the CRIU source (PVC or staging), and I/O mode. For PVC reads, compare the same mount with fio. |
| `cuda_restore` | Compare GPU payload size with sustained storage bandwidth. If storage is fast, check CUDA setup and GPU transfers before increasing buffers. |
| `pagebroker_stage` | Check backend download/read throughput and staging memory. For S3, check the Region, instance bandwidth, and reader concurrency. |
| `pagebroker_mount`, `pagebroker_commit`, `criu_prepare` | Inspect the surrounding agent logs for mount, transaction, or CRIU preparation delays. |

Measure application readiness and the first inference request separately.
Framework wake, compilation, and autotuning after agent completion are not
phases in this summary.

For a storage lower bound, divide payload bytes by measured sustained bandwidth:
520 GB at 25 GB/s takes at least 21 seconds to read. Other restore work can add
more time. Compare your workload with the published
[restore results](../../development/benchmarks.md#cold-start-compared-with-snapshot)
and [phase breakdown](../../development/benchmarks.md#where-the-restore-time-goes),
using the same model, GPU count, and engine where possible.
For example, the published vLLM Qwen3-14B TP1 restore took 5.72 seconds on
one B200 with VAST NFS.

For a slow CUDA phase, PageBroker's NVTX ranges separate CUDA setup, payload
transfer, storage waits, and completion. Use NVTX-only tracing; CUDA API tracing
can interfere with checkpoint operations. Tune `pageBroker.transferBufferCount`
and `pageBroker.transferChunkBytes` together, accounting for their product on
every visible GPU.

## 2. Check the storage configuration

Identify whether the snapshot is on a PVC or in S3. For a PVC, check whether
CRIU reads directly from storage or from PageBroker staging:

- `pageBroker.restoreMode: direct` reads CPU images from the PVC.
- `pageBroker.restoreMode: staged` copies CPU images to tmpfs before CRIU runs.

Check the CRIU I/O mode saved in the snapshot. Changing
`config.criu.imageIoMode` affects new snapshots, not existing ones.
For S3, check that the bucket and restore node use the same Region and that
the endpoint and credentials match the bucket.

## 3. Check the restore node

Record the Snapshot image versions, Linux kernel, GPU driver, and, for NFS,
the NFS client version.
Compare them with the [compatibility requirements](../../reference/compatibility.md).
Run these commands on the restore node:

```bash
uname -r
nvidia-smi --query-gpu=name,driver_version --format=csv
```

S3 has no snapshot filesystem mount; check its endpoint, reader settings,
and network path instead of running the mount command below.

For filesystem storage, select the node agent Pod on the restore node. Run
`findmnt` in its container so path lookup uses the agent's mount namespace
and root:

```bash
kubectl --context "<context>" --namespace "<snapshot-namespace>" \
  exec "<node-agent-pod>" --container agent -- \
  /usr/bin/findmnt --target "<snapshot-path>" \
  --output TARGET,SOURCE,FSTYPE,OPTIONS
```

For NFS, confirm the client supports the mount options shown by `findmnt`.
Check storage NIC traffic and routing before increasing transfer buffers.

## 4. Measure storage with fio

[fio](https://fio.readthedocs.io/en/master/fio_doc.html) generates read/write
requests and reports bandwidth, IOPS, and latency. Match the file, I/O mode,
request size, and concurrency to the restore path.

For filesystem storage, this example reads an existing file with one buffered
request at a time. Run it through the same storage mount as the node agent:

```bash
fio --name=restore-read --filename=<snapshot-file> \
  --readonly --allow_file_create=0 --rw=read --bs=1M \
  --ioengine=sync --iodepth=1 --direct=0 --invalidate=0 --group_reporting
```

For S3, measure parallel downloads from the restore node using the same
endpoint and route as PageBroker. Measure the destination disk separately
if the download writes to a file.

This fio test keeps the existing file cache. For an asynchronous direct-read
test, use `--ioengine=libaio`, `--direct=1`, and a suitable `--iodepth`. Check
the achieved depth in fio's output.

fio reuses destination buffers, so resident buffers usually avoid repeated
first-touch allocation faults. It measures the storage path; a fast fio result
does not include the work to populate workload memory or restore CPU/GPU state.

## 5. Repeat the real restore

Change one setting at a time. Keep the model revision, snapshot, workload image,
GPU count, and checkpoint preparation fixed. For a CRIU I/O-mode comparison,
take a separate snapshot with each `config.criu.imageIoMode` value; restore
uses the mode saved in each snapshot. Compare cold and warm restores
separately. Record:

- Snapshot images and running digests, kernel, GPU driver, and storage/client versions.
- Mount options in use for filesystem storage.
- `config.criu.imageIoMode` and `pageBroker.restoreMode`.
- Cache state, request size, and concurrency for storage measurements.
- CPU, GPU, staging, and application readiness times, plus backend-specific settings.

Check inference after each restore. Preserve compatible model, compiler, and
autotuning caches when taking a new snapshot. Use the
[Snapshot benchmarks](../../development/benchmarks-guide.md) for repeatable
workload measurements.
