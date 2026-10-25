// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include <fcntl.h>
#include <signal.h>
#include <unistd.h>

#include <cstdlib>
#include <filesystem>
#include <iostream>
#include <fstream>
#include <string>

int
main(int argc, char** argv)
{
  if (argc < 4)
    return 2;
  const std::string command(argv[1]);
  if (command == "lsjson") {
    // The client must bound output and reap us even when we never exit.
    std::cout << std::string(128 * 1024, 'x') << std::flush;
    for (;;)
      pause();
  }
  if (command != "copy")
    return 3;
  const std::filesystem::path root(argv[2]);
  for (int i = 4; i < argc; ++i) {
    if (std::string(argv[i]) == "--copy-links")
      return 8;
  }
  const std::string remote(argv[3]);
  if (remote == "pagebroker:bucket/buffer-limit") {
    auto has_argument = [&](const std::string& flag, const std::string& value) {
      for (int i = 4; i + 1 < argc; ++i) {
        if (argv[i] == flag && argv[i + 1] == value)
          return true;
      }
      return false;
    };
    return has_argument("--max-buffer-memory", "9437184B") &&
           has_argument("--s3-chunk-size", "8388609B") &&
           has_argument("--s3-upload-concurrency", "4") ? 0 : 10;
  }
  if (remote == "pagebroker:bucket/batch") {
    std::ofstream(root / "launches", std::ios::app) << "launch\n";
    std::string first, second, extra;
    return std::getline(std::cin, first) && first == "source" &&
           std::getline(std::cin, second) && second == "nested/# second" &&
           !std::getline(std::cin, extra) ? 0 : 9;
  }
  if (remote == "pagebroker:bucket/literal/$(false); `false` with spaces quote'\"") {
    const auto* token = std::getenv("RCLONE_CONFIG_PAGEBROKER_SESSION_TOKEN");
    return token && std::string(token) == "fake-session-token" && !std::getenv("PATH") ? 0 : 4;
  }
  if (remote.starts_with("pagebroker:bucket/descriptors/")) {
    if (fcntl(std::stoi(remote.substr(remote.rfind('/') + 1)), F_GETFD) >= 0)
      return 5;
    std::string bytes;
    std::getline(std::cin, bytes);
    if (bytes != "source")
      return 6;
    std::ifstream file(root / bytes);
    std::getline(file, bytes);
    return bytes == "original" ? 0 : 6;
  }
  if (remote == "pagebroker:bucket/flood") {
    for (int i = 0; i < 256; ++i)
      std::cerr << std::string(4096, 'e');
    return 0;
  }
  if (remote == "pagebroker:bucket/stubborn") {
    signal(SIGTERM, SIG_IGN);
    // Readiness makes cancellation deterministic after installing the handler.
    const auto path = (root / "source.ready").string();
    const int ready = open(path.c_str(), O_WRONLY | O_CREAT | O_CLOEXEC, 0600);
    if (ready < 0)
      return 7;
    close(ready);
    for (;;)
      pause();
  }
  return 127;
}
