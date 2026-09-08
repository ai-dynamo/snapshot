// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "direct_restore_transaction_descriptor.hpp"

#include <errno.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <unistd.h>

#include <stdexcept>
#include <utility>
#include <cstdlib>

#include "criu_provider.h"
#include "s3_range_reader.hpp"

namespace snapshot::pagebroker {
DirectRestoreTransactionDescriptor::DirectRestoreTransactionDescriptor(
    Path staging_directory, uintmax_t reserved_staging_bytes)
    : staging_directory_(std::move(staging_directory)),
      reserved_staging_bytes_(reserved_staging_bytes) {}

DirectRestoreTransactionDescriptor::~DirectRestoreTransactionDescriptor() { Close(); }

DirectRestoreTransactionDescriptor::DirectRestoreTransactionDescriptor(DirectRestoreTransactionDescriptor&& other) noexcept
    : staging_directory_(std::move(other.staging_directory_)),
      reserved_staging_bytes_(std::exchange(other.reserved_staging_bytes_, 0)),
      plan_(std::exchange(other.plan_, nullptr)),
      session_(std::exchange(other.session_, nullptr)), client_socket_(std::exchange(other.client_socket_, -1)),
      server_socket_(std::exchange(other.server_socket_, -1)), preparation_(std::move(other.preparation_)), server_(std::move(other.server_)),
      s3_prefix_(std::move(other.s3_prefix_)) {}

DirectRestoreTransactionDescriptor& DirectRestoreTransactionDescriptor::operator=(DirectRestoreTransactionDescriptor&& other) noexcept
{
  if (this == &other) return *this;
  Close();
  staging_directory_ = std::move(other.staging_directory_);
  reserved_staging_bytes_ = std::exchange(other.reserved_staging_bytes_, 0);
  plan_ = std::exchange(other.plan_, nullptr);
  session_ = std::exchange(other.session_, nullptr);
  client_socket_ = std::exchange(other.client_socket_, -1);
  server_socket_ = std::exchange(other.server_socket_, -1);
  preparation_ = std::move(other.preparation_);
  server_ = std::move(other.server_);
  s3_prefix_ = std::move(other.s3_prefix_);
  return *this;
}

int DirectRestoreTransactionDescriptor::ReadRange(
    void* context, const char* image, uint64_t offset, void* buffer, size_t length)
{
  auto* self = static_cast<DirectRestoreTransactionDescriptor*>(context);
  if (!self->s3_prefix_.empty())
    return ReadS3Range(self->s3_prefix_, image, offset, buffer, length);
  const int fd = open((self->staging_directory_ / image).c_str(), O_RDONLY | O_CLOEXEC);
  if (fd < 0) return -errno;
  const ssize_t read_bytes = pread(fd, buffer, length, static_cast<off_t>(offset));
  const int result = read_bytes == static_cast<ssize_t>(length) ? 0 : read_bytes < 0 ? -errno : -EIO;
  close(fd);
  return result;
}

int DirectRestoreTransactionDescriptor::OpenReadyImage(void* context, const char* image, int flags)
{
  auto* self = static_cast<DirectRestoreTransactionDescriptor*>(context);
  const int fd = open((self->staging_directory_ / image).c_str(), flags | O_CLOEXEC);
  return fd < 0 ? -errno : fd;
}

void DirectRestoreTransactionDescriptor::Start(std::function<void(const Path&)> stage)
{
  preparation_ = std::async(std::launch::async, [this, stage = std::move(stage)] {
    stage(staging_directory_);
    if (S3RangeReaderEnabled()) {
      const char* prefix = std::getenv("PAGEBROKER_S3_PREFIX");
      if (!prefix || std::string_view(prefix).rfind("s3://", 0) != 0)
        throw std::runtime_error("PAGEBROKER_S3_PREFIX must be an S3 URI");
      s3_prefix_ = prefix;
      while (s3_prefix_.size() > 5 && s3_prefix_.back() == '/') s3_prefix_.pop_back();
    }
    if (const int result = criu_provider_plan_load(
            (staging_directory_ / "criu-provider.plan").c_str(), &plan_); result != 0)
      throw std::runtime_error("direct restore plan load failed: " + std::to_string(result));
    const criu_provider_source_ops ops{ReadRange, OpenReadyImage};
    if (const int result = criu_provider_session_create(plan_, &ops, this, &session_); result != 0)
      throw std::runtime_error("direct restore provider session create failed: " + std::to_string(result));
    if (const int result = criu_provider_session_prepare(session_); result != 0)
      throw std::runtime_error("direct restore provider preparation failed: " + std::to_string(result));
    int sockets[2];
    if (socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, sockets) < 0)
      throw std::runtime_error("create direct restore provider socket failed");
    client_socket_ = sockets[0];
    server_socket_ = sockets[1];
  });
}

void DirectRestoreTransactionDescriptor::Wait()
{
	if (preparation_.valid()) preparation_.get();
}

const Path& DirectRestoreTransactionDescriptor::staging_directory() const { return staging_directory_; }

uintmax_t DirectRestoreTransactionDescriptor::TakeReservedStagingBytes()
{
  return std::exchange(reserved_staging_bytes_, 0);
}

int DirectRestoreTransactionDescriptor::TakeClientSocket()
{
  if (client_socket_ < 0) return -1;
  if (!server_.joinable()) {
    const int socket = server_socket_;
    server_ = std::thread([this, socket] {
      criu_provider_session_serve(session_, socket);
    });
  }
  return std::exchange(client_socket_, -1);
}

void DirectRestoreTransactionDescriptor::Close()
{
	if (preparation_.valid()) {
		try { preparation_.get(); } catch (...) {}
	}
  if (client_socket_ >= 0) close(client_socket_);
  client_socket_ = -1;
  if (server_socket_ >= 0) shutdown(server_socket_, SHUT_RDWR);
  if (server_.joinable()) server_.join();
  if (server_socket_ >= 0) close(server_socket_);
  server_socket_ = -1;
  criu_provider_session_destroy(session_);
  session_ = nullptr;
  criu_provider_plan_destroy(plan_);
  plan_ = nullptr;
}
}  // namespace snapshot::pagebroker
