// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_config.hpp"

#include <algorithm>
#include <charconv>
#include <cstdlib>
#include <fstream>
#include <limits>
#include <set>
#include <stdexcept>
#include <vector>

#include "utils/json.hpp"

namespace snapshot::pagebroker {
namespace {
using Json = nlohmann::json;
constexpr std::size_t kMaxConfigBytes = 65536;
constexpr std::uint64_t kMiB = 1024 * 1024;
constexpr std::uint64_t kMaxCount = std::numeric_limits<unsigned>::max();

struct NumericSetting {
  const char* group;
  const char* field;
  const char* environment;
  std::optional<std::uint64_t> ModelStreamerOptions::*member;
  std::uint64_t minimum;
  std::uint64_t maximum;
};

// One mapping drives parsing, validation and native translation, keeping the
// JSON contract and environment fallback rules identical.
constexpr NumericSetting kNumbers[] = {
    {"filesystem", "chunkBytes", "RUNAI_STREAMER_FS_CHUNK_BYTESIZE", &ModelStreamerOptions::filesystem_chunk_bytes, 1, 1024 * kMiB},
    {"filesystem", "maxEngines", "RUNAI_STREAMER_FS_MAX_ENGINES", &ModelStreamerOptions::filesystem_max_engines, 1, 1024},
    {"", "readChunkBytes", "RUNAI_STREAMER_CHUNK_BYTESIZE", &ModelStreamerOptions::read_chunk_bytes, 2 * kMiB, 1024 * kMiB},
    {"", "processGroupSize", "RUNAI_STREAMER_PROCESS_GROUP_SIZE", &ModelStreamerOptions::process_group_size, 1, kMaxCount},
    {"s3Reader", "concurrency", "RUNAI_STREAMER_OBJ_CONCURRENCY", &ModelStreamerOptions::s3_concurrency, 1, 1024},
    {"s3Reader", "targetGbps", "RUNAI_STREAMER_S3_TARGET_GBPS", &ModelStreamerOptions::s3_target_gbps, 1, kMaxCount},
    {"s3Reader", "maxConnections", "RUNAI_STREAMER_S3_MAX_CONNECTIONS", &ModelStreamerOptions::s3_max_connections, 1, kMaxCount},
    {"s3Reader", "maxInflightMiB", "RUNAI_STREAMER_S3_MAX_INFLIGHT_MIB", &ModelStreamerOptions::s3_max_inflight_mib, 1, 1024 * 1024},
    {"s3Reader", "maxRetries", "RUNAI_STREAMER_S3_MAX_RETRIES", &ModelStreamerOptions::s3_max_retries, 0, 10},
    {"s3Reader", "retryWindowSeconds", "RUNAI_STREAMER_S3_TIMEOUT", &ModelStreamerOptions::s3_retry_window_seconds, 0, 86400},
    {"s3Reader", "lowSpeedTimeoutMs", "RUNAI_STREAMER_S3_REQUEST_TIMEOUT_MS", &ModelStreamerOptions::s3_low_speed_timeout_ms, 1, 86400000},
    {"s3Reader", "lowSpeedBytesPerSecond", "RUNAI_STREAMER_S3_LOW_SPEED_LIMIT", &ModelStreamerOptions::s3_low_speed_bytes_per_second, 1, kMaxCount},
};

std::string
FieldName(const NumericSetting& setting)
{
  return std::string(setting.group) + (*setting.group ? "." : "") + setting.field;
}

[[noreturn]] void
Invalid(const std::string& field)
{
  // Only schema-owned names appear in diagnostics, never supplied values.
  throw std::invalid_argument("invalid Model Streamer configuration: " + field);
}

std::string_view
Trim(std::string_view value)
{
  const auto begin = value.find_first_not_of(" \t");
  if (begin == std::string_view::npos)
    return {};
  return value.substr(begin, value.find_last_not_of(" \t") - begin + 1);
}

std::vector<std::string_view>
Split(std::string_view value)
{
  std::vector<std::string_view> parts;
  for (;;) {
    const auto comma = value.find(',');
    parts.push_back(value.substr(0, comma));
    if (comma == std::string_view::npos)
      return parts;
    value.remove_prefix(comma + 1);
  }
}

std::uint64_t
Number(std::string_view value, const std::string& field)
{
  if (value.empty())
    Invalid(field);
  std::uint64_t result = 0;
  const auto [end, error] = std::from_chars(value.data(), value.data() + value.size(), result);
  if (error != std::errc{} || end != value.data() + value.size())
    Invalid(field);
  return result;
}

void
ValidateQueueDepth(std::string_view value)
{
  const std::string field = "filesystem.queueDepth";
  if (value.empty() || value.size() > 4096)
    Invalid(field);
  const auto entries = Split(value);
  std::set<std::string> types;
  for (std::size_t i = 0; i < entries.size(); ++i) {
    auto count = Trim(entries[i]);
    if (i != 0) {
      const auto equal = count.find('=');
      if (equal == std::string_view::npos)
        Invalid(field);
      std::string type(Trim(count.substr(0, equal)));
      for (char& c : type) {
        if (c >= 'A' && c <= 'Z')
          c += 'a' - 'A';
      }
      if (type.empty() || type.find_first_not_of("abcdefghijklmnopqrstuvwxyz0123456789_.-") != std::string::npos)
        Invalid(field);
      if (!types.insert(type).second)
        Invalid(field);
      count = Trim(count.substr(equal + 1));
    }
    const auto number = Number(count, field);
    if (number == 0 || number > kMaxCount)
      Invalid(field);
  }
}

void
ValidateFields(const Json& value, const std::string& group, std::set<std::string> allowed)
{
  if (!value.is_object())
    Invalid(group.empty() ? "root" : group);
  for (const auto& setting : kNumbers) {
    if (group == setting.group)
      allowed.insert(setting.field);
  }
  for (const auto& [key, ignored] : value.items()) {
    if (!allowed.contains(key))
      Invalid(group.empty() ? "unknown field" : group + ".unknown field");
  }
}

std::string
String(const Json& value, const char* field)
{
  if (!value.is_string())
    Invalid(field);
  return value.get<std::string>();
}

void
Inherit(std::optional<std::string>& target, const char* name)
{
  if (!target) {
    if (const auto* value = std::getenv(name))
      target = value;
  }
}

bool
IsWithinBounds(const std::optional<std::uint64_t>& value, const NumericSetting& setting)
{
  if (!value)
    return true;
  return *value >= setting.minimum && *value <= setting.maximum;
}
}  // namespace

void
ValidateFilesystemStrategy(std::string_view candidates)
{
  if (candidates.size() > 256)
    Invalid("filesystem.strategies");
  const std::set<std::string_view> allowed{"io_uring_direct", "io_uring_buffered", "libaio_direct", "sync_buffered"};
  std::set<std::string_view> seen;
  for (const auto strategy : Split(candidates)) {
    if (!allowed.contains(strategy) || !seen.insert(strategy).second)
      Invalid("filesystem.strategies");
  }
}

void
ModelStreamerOptions::Validate() const
{
  for (const auto& setting : kNumbers) {
    const auto& value = this->*setting.member;
    if (!IsWithinBounds(value, setting))
      Invalid(FieldName(setting));
  }
  if (filesystem_strategy)
    ValidateFilesystemStrategy(*filesystem_strategy);
  if (filesystem_queue_depth)
    ValidateQueueDepth(*filesystem_queue_depth);
  const std::set<std::string> levels{"SPAM", "DEBUG", "INFO", "WARNING", "ERROR"};
  if (log_level && !levels.contains(*log_level))
    Invalid("logging.level");
}

std::map<std::string, std::string>
ModelStreamerOptions::NativeEnvironment() const
{
  Validate();
  std::map<std::string, std::string> result;
  for (const auto& setting : kNumbers) {
    if (const auto& value = this->*setting.member; value)
      result.emplace(setting.environment, std::to_string(*value));
  }
  if (filesystem_strategy)
    result.emplace("RUNAI_STREAMER_FS_STRATEGY", *filesystem_strategy);
  if (filesystem_queue_depth)
    result.emplace("RUNAI_STREAMER_FS_QUEUE_DEPTH", *filesystem_queue_depth);
  if (log_level)
    result.emplace("RUNAI_STREAMER_LOG_LEVEL", *log_level);
  if (log_to_stderr)
    result.emplace("RUNAI_STREAMER_LOG_TO_STDERR", *log_to_stderr ? "1" : "0");
  return result;
}

void
ModelStreamerOptions::ValidateEnvironment() const
{
  for (const auto& [name, expected] : NativeEnvironment()) {
    if (name == "RUNAI_STREAMER_FS_STRATEGY")
      continue;  // This setting also has a session-scoped public setter.
    const auto* actual = std::getenv(name.c_str());
    if (!actual || expected != actual)
      throw std::invalid_argument("Model Streamer startup environment does not match " + name);
  }
}

ModelStreamerOptions
ParseModelStreamerConfig(std::string_view text)
{
  Json value;
  try {
    value = utils::ParseJson(text, kMaxConfigBytes);
  }
  catch (const std::exception&) {
    Invalid("JSON");
  }
  ValidateFields(value, "", {"filesystem", "s3Reader", "logging"});
  if (value.contains("filesystem"))
    ValidateFields(value.at("filesystem"), "filesystem", {"strategies", "queueDepth"});
  if (value.contains("s3Reader"))
    ValidateFields(value.at("s3Reader"), "s3Reader", {});
  if (value.contains("logging"))
    ValidateFields(value.at("logging"), "logging", {"level", "toStderr"});

  ModelStreamerOptions options;
  for (const auto& setting : kNumbers) {
    const Json* group = &value;
    if (*setting.group) {
      if (!value.contains(setting.group))
        continue;
      group = &value.at(setting.group);
    }
    if (group->contains(setting.field)) {
      try {
        options.*setting.member = utils::Unsigned(group->at(setting.field), setting.maximum, setting.minimum);
      }
      catch (const std::exception&) {
        Invalid(FieldName(setting));
      }
    }
  }
  if (value.contains("filesystem")) {
    const auto& fs = value.at("filesystem");
    if (fs.contains("strategies")) {
      if (!fs.at("strategies").is_array() || fs.at("strategies").empty())
        Invalid("filesystem.strategies");
      std::string candidates;
      for (const auto& strategy : fs.at("strategies")) {
        const auto name = String(strategy, "filesystem.strategies");
        if (name.find(',') != std::string::npos)
          Invalid("filesystem.strategies");
        if (!candidates.empty())
          candidates += ',';
        candidates += name;
        // Reject empty entries before joining, so they cannot disappear.
        ValidateFilesystemStrategy(candidates);
      }
      options.filesystem_strategy = std::move(candidates);
    }
    if (fs.contains("queueDepth"))
      options.filesystem_queue_depth = String(fs.at("queueDepth"), "filesystem.queueDepth");
  }
  if (value.contains("logging")) {
    const auto& logging = value.at("logging");
    if (logging.contains("level"))
      options.log_level = String(logging.at("level"), "logging.level");
    if (logging.contains("toStderr")) {
      if (!logging.at("toStderr").is_boolean())
        Invalid("logging.toStderr");
      options.log_to_stderr = logging.at("toStderr").get<bool>();
    }
  }
  options.Validate();
  return options;
}

ModelStreamerOptions
ReadModelStreamerConfig(const std::filesystem::path& path)
{
  std::ifstream input(path, std::ios::binary);
  if (!input)
    Invalid("unreadable file");
  std::string text(kMaxConfigBytes + 1, '\0');
  input.read(text.data(), text.size());
  if (input.bad())
    Invalid("unreadable file");
  text.resize(input.gcount());
  return ParseModelStreamerConfig(text);
}

ModelStreamerOptions
ResolveModelStreamerOptions(ModelStreamerOptions options, const S3Config* storage)
{
  for (const auto& setting : kNumbers) {
    if (!(options.*setting.member)) {
      if (const auto* value = std::getenv(setting.environment))
        options.*setting.member = Number(value, FieldName(setting));
    }
  }
  Inherit(options.filesystem_strategy, "RUNAI_STREAMER_FS_STRATEGY");
  Inherit(options.filesystem_queue_depth, "RUNAI_STREAMER_FS_QUEUE_DEPTH");
  Inherit(options.log_level, "RUNAI_STREAMER_LOG_LEVEL");
  if (!options.log_to_stderr) {
    if (const auto* value = std::getenv("RUNAI_STREAMER_LOG_TO_STDERR")) {
      const std::string_view flag(value);
      if (flag != "0" && flag != "1")
        Invalid("logging.toStderr");
      options.log_to_stderr = flag == "1";
    }
  }
  // The legacy variable supplies BOTH pools only when their specific setting
  // is absent. Preserve the library's distinct async/sync defaults when unset.
  if (const auto* legacy = std::getenv("RUNAI_STREAMER_CONCURRENCY")) {
    if (!options.filesystem_queue_depth)
      options.filesystem_queue_depth = legacy;
    if (!options.s3_concurrency)
      options.s3_concurrency = Number(legacy, "s3Reader.concurrency");
  }
  if (storage) {
    const auto& upload = storage->transfer.upload_limits;
    options.s3_max_retries = options.s3_max_retries.value_or(3);
    options.s3_retry_window_seconds = options.s3_retry_window_seconds.value_or(0);
    options.s3_low_speed_timeout_ms = options.s3_low_speed_timeout_ms.value_or(upload.request_timeout.count());
    options.s3_max_inflight_mib = options.s3_max_inflight_mib.value_or(upload.buffer_budget / kMiB);
    options.read_chunk_bytes = options.read_chunk_bytes.value_or(std::min<std::uint64_t>(16 * kMiB, upload.part_size));
    options.s3_max_connections = options.s3_max_connections.value_or(upload.workers);
  }
  options.Validate();
  return options;
}

void
ConfigureModelStreamerEnvironment(const ModelStreamerOptions& options, const S3Config* storage)
{
  auto environment = options.NativeEnvironment();
  if (storage) {
    const auto& connection = storage->transfer.connection;
    environment["RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING"] = connection.use_virtual_addressing ? "1" : "0";
    environment["AWS_EC2_METADATA_DISABLED"] = "true";
    environment["AWS_CA_BUNDLE"] = connection.ca_file;
  }
  for (const auto& [name, value] : environment) {
    if (setenv(name.c_str(), value.c_str(), 1) != 0)
      throw std::runtime_error("configure Model Streamer startup environment: " + name);
  }
}
}  // namespace snapshot::pagebroker
