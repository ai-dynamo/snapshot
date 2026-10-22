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
constexpr char kReadChunkBytesKey[] = "readChunkBytes";
constexpr char kProcessGroupSizeKey[] = "processGroupSize";
constexpr char kS3ReaderKey[] = "s3Reader";
constexpr char kConcurrencyKey[] = "concurrency";
constexpr char kTargetGbpsKey[] = "targetGbps";
constexpr char kMaxConnectionsKey[] = "maxConnections";
constexpr char kMaxInflightMiBKey[] = "maxInflightMiB";
constexpr char kMaxRetriesKey[] = "maxRetries";
constexpr char kRetryWindowSecondsKey[] = "retryWindowSeconds";
constexpr char kLowSpeedTimeoutMsKey[] = "lowSpeedTimeoutMs";
constexpr char kLowSpeedBytesPerSecondKey[] = "lowSpeedBytesPerSecond";
constexpr char kLoggingKey[] = "logging";
constexpr char kLevelKey[] = "level";
constexpr char kToStderrKey[] = "toStderr";

constexpr char kReadChunkBytesEnv[] = "RUNAI_STREAMER_CHUNK_BYTESIZE";
constexpr char kProcessGroupSizeEnv[] = "RUNAI_STREAMER_PROCESS_GROUP_SIZE";
constexpr char kS3ConcurrencyEnv[] = "RUNAI_STREAMER_OBJ_CONCURRENCY";
constexpr char kS3TargetGbpsEnv[] = "RUNAI_STREAMER_S3_TARGET_GBPS";
constexpr char kS3MaxConnectionsEnv[] = "RUNAI_STREAMER_S3_MAX_CONNECTIONS";
constexpr char kS3MaxInflightMiBEnv[] = "RUNAI_STREAMER_S3_MAX_INFLIGHT_MIB";
constexpr char kS3MaxRetriesEnv[] = "RUNAI_STREAMER_S3_MAX_RETRIES";
constexpr char kS3RetryWindowSecondsEnv[] = "RUNAI_STREAMER_S3_TIMEOUT";
constexpr char kS3LowSpeedTimeoutMsEnv[] = "RUNAI_STREAMER_S3_REQUEST_TIMEOUT_MS";
constexpr char kS3LowSpeedBytesPerSecondEnv[] = "RUNAI_STREAMER_S3_LOW_SPEED_LIMIT";
constexpr char kLogLevelEnv[] = "RUNAI_STREAMER_LOG_LEVEL";
constexpr char kLogToStderrEnv[] = "RUNAI_STREAMER_LOG_TO_STDERR";
constexpr char kLegacyConcurrencyEnv[] = "RUNAI_STREAMER_CONCURRENCY";
constexpr char kAwsEc2MetadataDisabledEnv[] = "AWS_EC2_METADATA_DISABLED";

constexpr char kNativeBooleanTrue[] = "1";
constexpr char kNativeBooleanFalse[] = "0";
constexpr char kDisableEc2Metadata[] = "true";

constexpr char kSpamLogLevel[] = "SPAM";
constexpr char kDebugLogLevel[] = "DEBUG";
constexpr char kInfoLogLevel[] = "INFO";
constexpr char kWarningLogLevel[] = "WARNING";
constexpr char kErrorLogLevel[] = "ERROR";

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
    {"", kReadChunkBytesKey, kReadChunkBytesEnv, &ModelStreamerOptions::read_chunk_bytes, 2 * kMiB, 1024 * kMiB},
    {"", kProcessGroupSizeKey, kProcessGroupSizeEnv, &ModelStreamerOptions::process_group_size, 1, kMaxCount},
    {kS3ReaderKey, kConcurrencyKey, kS3ConcurrencyEnv, &ModelStreamerOptions::s3_concurrency, 1, 1024},
    {kS3ReaderKey, kTargetGbpsKey, kS3TargetGbpsEnv, &ModelStreamerOptions::s3_target_gbps, 1, kMaxCount},
    {kS3ReaderKey, kMaxConnectionsKey, kS3MaxConnectionsEnv, &ModelStreamerOptions::s3_max_connections, 1, kMaxCount},
    {kS3ReaderKey, kMaxInflightMiBKey, kS3MaxInflightMiBEnv, &ModelStreamerOptions::s3_max_inflight_mib, 1, 1024 * 1024},
    {kS3ReaderKey, kMaxRetriesKey, kS3MaxRetriesEnv, &ModelStreamerOptions::s3_max_retries, 0, 10},
    {kS3ReaderKey, kRetryWindowSecondsKey, kS3RetryWindowSecondsEnv, &ModelStreamerOptions::s3_retry_window_seconds, 0, 86400},
    {kS3ReaderKey, kLowSpeedTimeoutMsKey, kS3LowSpeedTimeoutMsEnv, &ModelStreamerOptions::s3_low_speed_timeout_ms, 1, 86400000},
    {kS3ReaderKey, kLowSpeedBytesPerSecondKey, kS3LowSpeedBytesPerSecondEnv, &ModelStreamerOptions::s3_low_speed_bytes_per_second, 1, kMaxCount},
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
ModelStreamerOptions::Validate() const
{
  for (const auto& setting : kNumbers) {
    const auto& value = this->*setting.member;
    if (!IsWithinBounds(value, setting))
      Invalid(FieldName(setting));
  }
  const std::set<std::string> levels{kSpamLogLevel, kDebugLogLevel, kInfoLogLevel, kWarningLogLevel, kErrorLogLevel};
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
  if (log_level)
    result.emplace(kLogLevelEnv, *log_level);
  if (log_to_stderr)
    result.emplace(kLogToStderrEnv, *log_to_stderr ? kNativeBooleanTrue : kNativeBooleanFalse);
  return result;
}

void
ModelStreamerOptions::ValidateEnvironment() const
{
  for (const auto& [name, expected] : NativeEnvironment()) {
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
  ValidateFields(value, "", {kS3ReaderKey, kLoggingKey});
  if (value.contains(kS3ReaderKey))
    ValidateFields(value.at(kS3ReaderKey), kS3ReaderKey, {});
  if (value.contains(kLoggingKey))
    ValidateFields(value.at(kLoggingKey), kLoggingKey, {kLevelKey, kToStderrKey});

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
  if (value.contains(kLoggingKey)) {
    const auto& logging = value.at(kLoggingKey);
    if (logging.contains(kLevelKey))
      options.log_level = String(logging.at(kLevelKey), "logging.level");
    if (logging.contains(kToStderrKey)) {
      if (!logging.at(kToStderrKey).is_boolean())
        Invalid("logging.toStderr");
      options.log_to_stderr = logging.at(kToStderrKey).get<bool>();
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
  Inherit(options.log_level, kLogLevelEnv);
  if (!options.log_to_stderr) {
    if (const auto* value = std::getenv(kLogToStderrEnv)) {
      const std::string_view flag(value);
      if (flag != kNativeBooleanFalse && flag != kNativeBooleanTrue)
        Invalid("logging.toStderr");
      options.log_to_stderr = flag == kNativeBooleanTrue;
    }
  }
  // Preserve the legacy fallback for S3 concurrency when no explicit setting exists.
  if (const auto* legacy = std::getenv(kLegacyConcurrencyEnv)) {
    if (!options.s3_concurrency)
      options.s3_concurrency = Number(legacy, "s3Reader.concurrency");
  }
  if (storage) {
    const auto& upload = storage->transfer.upload_limits;
    options.s3_max_retries = options.s3_max_retries.value_or(3);
    options.s3_retry_window_seconds = options.s3_retry_window_seconds.value_or(0);
    options.s3_low_speed_timeout_ms = options.s3_low_speed_timeout_ms.value_or(upload.request_timeout.count());
    // Keep native reader defaults independent of CRT upload memory/concurrency.
    options.s3_max_inflight_mib = options.s3_max_inflight_mib.value_or(128);
    options.read_chunk_bytes = options.read_chunk_bytes.value_or(std::min<std::uint64_t>(16 * kMiB, upload.part_size));
    options.s3_max_connections = options.s3_max_connections.value_or(4);
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
    environment[kS3VirtualAddressingEnv] = connection.use_virtual_addressing
        ? kS3VirtualAddressingEnabled : kS3VirtualAddressingDisabled;
    environment[kAwsEc2MetadataDisabledEnv] = kDisableEc2Metadata;
    environment[kAwsCaBundleEnv] = connection.ca_file;
  }
  for (const auto& [name, value] : environment) {
    if (setenv(name.c_str(), value.c_str(), 1) != 0)
      throw std::runtime_error("configure Model Streamer startup environment: " + name);
  }
}
}  // namespace snapshot::pagebroker
