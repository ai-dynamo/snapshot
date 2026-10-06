// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_config.hpp"

#include <algorithm>
#include <fstream>
#include <set>

#include "transfer/restore_plan.hpp"
#include "utils/json.hpp"

namespace snapshot::pagebroker {
namespace {
using Json = nlohmann::json;
constexpr std::size_t kMaxConfigBytes = 65536;

void
ValidateConfigFields(const Json& value)
{
  const std::set<std::string> fields{"storeId", "endpoint", "region", "bucket", "prefix", "addressing", "caFile", "allowHttp", "limits"};
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
  if (!value.value("allowHttp", false) && !connection.endpoint.starts_with("https://"))
    return false;
  return connection.ca_file.empty() || std::filesystem::path(connection.ca_file).is_absolute();
}

S3Connection
ParseConnection(const Json& value)
{
  S3Connection connection;
  connection.endpoint = value.at("endpoint").get<std::string>();
  connection.region = value.at("region").get<std::string>();

  const auto addressing = value.value("addressing", std::string("virtual"));
  if (addressing != "path" && addressing != "virtual")
    throw std::invalid_argument("invalid S3 addressing mode");
  connection.use_virtual_addressing = addressing == "virtual";
  connection.ca_file = value.value("caFile", std::string());

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
  S3ObjectLocation{bucket, "index.json"}.Validate();
}

void
ParseTransferLimits(const Json& value, S3Config& config)
{
  auto& upload = config.transfer.upload_limits;
  if (value.contains("limits")) {
    const auto& limits = value.at("limits");
    const std::set<std::string> allowed{"transactionSeconds", "stagingBytes", "activeTransactions", "uploadPartBytes",
        "uploadBufferBytes", "uploadWorkers", "uploadActiveFiles", "requestSeconds", "uploadRequestRetries", "uploadConnectSeconds"};
    if (!limits.is_object())
      throw std::invalid_argument("S3 limits must be an object");
    for (const auto& [key, ignored] : limits.items()) {
      if (!allowed.contains(key))
        throw std::invalid_argument("unknown S3 limit");
    }
    auto read_limit = [&](const char* key, std::uint64_t fallback, std::uint64_t maximum, std::uint64_t minimum = 1) {
      return limits.contains(key) ? utils::Unsigned(limits.at(key), maximum, minimum) : fallback;
    };
    config.transaction_lifetime = std::chrono::seconds(read_limit("transactionSeconds", 7200, 7200));
    config.staging_bytes = read_limit("stagingBytes", config.staging_bytes, 1ULL << 40);
    config.active_transactions = read_limit("activeTransactions", config.active_transactions, 1024);
    upload.part_size = read_limit("uploadPartBytes", upload.part_size, 1ULL << 30, 5 * 1024 * 1024);
    upload.buffer_budget = read_limit("uploadBufferBytes", upload.buffer_budget, 1ULL << 30, upload.part_size);
    upload.workers = read_limit("uploadWorkers", upload.workers, 64);
    upload.active_files = read_limit("uploadActiveFiles", upload.active_files, 64);
    upload.request_timeout = std::chrono::seconds(read_limit("requestSeconds", 60, 60));
    upload.request_retries = read_limit("uploadRequestRetries", upload.request_retries, 10, 0);
    const auto request_seconds = std::chrono::duration_cast<std::chrono::seconds>(upload.request_timeout).count();
    upload.connect_timeout = std::chrono::seconds(read_limit("uploadConnectSeconds", std::min<std::uint64_t>(5, request_seconds), request_seconds));
  }
  upload.operation_timeout = config.transaction_lifetime;
  config.transfer.restore_timeout = config.transaction_lifetime;
  upload.Validate();
}
}  // namespace

S3Config
ParseS3Config(std::string_view text)
{
  const auto value = utils::ParseJson(text, kMaxConfigBytes);
  ValidateConfigFields(value);

  S3Config config;
  config.store_id = value.at("storeId").get<std::string>();
  if (!IsValidStoreID(config.store_id))
    throw std::invalid_argument("S3 storeId must be a nonempty bounded identifier");
  config.transfer.connection = ParseConnection(value);
  config.bucket = value.at("bucket").get<std::string>();
  config.prefix = value.value("prefix", std::string());
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
