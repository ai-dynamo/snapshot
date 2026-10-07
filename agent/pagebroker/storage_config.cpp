// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "storage_config.hpp"

#include <sstream>
#include <stdexcept>

namespace snapshot::pagebroker {
namespace {

std::string
Trim(const std::string& value)
{
  const size_t start = value.find_first_not_of(" \t\r");
  if (start == std::string::npos)
    return "";
  const size_t end = value.find_last_not_of(" \t\r");
  return value.substr(start, end - start + 1);
}

// Unquote strips one layer of matching double quotes, as Helm's `| quote`
// always emits around every scalar this parser expects. A bare scalar is
// accepted unchanged for robustness, but every field the chart actually
// produces is quoted.
std::string
Unquote(const std::string& value)
{
  if (value.size() >= 2 && value.front() == '"' && value.back() == '"')
    return value.substr(1, value.size() - 2);
  return value;
}

// Indent returns the number of leading spaces, or -1 for a blank/comment line.
int
Indent(const std::string& line)
{
  size_t i = 0;
  while (i < line.size() && line[i] == ' ') ++i;
  if (i == line.size() || line[i] == '#')
    return -1;
  return static_cast<int>(i);
}

}  // namespace

StorageConfig
ParseStorageConfig(const std::string& yaml_text)
{
  StorageConfig config;
  bool in_pvc_block = false;
  int pvc_indent = -1;

  std::istringstream stream(yaml_text);
  std::string raw_line;
  while (std::getline(stream, raw_line)) {
    // Normalize a trailing '\r' from a CRLF source without rejecting it.
    if (!raw_line.empty() && raw_line.back() == '\r')
      raw_line.pop_back();
    const int indent = Indent(raw_line);
    if (indent < 0)
      continue;

    if (in_pvc_block && indent <= pvc_indent) {
      in_pvc_block = false;
    }

    const std::string trimmed = Trim(raw_line);
    const size_t colon = trimmed.find(':');
    if (colon == std::string::npos)
      throw std::runtime_error("storage.yaml: expected 'key: value' at '" + trimmed + "'");
    const std::string key = Trim(trimmed.substr(0, colon));
    const std::string rest = Trim(trimmed.substr(colon + 1));

    if (!in_pvc_block && indent == 0) {
      if (key == "type") {
        config.type = Unquote(rest);
      } else if (key == "pvc") {
        if (!rest.empty())
          throw std::runtime_error("storage.yaml: 'pvc' must introduce a nested block, not an inline value");
        in_pvc_block = true;
        pvc_indent = indent;
      }
      // Unknown top-level keys (e.g. a future backend's own block) are
      // ignored: this parser only extracts what PageBroker's PVC store needs.
      continue;
    }

    if (in_pvc_block) {
      if (key == "namespace")
        config.pvc_namespace = Unquote(rest);
      else if (key == "claimName")
        config.pvc_claim_name = Unquote(rest);
      else if (key == "basePath")
        config.pvc_base_path = Unquote(rest);
      continue;
    }
    // A line indented under something other than "pvc:" at the top level;
    // ignore it the same way unknown top-level keys are ignored.
  }

  if (config.type.empty())
    throw std::runtime_error("storage.yaml: missing required 'type'");
  if (config.type == "pvc" &&
      (config.pvc_namespace.empty() || config.pvc_claim_name.empty() || config.pvc_base_path.empty()))
    throw std::runtime_error("storage.yaml: type is 'pvc' but pvc.namespace/claimName/basePath is incomplete");
  return config;
}

}  // namespace snapshot::pagebroker
