// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include <fcntl.h>
#include <signal.h>
#include <unistd.h>

#include <cstdlib>
#include <filesystem>
#include <iostream>
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
  if (command != "copyto" || std::string(argv[2]) != "/proc/self/fd/0")
    return 3;
  const std::string remote(argv[3]);
  if (remote == "pagebroker:bucket/literal/$(false); `false` with spaces quote'\"") {
    const auto* token = std::getenv("RCLONE_CONFIG_PAGEBROKER_SESSION_TOKEN");
    return token && std::string(token) == "fake-session-token" && !std::getenv("PATH") ? 0 : 4;
  }
  if (remote.starts_with("pagebroker:bucket/descriptors/")) {
    if (fcntl(std::stoi(remote.substr(remote.rfind('/') + 1)), F_GETFD) >= 0)
      return 5;
    std::string bytes;
    std::getline(std::cin, bytes);
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
    const auto path = std::filesystem::read_symlink("/proc/self/fd/0").string() + ".ready";
    const int ready = open(path.c_str(), O_WRONLY | O_CREAT | O_CLOEXEC, 0600);
    if (ready < 0)
      return 7;
    close(ready);
    for (;;)
      pause();
  }
  return 127;
}
