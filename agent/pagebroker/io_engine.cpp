// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "io_engine.hpp"
#include "fatal_cleanup.hpp"

#include <nixl.h>
#include <nixl_descriptors.h>
#include <nixl_params.h>
#include <nvtx3/nvtx3.hpp>
#include <unistd.h>

#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <exception>
#include <optional>
#include <stdexcept>
#include <string>
#include <thread>
#include <utility>
#include <vector>

namespace snapshot::pagebroker {
namespace {

using Clock = std::chrono::steady_clock;
constexpr auto kRequestTimeout = std::chrono::seconds{240};
constexpr auto kPollInterval = std::chrono::microseconds{50};

void CheckNixl(nixl_status_t status, const char* operation)
{
  if (status != NIXL_SUCCESS) {
    throw std::runtime_error(std::string("NIXL ") + operation + ": " + std::to_string(status));
  }
}

}  // namespace

struct NixlTransferEngine::Impl {
  explicit Impl(FaultInjector inject) : inject(std::move(inject)) {}
  bool Inject(Fault fault) const { return inject && inject(fault); }
  FaultInjector inject;

  struct Request {
    nixlXferReqH* handle = nullptr;
    nixl_status_t status = NIXL_SUCCESS;
    Clock::time_point deadline;
    std::optional<nvtx3::unique_range> range;
  };

  std::string name;
  std::unique_ptr<nixlAgent> agent;
  nixl_reg_dlist_t buffers{DRAM_SEG};
  nixl_reg_dlist_t file{FILE_SEG};
  std::vector<void*> addresses;
  std::vector<Request> requests;
  int descriptor = -1;
};

NixlTransferEngine::NixlTransferEngine(std::span<void* const> buffers, size_t capacity, FaultInjector inject)
    : impl_(std::make_unique<Impl>(std::move(inject)))
{
  auto& impl = *impl_;
  static std::atomic<unsigned> sequence{0};
  impl.name = "pagebroker-" + std::to_string(getpid()) + "-" + std::to_string(sequence.fetch_add(1));
  nixlAgentConfig config;
  config.useProgThread = true;
  impl.agent = std::make_unique<nixlAgent>(impl.name, config);
  nixl_b_params_t parameters;
  parameters["use_aio"] = "true";
  nixlBackendH* backend = nullptr;
  CheckNixl(impl.agent->createBackend("POSIX", parameters, backend), "create POSIX backend");
  impl.addresses.assign(buffers.begin(), buffers.end());
  impl.requests.resize(buffers.size());
  for (void* address : buffers) {
    impl.buffers.addDesc(nixlBlobDesc(reinterpret_cast<uintptr_t>(address), capacity, 0));
  }
  CheckNixl(impl.agent->registerMem(impl.buffers), "register buffers");
}

NixlTransferEngine::~NixlTransferEngine()
{
  // POSIX releaseXferReq does not cancel queued AIO callbacks. Wait for I/O
  // before releasing requests or buffers.
  try {
    Close();
  } catch (const std::exception& error) {
    std::fprintf(stderr, "PageBroker I/O cleanup: %s\n", error.what());
  } catch (...) {
    std::fprintf(stderr, "PageBroker I/O cleanup: unknown error\n");
  }
  const auto status = impl_->Inject(Fault::BufferRelease) ? NIXL_ERR_BACKEND
      : impl_->agent->deregisterMem(impl_->buffers);
  if (status != NIXL_SUCCESS) {
    ReportFatalCleanup("NIXL buffer deregistration failed");
  }
}

void NixlTransferEngine::PromoteCheckpoint(const Path&, const StorageBackend&) const
{
  throw std::logic_error("NIXL does not support directory promotion");
}

void NixlTransferEngine::Open(int descriptor, size_t size)
{
  auto& impl = *impl_;
  if (impl.descriptor != -1) {
    throw std::logic_error("NIXL file already open");
  }
  impl.file.clear();
  impl.file.addDesc(nixlBlobDesc(0, size, descriptor));
  CheckNixl(impl.agent->registerMem(impl.file), "register file");
  impl.descriptor = descriptor;
}

void NixlTransferEngine::Submit(size_t slot, io::Operation operation, size_t offset, size_t size)
{
  auto& impl = *impl_;
  auto& request = impl.requests.at(slot);
  if (request.handle || impl.descriptor == -1) {
    throw std::logic_error("NIXL slot or file is not ready");
  }
  nixl_xfer_dlist_t local(DRAM_SEG);
  nixl_xfer_dlist_t remote(FILE_SEG);
  local.addDesc(nixlBlobDesc(reinterpret_cast<uintptr_t>(impl.addresses.at(slot)), size, 0));
  remote.addDesc(nixlBlobDesc(offset, size, impl.descriptor));
  request.deadline = Clock::now() + kRequestTimeout;
  request.range.emplace(operation == io::Operation::Write ? "PageBroker storage write" : "PageBroker storage read");
  CheckNixl(impl.agent->createXferReq(operation == io::Operation::Write ? NIXL_WRITE : NIXL_READ,
                                    local, remote, impl.name, request.handle), "create request");
  request.status = impl.agent->postXferReq(request.handle);
  if (impl.Inject(Fault::DrainTimeout) && (request.status == NIXL_SUCCESS || request.status == NIXL_IN_PROG)) {
    request.status = NIXL_IN_PROG;
    request.deadline = Clock::now();
  }
  if (request.status != NIXL_IN_PROG) {
    CheckNixl(request.status, "post request");
  }
}

void NixlTransferEngine::Wait(size_t slot)
{
  auto& impl = *impl_;
  auto& request = impl.requests.at(slot);
  if (!request.handle) {
    request.range.reset();
    return;
  }
  const nvtx3::scoped_range range{"PageBroker storage wait"};
  while (request.status == NIXL_IN_PROG) {
    request.status = impl.agent->getXferStatus(request.handle);
    if (impl.Inject(Fault::DrainTimeout) && request.status == NIXL_SUCCESS) {
      request.status = NIXL_IN_PROG;
    }
    if (request.status == NIXL_IN_PROG && Clock::now() >= request.deadline) {
      ReportFatalCleanup("NIXL transfer did not drain before its deadline");
    }
    if (request.status == NIXL_IN_PROG) {
      std::this_thread::sleep_for(kPollInterval);
    }
  }
  const auto released = impl.Inject(Fault::RequestRelease) ? NIXL_ERR_BACKEND
      : impl.agent->releaseXferReq(request.handle);
  if (released != NIXL_SUCCESS) {
    ReportFatalCleanup("release completed NIXL request failed");
  }
  request.handle = nullptr;
  request.range.reset();
  CheckNixl(request.status, "transfer");
}

void NixlTransferEngine::Close()
{
  auto& impl = *impl_;
  std::exception_ptr failure;
  for (size_t slot = 0; slot < impl.requests.size(); ++slot) {
    try {
      Wait(slot);
    } catch (...) {
      if (!failure) {
        failure = std::current_exception();
      }
    }
  }
  if (impl.descriptor != -1) {
    const auto released = impl.Inject(Fault::FileRelease) ? NIXL_ERR_BACKEND
        : impl.agent->deregisterMem(impl.file);
    if (released != NIXL_SUCCESS) {
      ReportFatalCleanup("NIXL file deregistration failed");
    }
    impl.descriptor = -1;
  }
  // Ordinary I/O failure does not prevent releasing every request and file.
  if (failure) {
    std::rethrow_exception(failure);
  }
}

}  // namespace snapshot::pagebroker
