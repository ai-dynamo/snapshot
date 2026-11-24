// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include <fcntl.h>
#include <signal.h>
#include <unistd.h>

#include <cstdlib>
#include <filesystem>
#include <iostream>
#include <fstream>
#include <set>
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
    const auto* auth = std::getenv("RCLONE_CONFIG_PAGEBROKER_ENV_AUTH");
    return token && std::string(token) == "fake-session-token" && auth && std::string(auth) == "false" &&
           !std::getenv("PATH") ? 0 : 4;
  }
  if (remote.starts_with("pagebroker:bucket/default-chain")) {
    std::set<std::string> actual;
    for (char** variable = environ; *variable; ++variable)
      actual.insert(*variable);
    std::string ca;
    for (int i = 4; i + 1 < argc; ++i) {
      if (std::string(argv[i]) == "--ca-cert")
        ca = argv[i + 1];
    }
    std::set<std::string> expected{
        "RCLONE_CONFIG_PAGEBROKER_TYPE=s3",
        "RCLONE_CONFIG_PAGEBROKER_PROVIDER=AWS",
        "RCLONE_CONFIG_PAGEBROKER_ENV_AUTH=true",
        "RCLONE_CONFIG_PAGEBROKER_REGION=us-west-2",
        "RCLONE_CONFIG_PAGEBROKER_ENDPOINT=",
        "AWS_EC2_METADATA_DISABLED=true",
        "AWS_CONFIG_FILE=/dev/null",
        "AWS_SHARED_CREDENTIALS_FILE=/dev/null",
        "AWS_ROLE_ARN=arn:aws:iam::000000000000:role/fake-role",
        "AWS_WEB_IDENTITY_TOKEN_FILE=/var/run/fake/web-identity-token",
        "AWS_CONTAINER_CREDENTIALS_FULL_URI=https://credentials.example.test/v1",
        "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE=/var/run/fake/container-token",
        remote.ends_with("-region") ? "AWS_REGION=eu-central-1" : "AWS_REGION=us-west-2"};
    if (remote.ends_with("-ca")) {
      expected.insert("AWS_CA_BUNDLE=" + ca);
      expected.insert("SSL_CERT_FILE=" + ca);
    }
    for (const auto& entry : actual) {
      if (!expected.contains(entry))
        std::cerr << "unexpected: " << entry << '\n';
    }
    for (const auto& entry : expected) {
      if (!actual.contains(entry))
        std::cerr << "missing: " << entry << '\n';
    }
    return actual == expected ? 0 : 11;
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
  if (remote == "pagebroker:bucket/no-credentials") {
    std::cerr << "{\"level\":\"error\",\"msg\":\"Failed to create file system for \\\"pagebroker:bucket/no-credentials\\\": "
                 "operation error S3: HeadBucket, get identity: get credentials: failed to refresh cached credentials, "
                 "no EC2 IMDS role found, operation error ec2imds: GetMetadata, access disabled to EC2 IMDS via client "
                 "option, or \\\"AWS_EC2_METADATA_DISABLED\\\" environment variable\"}\n";
    return 1;
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
