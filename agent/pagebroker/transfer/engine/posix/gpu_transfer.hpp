// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include <cstdint>
#include "cancellation.hpp"
#include <memory>
#include <span>
#include <vector>

// GPU transfers through CPU staging buffers and NIXL's POSIX backend. The lane
// policy belongs to this path, not to other GPU transfer engines such as direct
// storage-to-device I/O.
namespace snapshot::pagebroker::posix {
struct TransferOptions {
  size_t buffer_count = 32;
  size_t chunk_bytes = 128 * 1024 * 1024;
  bool direct_io = false;
};
// Validate buffer_count * chunk_bytes for all devices. A zero limit means no
// cap. StagingAllocationBytes includes CUDA allocation rounding.
size_t TransferMemoryBytes(const TransferOptions& options, size_t device_count, size_t memory_limit_bytes);
// Physical bytes of one device's staging slots after rounding each slot to its
// allocation granularity. The engine sums these values before enforcing its cap.
size_t StagingAllocationBytes(CUcontext context, const TransferOptions& options);
// Workers for a pool of `slots` staging slots: one per 16 slots, from 1 to 16.
// Restores on B200 with VAST NFS were fastest at about 16 slots per worker for
// 32 to 256 slots. More workers leave each one too few requests in flight.
size_t PoolLanes(size_t slots);

// Prepare each GPU independently before starting workers. Access setup is a
// separate pass so a GPU that cannot access the shared buffers can be dropped
// without discarding the healthy GPUs' allocations. CUDA contexts outlive this.
class TransferPoolDevice {
 public:
  TransferPoolDevice(CUcontext context, TransferOptions options);
  ~TransferPoolDevice();
  void AllowAccess(std::span<TransferPoolDevice* const> devices);
 private:
  friend class TransferPool;
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
// A bounded pool of I/O workers shared by ready device extents. Lanes assign
// chunks from the oldest ready extent first, moving on as soon as its chunks are
// assigned without waiting for its tail to drain. The total slot count is
// options.buffer_count * surviving devices, and PoolLanes sets the worker count.
// Free slots are shared by all lanes, preferring the target GPU's NUMA node.
// Each file holds one contiguous device extent. Open it with O_DIRECT when
// direct_io is enabled. Calls may run concurrently and return only after their
// storage requests and DMA have drained. Lanes use private streams after
// synchronizing the caller stream. Cancellation and caller deadlines are checked
// between chunks. They do not interrupt NIXL waits, CUDA synchronization, or
// fsync. Contexts and the pool must outlive all calls. Stream and token
// ownership stays with the caller. The pool duplicates fd for each job and may
// keep it open briefly after a call returns. Workers reopen it with independent
// filesystem state. Checkpoint includes fsync.
class TransferPool {
 public:
  explicit TransferPool(std::vector<std::unique_ptr<TransferPoolDevice>> devices);
  ~TransferPool();
  void Restore(int fd, CUcontext context, CUdeviceptr device, size_t size,
               CUstream stream, const Cancellation& cancellation);
  void Checkpoint(int fd, CUcontext context, CUdeviceptr device, size_t size,
                  CUstream stream, const Cancellation& cancellation);
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}  // namespace snapshot::pagebroker::posix
