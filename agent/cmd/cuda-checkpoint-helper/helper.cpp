// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include "daemon.hpp"

#include <charconv>
#include <cstdlib>
#include <iostream>
#include <stdexcept>
#include <string_view>
#include <syncstream>

namespace {
using snapshot::cuda_checkpoint::DaemonOptions;
DaemonOptions
ParseOptions(int argc, char** argv)
{
  DaemonOptions options;
  if (argc % 2 != 1)
    throw std::runtime_error("CUDA helper options require values");
  for (int index = 1; index < argc; index += 2) {
    const std::string_view option(argv[index]), value(argv[index + 1]);
    if (option == "--cuda-storage-mode") {
      if (value != "custom" && value != "driver")
        throw std::runtime_error("unknown CUDA storage mode");
      options.custom_storage = value == "custom";
      continue;
    }
    size_t number = 0;
    const auto [end, error] = std::from_chars(value.data(), value.data() + value.size(), number);
    if (error != std::errc{} || end != value.data() + value.size())
      throw std::runtime_error("invalid GPU allocation option");
    if (option == "--transfer-buffer-count")
      options.buffer_count = number;
    else if (option == "--transfer-chunk-bytes")
      options.chunk_bytes = number;
    else if (option == "--max-pinned-bytes")
      options.max_pinned_bytes = number;
    else
      throw std::runtime_error("unknown CUDA helper option");
  }
  return options;
}
} // namespace

extern "C" int cuda_checkpoint_cli_main(int argc, char** argv);

int
main(int argc, char** argv)
{
  if (argc < 2 || std::string_view(argv[1]) != "--daemon")
    return cuda_checkpoint_cli_main(argc, argv);
  try {
    snapshot::cuda_checkpoint::RunDaemon(3, ParseOptions(argc - 1, argv + 1));
  } catch (const std::exception& error) {
    std::osyncstream(std::cerr) << "CUDA helper failed: " << error.what() << '\n';
    std::_Exit(1);
  }
}
