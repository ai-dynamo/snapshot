// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <nlohmann/json.hpp>

#include <set>
#include <stdexcept>
#include <string_view>
#include <vector>

namespace snapshot::pagebroker::utils {
// Parses JSON with a caller-supplied byte limit and fixed depth/event limits.
// Rejects duplicate object members instead of silently choosing one value;
// callers remain responsible for validating their configuration or index schema.
inline nlohmann::json
ParseJson(std::string_view text, std::size_t limit)
{
  if (text.empty() || text.size() > limit)
    throw std::invalid_argument("JSON exceeds size limit");
  std::vector<std::set<std::string>> keys;
  std::size_t events = 0;
  using Json = nlohmann::json;
  return Json::parse(text, [&](int depth, Json::parse_event_t event, Json& value) {
    if (depth > 12 || ++events > 160128)
      throw std::invalid_argument("JSON exceeds structural limits");
    if (event == Json::parse_event_t::object_start)
      keys.emplace_back();
    else if (event == Json::parse_event_t::object_end)
      keys.pop_back();
    else if (event == Json::parse_event_t::key && !keys.back().insert(value.get<std::string>()).second)
      throw std::invalid_argument("duplicate JSON member");
    return true;
  });
}

// Extracts an unsigned JSON integer within the inclusive bounds. Rejects signed
// values, floating-point values and strings rather than coercing their types.
inline std::uint64_t
Unsigned(const nlohmann::json& value, std::uint64_t maximum, std::uint64_t minimum = 0)
{
  if (!value.is_number_unsigned())
    throw std::invalid_argument("expected unsigned integer");
  const auto number = value.get<std::uint64_t>();
  if (number < minimum || number > maximum)
    throw std::invalid_argument("integer outside allowed range");
  return number;
}
}  // namespace snapshot::pagebroker::utils
