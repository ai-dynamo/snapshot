// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
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
// Validate buffer_count * chunk_bytes for all devices and return the total.
size_t TransferMemoryBytes(const TransferOptions& options, size_t device_count);
// Workers for a pool of `slots` staging slots: one per 16 slots, from 1 to 16.
// Each worker then keeps about 16 storage requests in flight.
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
// I/O workers shared by all devices' slots. Workers take chunks from the oldest
// extent first and prefer slots on the target GPU's NUMA node. Each file holds
// one device extent, opened with O_DIRECT when direct_io is set. Calls may run
// concurrently and return after their storage requests and DMA drain.
// Cancellation is checked between chunks, not inside NIXL or CUDA waits.
// Contexts and the pool must outlive all calls. The pool duplicates fd and
// workers reopen it. Checkpoint includes fsync.
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
