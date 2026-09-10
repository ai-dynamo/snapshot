// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_transfer.hpp"

#include <sys/wait.h>
#include <unistd.h>

#include <cstdlib>
#include <stdexcept>
#include <string>
#include <string_view>

#include "s3_range_reader.hpp"

namespace snapshot::pagebroker {
namespace {
const char* Prefix()
{
  const char* prefix = std::getenv("PAGEBROKER_S3_PREFIX");
  return prefix && std::string_view(prefix).rfind("s3://", 0) == 0 ? prefix : nullptr;
}
}

bool S3TransferEnabled()
{
  return Prefix() && S3RangeReaderEnabled();
}

void PublishToS3(const Path& source)
{
  const char* prefix = Prefix();
  if (!prefix) throw std::runtime_error("PAGEBROKER_S3_PREFIX must be an S3 URI");
  const std::string source_path = source.string() + "/";
  std::string destination(prefix);
  if (destination.back() != '/') destination += '/';
  const pid_t child = fork();
  if (child < 0) throw std::runtime_error("fork s5cmd");
  if (child == 0) {
    execlp("s5cmd", "s5cmd", "sync", source_path.c_str(), destination.c_str(), nullptr);
    _exit(127);
  }
  int status = 0;
  if (waitpid(child, &status, 0) != child || !WIFEXITED(status) || WEXITSTATUS(status) != 0)
    throw std::runtime_error("s5cmd checkpoint upload failed");
}

void StageLocalImagesFromS3(const Path& destination)
{
  const char* prefix = Prefix();
  if (!prefix) throw std::runtime_error("PAGEBROKER_S3_PREFIX must be an S3 URI");
  std::string source(prefix);
  while (source.size() > 5 && source.back() == '/') source.pop_back();
  source += "/*";
  std::string destination_path = destination.string() + "/";
  const pid_t child = fork();
  if (child < 0) throw std::runtime_error("fork s5cmd");
  if (child == 0) {
    execlp("s5cmd", "s5cmd", "sync", "--exclude", "*pages-*.img",
           "--exclude", "*criu-provider_plan.json", source.c_str(),
           destination_path.c_str(), nullptr);
    _exit(127);
  }
  int status = 0;
  if (waitpid(child, &status, 0) != child || !WIFEXITED(status) ||
      WEXITSTATUS(status) != 0)
    throw std::runtime_error("s5cmd checkpoint metadata download failed");
}
}  // namespace snapshot::pagebroker
