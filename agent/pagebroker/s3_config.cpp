// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_config.hpp"

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <stdexcept>
#include <string>
#include <set>
#include <sstream>

#include "transfer/checkpoint_inventory.hpp"
#include "transfer/s3/checkpoint_index.hpp"
#include "utils/config.hpp"
#include "utils/json.hpp"
#include <string_view>

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
constexpr char kUploadActiveFilesKey[] = "uploadActiveFiles";
constexpr char kRequestSecondsKey[] = "requestSeconds";
constexpr char kUploadRequestRetriesKey[] = "uploadRequestRetries";
constexpr char kUploadConnectSecondsKey[] = "uploadConnectSeconds";
constexpr char kCredentialsFileEnv[] = "AWS_SHARED_CREDENTIALS_FILE";
constexpr char kDefaultProfile[] = "default";
constexpr char kAccessKey[] = "aws_access_key_id";
constexpr char kSecretKey[] = "aws_secret_access_key";
constexpr char kSessionToken[] = "aws_session_token";
constexpr std::size_t kMaxCredentialsBytes = 64 * 1024;

std::string_view
Trim(std::string_view text)
{
  const auto first = text.find_first_not_of(" \t\r");
  return first == text.npos ? std::string_view{} : text.substr(first, text.find_last_not_of(" \t\r") - first + 1);
}

S3Connection
ParseCredentials(std::string_view text, S3Connection connection)
{
  if (text.size() > kMaxCredentialsBytes || text.find('\0') != text.npos)
    throw std::invalid_argument("invalid S3 credentials file");
  connection.access_key_id.clear();
  connection.secret_access_key.clear();
  connection.session_token.clear();
  std::istringstream input{std::string(text)};
  std::string line;
  std::string profile;
  std::set<std::string> seen;
  while (std::getline(input, line)) {
    auto value = Trim(line);
    if (value.empty() || value.front() == '#' || value.front() == ';')
      continue;
    if (value.front() == '[' && value.back() == ']') {
      profile = Trim(value.substr(1, value.size() - 2));
      if (profile.empty())
        throw std::invalid_argument("invalid S3 credentials profile");
      continue;
    }
    const auto equals = value.find('=');
    if (profile.empty() || equals == value.npos || Trim(value.substr(0, equals)).empty())
      throw std::invalid_argument("invalid S3 credentials file");
    if (profile != kDefaultProfile)
      continue;
    const auto key = Trim(value.substr(0, equals));
    std::string* destination = key == kAccessKey ? &connection.access_key_id :
        key == kSecretKey ? &connection.secret_access_key : key == kSessionToken ? &connection.session_token : nullptr;
    if (!destination)
      continue;
    if (!seen.insert(std::string(key)).second)
      throw std::invalid_argument("duplicate S3 credential key");
    *destination = Trim(value.substr(equals + 1));
  }
  if (connection.access_key_id.empty() || connection.secret_access_key.empty())
    throw std::invalid_argument("S3 credentials file requires a default access key and secret key");
  connection.Validate();
  return connection;
}

constexpr char kPathAddressing[] = "path";
constexpr char kVirtualAddressing[] = "virtual";

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

using Json = nlohmann::json;
constexpr std::size_t kMaxConfigBytes = 65536;
constexpr auto kMaxTransactionLifetime = std::chrono::hours(2);
constexpr auto kMaxRequestTimeout = std::chrono::seconds(60);

template<typename T>
T
ReadField(const Json& value, const char* key)
{
  try {
    return utils::config::ReadField<T>(value, key);
  }
  catch (const std::invalid_argument&) {
    throw std::invalid_argument(std::string("missing or invalid S3 field: ") + key);
  }
}

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
  const bool allow_http = value.contains(kAllowHttpKey) && ReadField<bool>(value, kAllowHttpKey);
  if (!allow_http && !connection.endpoint.starts_with(kHttpsEndpointPrefix))
    return false;
  return true;
}

S3Connection
ParseConnection(const Json& value)
{
  S3Connection connection;
  connection.endpoint = ReadField<std::string>(value, kEndpointKey);
  connection.region = ReadField<std::string>(value, kRegionKey);

  const auto addressing = value.contains(kAddressingKey) ? ReadField<std::string>(value, kAddressingKey) : kVirtualAddressing;
  if (addressing != kPathAddressing && addressing != kVirtualAddressing)
    throw std::invalid_argument("invalid S3 addressing mode");
  connection.use_virtual_addressing = addressing == kVirtualAddressing;
  connection.ca_file = value.contains(kCaFileKey) ? ReadField<std::string>(value, kCaFileKey) : std::string();

  if (!HasValidConnectionSettings(connection, value))
    throw std::invalid_argument("invalid S3 endpoint or region");
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
        kUploadBufferBytesKey, kUploadActiveFilesKey, kRequestSecondsKey, kUploadRequestRetriesKey, kUploadConnectSecondsKey};
    if (!limits.is_object())
      throw std::invalid_argument("S3 limits must be an object");
    for (const auto& [key, ignored] : limits.items()) {
      if (!allowed.contains(key))
        throw std::invalid_argument("unknown S3 limit");
    }
    auto read_limit = [&](const char* key, std::uint64_t fallback, std::uint64_t maximum, std::uint64_t minimum = 1) {
      try {
        return limits.contains(key) ? utils::Unsigned(limits.at(key), maximum, minimum) : fallback;
      }
      catch (const std::invalid_argument&) {
        throw std::invalid_argument(std::string("invalid S3 limit: ") + key);
      }
    };
    config.transaction_lifetime = std::chrono::seconds(read_limit(kTransactionSecondsKey, config.transaction_lifetime.count(),
        std::chrono::duration_cast<std::chrono::seconds>(kMaxTransactionLifetime).count()));
    config.staging_bytes = read_limit(kStagingBytesKey, config.staging_bytes, 1ULL << 40);
    config.active_transactions = read_limit(kActiveTransactionsKey, config.active_transactions, 1024);
    upload.part_size = read_limit(kUploadPartBytesKey, upload.part_size, 1ULL << 30, 8 * 1024 * 1024);
    upload.buffer_budget = read_limit(kUploadBufferBytesKey, upload.buffer_budget, 16ULL << 30, upload.part_size);
    upload.active_files = read_limit(kUploadActiveFilesKey, upload.active_files, 64);
    upload.request_timeout = std::chrono::seconds(read_limit(kRequestSecondsKey,
        std::chrono::duration_cast<std::chrono::seconds>(upload.request_timeout).count(), kMaxRequestTimeout.count()));
    upload.request_retries = read_limit(kUploadRequestRetriesKey, upload.request_retries, 10, 0);
    const auto request_seconds = std::chrono::duration_cast<std::chrono::seconds>(upload.request_timeout).count();
    const auto connect_seconds = std::chrono::duration_cast<std::chrono::seconds>(upload.connect_timeout).count();
    upload.connect_timeout = std::chrono::seconds(read_limit(kUploadConnectSecondsKey, std::min(connect_seconds, request_seconds), request_seconds));
  }
  config.transfer.restore_timeout = std::min(config.transfer.restore_timeout,
      std::chrono::duration_cast<std::chrono::milliseconds>(config.transaction_lifetime));
  upload.Validate();
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


S3Config
ParseS3Config(std::string_view text)
{
  Json value;
  try {
    value = utils::ParseJson(text, kMaxConfigBytes);
  }
  catch (const Json::exception&) {
    // JSON parser diagnostics can contain supplied configuration values.
    throw std::invalid_argument("invalid S3 configuration JSON");
  }
  ValidateConfigFields(value);

  S3Config config;
  config.store_id = ReadField<std::string>(value, kStoreIdKey);
  if (!IsValidStoreID(config.store_id))
    throw std::invalid_argument("S3 storeId must be a nonempty bounded identifier");
  config.transfer.connection = ParseConnection(value);
  config.bucket = ReadField<std::string>(value, kBucketKey);
  config.prefix = value.contains(kPrefixKey) ? ReadField<std::string>(value, kPrefixKey) : std::string();
  ValidateStorageLocation(config.bucket, config.prefix);
  ParseTransferLimits(value, config);
  return config;
}

S3Connection
LoadS3Credentials(S3Connection connection)
{
  connection.Validate();
  if (connection.ca_file.empty())
    connection.ca_file = Environment(kAwsCaBundleEnv);
  connection.Validate();
  const auto path = Environment(kCredentialsFileEnv);
  if (path.empty())
    throw std::invalid_argument("AWS_SHARED_CREDENTIALS_FILE is required for S3");
  try {
    return ParseCredentials(utils::config::ReadBoundedConfigFile(path, kMaxCredentialsBytes), std::move(connection));
  }
  catch (const std::ios_base::failure&) {
    throw std::invalid_argument("cannot read S3 credentials file");
  }
}

S3Config
ReadS3Config(const std::filesystem::path& path)
{
  try {
    return ParseS3Config(utils::config::ReadBoundedConfigFile(path, kMaxConfigBytes));
  }
  catch (const std::ios_base::failure&) {
    throw std::invalid_argument("cannot read S3 configuration");
  }
}
}  // namespace snapshot::pagebroker
