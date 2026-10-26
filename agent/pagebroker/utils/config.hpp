// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <charconv>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <fstream>
#include <limits>
#include <nlohmann/json.hpp>
#include <stdexcept>
#include <string>
#include <string_view>
#include <system_error>

namespace snapshot::pagebroker::utils::config {
// Requires a complete unsigned decimal value; never includes supplied text in errors.
inline std::uint64_t
ParseUnsigned(std::string_view text)
{
  if (text.empty())
    throw std::invalid_argument("expected unsigned integer");
  std::uint64_t value = 0;
  const auto [end, error] = std::from_chars(text.data(), text.data() + text.size(), value);
  if (error != std::errc{} || end != text.data() + text.size())
    throw std::invalid_argument("expected unsigned integer");
  return value;
}

// Callers supply schema-owned keys and add their configuration-specific context.
template<typename T>
T
ReadField(const nlohmann::json& object, const char* key)
{
  try {
    return object.at(key).get<T>();
  }
  catch (const nlohmann::json::exception&) {
    throw std::invalid_argument(std::string("missing or invalid configuration field: ") + key);
  }
}

// Read one extra byte so the caller's parser can reject oversized input without
// reading the rest of the file. File errors carry no supplied path or contents.
inline std::string
ReadBoundedConfigFile(const std::filesystem::path& path, std::size_t limit)
{
  if (limit >= static_cast<std::size_t>(std::numeric_limits<std::streamsize>::max()))
    throw std::invalid_argument("configuration file limit is too large");
  std::ifstream input(path, std::ios::binary);
  if (!input)
    throw std::ios_base::failure("cannot read configuration file");
  std::string text(limit + 1, '\0');
  input.read(text.data(), text.size());
  if (input.bad())
    throw std::ios_base::failure("cannot read configuration file");
  text.resize(input.gcount());
  return text;
}
}  // namespace snapshot::pagebroker::utils::config
