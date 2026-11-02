// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include <cstdint>
#include "cancellation.hpp"
#include <memory>
#include <span>
#include <vector>
#include "file_descriptor.hpp"

namespace snapshot::pagebroker::gpu::storage {
struct GpuDataFile;
}

// GPU transfers through CPU staging buffers and NIXL's POSIX backend.
// Ring and pooled lanes are policies of this path, not requirements on other
// GPU transfer engines such as direct storage-to-device I/O.
namespace snapshot::pagebroker::posix {
struct TransferOptions {
  size_t buffer_count = 32;
  size_t chunk_bytes = 128 * 1024 * 1024;
  bool direct_io = false;
};
// Validate buffer_count * chunk_bytes for all devices. A zero limit means no
// cap. AllocationBytes includes CUDA allocation rounding.
size_t TransferMemoryBytes(const TransferOptions& options, size_t device_count, size_t memory_limit_bytes);

// One persistent ring per device, initialized before the engine is ready.
// The caller serializes access and owns the context, stream, and file. Each file
// holds one contiguous device extent. Both operations drain storage I/O and DMA
// before returning. Open the file with O_DIRECT when direct_io is enabled. Neither
// operation changes file status flags. Cancellation and caller deadlines are
// checked between operations, including after fsync. They do not interrupt NIXL
// waits, CUDA synchronization, or fsync. NIXL waits use a separate request timeout.
// The token must outlive the call. Fatal cleanup retains the calling thread's
// resources while the daemon stops other operations and exits within 30 seconds.
class TransferBuffers {
 public:
  explicit TransferBuffers(TransferOptions options = {});
  ~TransferBuffers();
  // Physical bytes after rounding each slot to this device's allocation
  // granularity. The engine sums these values before enforcing its memory cap.
  static size_t AllocationBytes(CUcontext context, const TransferOptions& options);
  void Initialize(CUcontext context);
  void Checkpoint(int fd, CUdeviceptr device, size_t size, CUstream stream,
                  const Cancellation& cancellation);
  void Restore(int fd, CUdeviceptr device, size_t size, CUstream stream,
               const Cancellation& cancellation);
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
// Holds one validated extent open while restore prepares CUDA mappings.
// Workers reopen it only while transferring, with independent filesystem state.
class TransferFile {
 public:
  explicit TransferFile(int fd) : file_(FileDescriptor::Duplicate(fd)) {}
  int fd() const { return file_.get(); }
 private:
  FileDescriptor file_;
};
// Prepare each GPU independently before starting workers. Access setup is a
// separate pass so a GPU that cannot access the shared buffers can be dropped
// without discarding the healthy GPUs' allocations. CUDA contexts outlive this.
class TransferPoolDevice {
 public:
  TransferPoolDevice(CUcontext context, TransferOptions options, size_t lanes);
  ~TransferPoolDevice();
  void AllowAccess(std::span<TransferPoolDevice* const> devices);
 private:
  friend class TransferPool;
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
// A bounded pool of I/O workers shared by ready device extents. Lanes assign
// chunks from the oldest ready extent first, moving on as soon as its chunks are
// assigned without waiting for its tail to drain. The total slot count remains
// options.buffer_count * surviving devices. Free slots are shared by all lanes,
// preferring the target GPU's NUMA node. Calls may run concurrently and return
// only after their storage requests and DMA have drained. Lanes use private
// streams after synchronizing the caller stream. Contexts and the pool must
// outlive all calls. Stream and token ownership stays with the caller. The pool
// may keep a TransferFile open briefly after a call returns, and the int
// overloads open their own descriptions of fd. Checkpoint includes fsync, as
// with TransferBuffers.
class TransferPool {
 public:
  explicit TransferPool(std::vector<std::unique_ptr<TransferPoolDevice>> devices);
  ~TransferPool();
  void Restore(std::shared_ptr<const TransferFile> file, CUcontext context, CUdeviceptr device,
               size_t size, CUstream stream, const Cancellation& cancellation);
  void Restore(int fd, CUcontext context, CUdeviceptr device, size_t size,
               CUstream stream, const Cancellation& cancellation);
  void Checkpoint(int fd, CUcontext context, CUdeviceptr device, size_t size,
                  CUstream stream, const Cancellation& cancellation);
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
// Pre-opens one descriptor per local GPU extent during CRIU restore.
// Destruction cancels and joins preparation before releasing its descriptors.
class GpuRestorePreparation {
 public:
  explicit GpuRestorePreparation(int directory_fd);
  ~GpuRestorePreparation();
  // Returns null when preparation could not open this extent. The caller still
  // validates requested participants and opens missing files during transfer.
  std::shared_ptr<TransferFile> Get(uint32_t pid, const gpu::storage::GpuDataFile& extent, int directory);
 private:
  struct State;
  std::unique_ptr<State> state_;
};
}  // namespace snapshot::pagebroker::posix
