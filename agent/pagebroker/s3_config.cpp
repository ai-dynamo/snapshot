// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_config.hpp"

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <stdexcept>
#include <string>
#include <fstream>
#include <set>

#include "transfer/checkpoint_inventory.hpp"
#include "transfer/s3/checkpoint_index.hpp"
#include "utils/json.hpp"

namespace snapshot::pagebroker {
namespace {
constexpr char kStoreIdKey[] = "storeId";
constexpr char kEndpointKey[] = "endpoint";
constexpr char kRegionKey[] = "region";
constexpr char kBucketKey[] = "bucket";
constexpr char kPrefixKey[] = "prefix";
constexpr char kAddressingKey[] = "addressing";
constexpr char kCaFileKey[] = "caFile";
constexpr char kAllowHttpKey[] = "allowHttp";
constexpr char kLimitsKey[] = "limits";
constexpr char kTransactionSecondsKey[] = "transactionSeconds";
constexpr char kStagingBytesKey[] = "stagingBytes";
constexpr char kActiveTransactionsKey[] = "activeTransactions";
constexpr char kUploadPartBytesKey[] = "uploadPartBytes";
constexpr char kUploadBufferBytesKey[] = "uploadBufferBytes";
constexpr char kUploadTargetGbpsKey[] = "uploadTargetGbps";
constexpr char kUploadActiveFilesKey[] = "uploadActiveFiles";
constexpr char kRequestSecondsKey[] = "requestSeconds";
constexpr char kUploadRequestRetriesKey[] = "uploadRequestRetries";
constexpr char kUploadConnectSecondsKey[] = "uploadConnectSeconds";
constexpr char kPathAddressing[] = "path";
constexpr char kVirtualAddressing[] = "virtual";

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

using Json = nlohmann::json;
constexpr std::size_t kMaxConfigBytes = 65536;

void
ValidateConfigFields(const Json& value)
{
  const std::set<std::string> fields{kStoreIdKey, kEndpointKey, kRegionKey, kBucketKey, kPrefixKey, kAddressingKey, kCaFileKey, kAllowHttpKey, kLimitsKey};
  if (!value.is_object())
    throw std::invalid_argument("S3 configuration must be an object");
  for (const auto& [key, ignored] : value.items()) {
    if (!fields.contains(key))
      throw std::invalid_argument("unknown S3 configuration field");
  }
}

bool
HasValidConnectionSettings(const S3Connection& connection, const Json& value)
{
  if (connection.region.empty() || connection.endpoint.empty())
    return false;
  if (!value.value(kAllowHttpKey, false) && !connection.endpoint.starts_with(kHttpsEndpointPrefix))
    return false;
  return connection.ca_file.empty() || std::filesystem::path(connection.ca_file).is_absolute();
}

S3Connection
ParseConnection(const Json& value)
{
  S3Connection connection;
  connection.endpoint = value.at(kEndpointKey).get<std::string>();
  connection.region = value.at(kRegionKey).get<std::string>();

  const auto addressing = value.value(kAddressingKey, std::string(kVirtualAddressing));
  if (addressing != kPathAddressing && addressing != kVirtualAddressing)
    throw std::invalid_argument("invalid S3 addressing mode");
  connection.use_virtual_addressing = addressing == kVirtualAddressing;
  connection.ca_file = value.value(kCaFileKey, std::string());

  if (!HasValidConnectionSettings(connection, value))
    throw std::invalid_argument("invalid S3 endpoint, region, or CA path");
  connection.Validate();
  return connection;
}

bool
IsValidStoragePrefix(const std::string& prefix)
{
  const bool invalid_characters = std::any_of(prefix.begin(), prefix.end(), [](unsigned char c) {
    return c < 33 || c > 126 || c == '\\';
  });
  if (prefix.size() > 384 || invalid_characters)
    return false;
  return prefix.empty() || IsSafeRelativePath(prefix);
}

bool
IsValidStoreID(const std::string& id)
{
  if (id.empty() || id.size() > 256)
    return false;
  if (id.find_first_of("\r\n\t") != std::string::npos)
    return false;
  return id.find('\0') == std::string::npos;
}

void
ValidateStorageLocation(const std::string& bucket, const std::string& prefix)
{
  if (!IsValidStoragePrefix(prefix))
    throw std::invalid_argument("S3 prefix must be a canonical relative key prefix");
  S3ObjectLocation{bucket, kCheckpointIndexName}.Validate();
}

void
ParseTransferLimits(const Json& value, S3Config& config)
{
  auto& upload = config.transfer.upload_limits;
  if (value.contains(kLimitsKey)) {
    const auto& limits = value.at(kLimitsKey);
    const std::set<std::string> allowed{kTransactionSecondsKey, kStagingBytesKey, kActiveTransactionsKey, kUploadPartBytesKey,
        kUploadBufferBytesKey, kUploadTargetGbpsKey, kUploadActiveFilesKey, kRequestSecondsKey, kUploadRequestRetriesKey, kUploadConnectSecondsKey};
    if (!limits.is_object())
      throw std::invalid_argument("S3 limits must be an object");
    for (const auto& [key, ignored] : limits.items()) {
      if (!allowed.contains(key))
        throw std::invalid_argument("unknown S3 limit");
    }
    auto read_limit = [&](const char* key, std::uint64_t fallback, std::uint64_t maximum, std::uint64_t minimum = 1) {
      return limits.contains(key) ? utils::Unsigned(limits.at(key), maximum, minimum) : fallback;
    };
    config.transaction_lifetime = std::chrono::seconds(read_limit(kTransactionSecondsKey, 7200, 7200));
    config.staging_bytes = read_limit(kStagingBytesKey, config.staging_bytes, 1ULL << 40);
    config.active_transactions = read_limit(kActiveTransactionsKey, config.active_transactions, 1024);
    upload.part_size = read_limit(kUploadPartBytesKey, upload.part_size, 1ULL << 30, 8 * 1024 * 1024);
    upload.buffer_budget = read_limit(kUploadBufferBytesKey, upload.buffer_budget, 16ULL << 30, std::max<std::uint64_t>(1ULL << 30, 2 * upload.part_size));
    upload.target_gbps = read_limit(kUploadTargetGbpsKey, upload.target_gbps, 1000);
    upload.active_files = read_limit(kUploadActiveFilesKey, upload.active_files, 64);
    upload.request_timeout = std::chrono::seconds(read_limit(kRequestSecondsKey, 60, 60));
    upload.request_retries = read_limit(kUploadRequestRetriesKey, upload.request_retries, 10, 0);
    const auto request_seconds = std::chrono::duration_cast<std::chrono::seconds>(upload.request_timeout).count();
    upload.connect_timeout = std::chrono::seconds(read_limit(kUploadConnectSecondsKey, std::min<std::uint64_t>(5, request_seconds), request_seconds));
  }
  upload.operation_timeout = config.transaction_lifetime;
  config.transfer.restore_timeout = std::min(config.transfer.restore_timeout,
      std::chrono::duration_cast<std::chrono::milliseconds>(config.transaction_lifetime));
  upload.Validate();
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


S3Config
ParseS3Config(std::string_view text)
{
  const auto value = utils::ParseJson(text, kMaxConfigBytes);
  ValidateConfigFields(value);

  S3Config config;
  config.store_id = value.at(kStoreIdKey).get<std::string>();
  if (!IsValidStoreID(config.store_id))
    throw std::invalid_argument("S3 storeId must be a nonempty bounded identifier");
  config.transfer.connection = ParseConnection(value);
  config.bucket = value.at(kBucketKey).get<std::string>();
  config.prefix = value.value(kPrefixKey, std::string());
  ValidateStorageLocation(config.bucket, config.prefix);
  ParseTransferLimits(value, config);
  return config;
}

S3Config
ReadS3Config(const std::filesystem::path& path)
{
  std::ifstream input(path, std::ios::binary);
  if (!input)
    throw std::invalid_argument("cannot read S3 configuration");
  std::string text(kMaxConfigBytes + 1, '\0');
  input.read(text.data(), text.size());
  if (input.bad())
    throw std::invalid_argument("cannot read S3 configuration");
  text.resize(input.gcount());
  return ParseS3Config(text);
}
}  // namespace snapshot::pagebroker
