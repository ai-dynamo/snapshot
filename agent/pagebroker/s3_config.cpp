// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "s3_config.hpp"

#include <chrono>
#include <cstdlib>
#include <stdexcept>
#include <string>

namespace snapshot::pagebroker {
namespace {
bool
HasValidAddressingSetting(const std::string& addressing)
{
  if (!std::getenv(kS3VirtualAddressingEnv))
    return true;
  return addressing == kS3VirtualAddressingDisabled || addressing == kS3VirtualAddressingEnabled;
}

std::string
Environment(const char* name)
{
  const auto* value = std::getenv(name);
  return value ? value : "";
}

}  // namespace

void
ValidateS3TransferConfiguration(const S3TransferOptions& options)
{
  if (options.restore_timeout <= std::chrono::milliseconds::zero() || options.restore_timeout > std::chrono::hours(24))
    throw std::invalid_argument("S3 restore timeout must be positive and at most 24 hours");

  const auto addressing = Environment(kS3VirtualAddressingEnv);
  if (!HasValidAddressingSetting(addressing))
    throw std::invalid_argument("RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING must be 0 or 1");
  if (options.connection.use_virtual_addressing != (addressing != kS3VirtualAddressingDisabled))
    throw std::invalid_argument("S3 addressing must match RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING");
  if (options.connection.ca_file != Environment(kAwsCaBundleEnv))
    throw std::invalid_argument("S3 CA file must match AWS_CA_BUNDLE for both upload and restore");
}

}  // namespace snapshot::pagebroker
