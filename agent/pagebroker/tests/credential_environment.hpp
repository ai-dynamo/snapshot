// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <unistd.h>

#include <cstdlib>
#include <map>
#include <string>
#include <string_view>
#include <vector>

extern char** environ;

namespace snapshot::pagebroker::test {
// Clears every AWS_* variable and SSL_CERT_FILE for one test, and restores the
// original environment afterwards, also when the test throws.
class CredentialEnvironment {
 public:
  CredentialEnvironment()
  {
    for (const auto& name : Present()) {
      saved_[name] = std::getenv(name.c_str());
      unsetenv(name.c_str());
    }
  }
  ~CredentialEnvironment() noexcept
  {
    for (const auto& name : Present())
      unsetenv(name.c_str());
    for (const auto& [name, value] : saved_)
      setenv(name.c_str(), value.c_str(), 1);
  }
  CredentialEnvironment(const CredentialEnvironment&) = delete;
  CredentialEnvironment& operator=(const CredentialEnvironment&) = delete;

 private:
  static std::vector<std::string> Present()
  {
    std::vector<std::string> names;
    for (char** variable = environ; variable != nullptr && *variable != nullptr; ++variable) {
      const std::string_view entry(*variable);
      const std::string name(entry.substr(0, entry.find('=')));
      if (name.starts_with("AWS_") || name == "SSL_CERT_FILE")
        names.push_back(name);
    }
    return names;
  }

  std::map<std::string, std::string> saved_;
};
}  // namespace snapshot::pagebroker::test
