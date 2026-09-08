// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "checkpoint_transaction_descriptor.hpp"

#include <errno.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <unistd.h>

#include <filesystem>
#include <iostream>
#include <stdexcept>
#include <utility>

#include "criu_provider.h"

namespace snapshot::pagebroker {
CheckpointTransactionDescriptor::CheckpointTransactionDescriptor(
    Path staging_directory, StorageBackend destination_storage, TransferEngineType engine_type)
    : staging_directory_(std::move(staging_directory)), destination_storage_(std::move(destination_storage)),
      engine_type_(engine_type)
{
}

CheckpointTransactionDescriptor::~CheckpointTransactionDescriptor() { Close(); }

CheckpointTransactionDescriptor::CheckpointTransactionDescriptor(
    CheckpointTransactionDescriptor&& other) noexcept
    : staging_directory_(std::move(other.staging_directory_)),
      destination_storage_(std::move(other.destination_storage_)), engine_type_(other.engine_type_),
      dump_plan_(std::exchange(other.dump_plan_, nullptr)),
      dump_session_(std::exchange(other.dump_session_, nullptr)),
      client_socket_(std::exchange(other.client_socket_, -1)),
      server_socket_(std::exchange(other.server_socket_, -1)), server_(std::move(other.server_)) {}

CheckpointTransactionDescriptor& CheckpointTransactionDescriptor::operator=(
    CheckpointTransactionDescriptor&& other) noexcept
{
  if (this == &other) return *this;
  Close();
  staging_directory_ = std::move(other.staging_directory_);
  destination_storage_ = std::move(other.destination_storage_);
  engine_type_ = other.engine_type_;
  dump_plan_ = std::exchange(other.dump_plan_, nullptr);
  dump_session_ = std::exchange(other.dump_session_, nullptr);
  client_socket_ = std::exchange(other.client_socket_, -1);
  server_socket_ = std::exchange(other.server_socket_, -1);
  server_ = std::move(other.server_);
  return *this;
}

int CheckpointTransactionDescriptor::OpenOutput(void* context, const char* image, int)
{
  auto* self = static_cast<CheckpointTransactionDescriptor*>(context);
  if (!image) return -EINVAL;
  const std::filesystem::path name(image);
  if (name.filename() != name) return -EINVAL;
  const int fd = open((self->staging_directory_ / name).c_str(),
      O_RDWR | O_CREAT | O_EXCL | O_CLOEXEC, 0600);
  return fd < 0 ? -errno : fd;
}

int CheckpointTransactionDescriptor::FinishDump(void* context, const criu_provider_plan*)
{
  auto* self = static_cast<CheckpointTransactionDescriptor*>(context);
  criu_provider_plan* plan = nullptr;
  const int index_result = criu_provider_plan_from_checkpoint(
      self->staging_directory_.c_str(), &plan);
  if (index_result != 0) {
    std::cerr << "criu memory provider: cannot create optional plan: "
              << index_result << '\n';
    return 0;
  }
  const int write_result = criu_provider_plan_write(plan,
      (self->staging_directory_ / "criu-provider.plan").c_str());
  criu_provider_plan_destroy(plan);
  if (write_result != 0)
    std::cerr << "criu memory provider: cannot write optional plan: "
              << write_result << '\n';
  return 0;
}

void CheckpointTransactionDescriptor::AbortDump(void*) {}

void CheckpointTransactionDescriptor::StartProvider()
{
  if (criu_provider_dump_plan_create(getpagesize(), &dump_plan_) != 0)
    throw std::runtime_error("create direct checkpoint provider plan failed");
  const criu_provider_dump_ops ops{OpenOutput, FinishDump, AbortDump};
  if (criu_provider_dump_session_create(dump_plan_, &ops, this, &dump_session_) != 0)
    throw std::runtime_error("create direct checkpoint provider failed");
  int sockets[2];
  if (socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, sockets) < 0)
    throw std::runtime_error("create direct checkpoint provider socket failed");
  client_socket_ = sockets[0];
  server_socket_ = sockets[1];
  server_ = std::thread([this] { criu_provider_dump_session_serve(dump_session_, server_socket_); });
}

int CheckpointTransactionDescriptor::TakeProviderSocket()
{
  return std::exchange(client_socket_, -1);
}

void CheckpointTransactionDescriptor::Close()
{
  if (client_socket_ >= 0) close(client_socket_);
  client_socket_ = -1;
  if (server_socket_ >= 0) shutdown(server_socket_, SHUT_RDWR);
  if (server_.joinable()) server_.join();
  if (server_socket_ >= 0) close(server_socket_);
  server_socket_ = -1;
  criu_provider_dump_session_destroy(dump_session_);
  dump_session_ = nullptr;
  criu_provider_plan_destroy(dump_plan_);
  dump_plan_ = nullptr;
}

const Path&
CheckpointTransactionDescriptor::staging_directory() const
{
  return staging_directory_;
}

const StorageBackend&
CheckpointTransactionDescriptor::destination_storage() const
{
  return destination_storage_;
}

TransferEngineType
CheckpointTransactionDescriptor::engine_type() const
{
  return engine_type_;
}
}  // namespace snapshot::pagebroker
