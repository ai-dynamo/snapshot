// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "nixl_transfer.hpp"

#include <nixl.h>
#include <nixl_descriptors.h>
#include <nixl_params.h>
#include <unistd.h>
#include <atomic>
#include <chrono>
#include <stdexcept>
#include <thread>

namespace snapshot::pagebroker {
namespace {
using Clock = std::chrono::steady_clock;

void Check(nixl_status_t status, const char* operation)
{
  if (status != NIXL_SUCCESS)
    throw std::runtime_error(std::string("NIXL ") + operation + ": " + std::to_string(status));
}
}  // namespace

struct NixlTransfer::Impl {
  struct Request {
    nixlXferReqH* handle = nullptr;
    nixl_status_t status = NIXL_SUCCESS;
    Clock::time_point started;
  };
  std::string name;
  std::unique_ptr<nixlAgent> agent;
  nixl_reg_dlist_t buffers{DRAM_SEG};
  nixl_reg_dlist_t file{FILE_SEG};
  std::vector<void*> addresses;
  std::vector<Request> requests;
  int descriptor = -1;
};

NixlTransfer::NixlTransfer(const std::vector<void*>& buffers, size_t capacity)
    : impl_(std::make_unique<Impl>())
{
  static std::atomic<unsigned> sequence{0};
  impl_->name = "pagebroker-" + std::to_string(getpid()) + "-" + std::to_string(sequence.fetch_add(1));
  nixlAgentConfig config;
  config.useProgThread = true;
  impl_->agent = std::make_unique<nixlAgent>(impl_->name, config);
  nixl_b_params_t parameters;
  parameters["use_aio"] = "true";
  nixlBackendH* backend = nullptr;
  Check(impl_->agent->createBackend("POSIX", parameters, backend), "create POSIX backend");
  impl_->addresses = buffers;
  impl_->requests.resize(buffers.size());
  for (void* address : buffers)
    impl_->buffers.addDesc(nixlBlobDesc(reinterpret_cast<uintptr_t>(address), capacity, 0));
  Check(impl_->agent->registerMem(impl_->buffers), "register buffers");
}

NixlTransfer::~NixlTransfer()
{
  // POSIX releaseXferReq does not cancel queued AIO callbacks. On failure,
  // reclaim neither request nor buffer until storage completion is known.
  try {
    Close();
    Check(impl_->agent->deregisterMem(impl_->buffers), "deregister buffers");
  } catch (...) {
    _exit(1);
  }
}

void NixlTransfer::Open(int descriptor, size_t size)
{
  if (impl_->descriptor != -1)
    throw std::logic_error("NIXL file already open");
  impl_->file.clear();
  impl_->file.addDesc(nixlBlobDesc(0, size, descriptor));
  Check(impl_->agent->registerMem(impl_->file), "register file");
  impl_->descriptor = descriptor;
}

void NixlTransfer::Submit(size_t slot, bool write, size_t offset, size_t size)
{
  auto& request = impl_->requests.at(slot);
  if (request.handle || impl_->descriptor == -1)
    throw std::logic_error("NIXL slot or file is not ready");
  nixl_xfer_dlist_t local(DRAM_SEG);
  nixl_xfer_dlist_t remote(FILE_SEG);
  local.addDesc(nixlBlobDesc(reinterpret_cast<uintptr_t>(impl_->addresses.at(slot)), size, 0));
  remote.addDesc(nixlBlobDesc(offset, size, impl_->descriptor));
  request.started = Clock::now();
  Check(impl_->agent->createXferReq(write ? NIXL_WRITE : NIXL_READ, local, remote,
                                  impl_->name, request.handle), "create request");
  request.status = impl_->agent->postXferReq(request.handle);
  if (request.status != NIXL_IN_PROG)
    Check(request.status, "post request");
}

bool NixlTransfer::Wait(size_t slot,
                        double* seconds, std::string* error)
{
  auto& request = impl_->requests.at(slot);
  if (!request.handle)
    return true;
  // A stuck backend is isolated by worker termination, never by releasing
  // registrations that an outstanding request may still access.
  const auto deadline = Clock::now() + std::chrono::seconds(240);
  while (request.status == NIXL_IN_PROG) {
    request.status = impl_->agent->getXferStatus(request.handle);
    if (Clock::now() >= deadline)
      _exit(1);
    if (request.status == NIXL_IN_PROG)
      std::this_thread::sleep_for(std::chrono::microseconds(50));
  }
  *seconds += std::chrono::duration<double>(Clock::now() - request.started).count();
  Check(impl_->agent->releaseXferReq(request.handle), "release completed request");
  request.handle = nullptr;
  if (request.status != NIXL_SUCCESS) {
    *error = "NIXL transfer failed: " + std::to_string(request.status);
    return false;
  }
  return true;
}

void NixlTransfer::Close()
{
  std::string error;
  double seconds = 0;
  bool success = true;
  for (size_t slot = 0; slot < impl_->requests.size(); ++slot)
    if (!Wait(slot, &seconds, &error)) success = false;
  if (impl_->descriptor != -1) {
    Check(impl_->agent->deregisterMem(impl_->file), "deregister file");
    impl_->descriptor = -1;
  }
  if (!success) throw std::runtime_error(error);
}
}  // namespace snapshot::pagebroker
