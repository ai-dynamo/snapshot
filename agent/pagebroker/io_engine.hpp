// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <cstddef>
#include <functional>
#include <memory>
#include <span>

#include "transfer_engine.hpp"

namespace snapshot::pagebroker {

// Registers caller-owned host buffers for asynchronous file I/O. Buffers must
// outlive the engine. Close drains requests before the caller closes the file.
// The caller serializes access. Unsafe cleanup retains resources until daemon shutdown.
class NixlTransferEngine final : public TransferEngine {
 public:
  enum class Fault { DrainTimeout, RequestRelease, FileRelease, BufferRelease };
  // Optional per-instance control for exceptional-path tests. Normal callers omit it.
  using FaultInjector = std::function<bool(Fault)>;
  NixlTransferEngine(std::span<void* const> buffers, size_t capacity, FaultInjector inject = {});
  ~NixlTransferEngine() override;
  IoEngine type() const override { return IoEngine::NIXL; }

  void Open(int descriptor, size_t size) override;
  void Submit(size_t slot, io::Operation operation, size_t offset, size_t size) override;
  void Wait(size_t slot) override;
  void Close() override;

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};

}  // namespace snapshot::pagebroker
