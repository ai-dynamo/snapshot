// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "s3_config.hpp"

#include <chrono>
#include <cstdlib>
#include <stdexcept>
#include <string>
#include <string_view>

namespace snapshot::pagebroker {
namespace {
bool
ParseVirtualAddressingEnvironment()
{
  const auto* value = std::getenv(kS3VirtualAddressingEnv);
  if (!value || std::string_view(value) == kS3VirtualAddressingEnabled)
    return true;
  if (std::string_view(value) == kS3VirtualAddressingDisabled)
    return false;
  throw std::invalid_argument("RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING must be 0 or 1");
}

std::string
Environment(const char* name)
{
  const auto* value = std::getenv(name);
  return value ? value : "";
}

}  // namespace

void
S3TransferOptions::Validate() const
{
  connection.Validate();
  upload_limits.Validate();
  if (restore_timeout <= std::chrono::milliseconds::zero() || restore_timeout > std::chrono::hours(24))
    throw std::invalid_argument("S3 restore timeout must be positive and at most 24 hours");
}

void
ValidateModelStreamerEnvironment(const S3TransferOptions& options)
{
  if (options.connection.use_virtual_addressing != ParseVirtualAddressingEnvironment())
    throw std::invalid_argument("S3 addressing must match RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING");
  if (options.connection.ca_file != Environment(kAwsCaBundleEnv))
    throw std::invalid_argument("S3 CA file must match AWS_CA_BUNDLE for both upload and restore");
}

}  // namespace snapshot::pagebroker
