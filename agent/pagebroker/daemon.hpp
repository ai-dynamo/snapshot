#pragma once

#include <cstddef>
#include <filesystem>

enum class ExitCode { SUCCESS = 0, FAILURE = 1, INVALID_ARGUMENTS = 2 };

ExitCode RunDaemon(
    const std::filesystem::path& socket_path,
    const std::filesystem::path& staging_directory,
    const std::filesystem::path& storage_root,
    size_t max_concurrent_requests);
