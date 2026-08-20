#include <charconv>
#include <iostream>
#include <string_view>

#include "daemon.hpp"

namespace {
bool
ParseMaxConcurrentRequests(std::string_view value, size_t& max_concurrent_requests)
{
  const auto [end, error] = std::from_chars(value.data(), value.data() + value.size(), max_concurrent_requests);
  return error == std::errc{} && end == value.data() + value.size() && max_concurrent_requests > 0;
}
}  // namespace

int
main(int argc, char** argv)
{
  size_t max_concurrent_requests;
  if (argc != 6 || std::string_view(argv[4]) != "--max-concurrent-requests" ||
      !ParseMaxConcurrentRequests(argv[5], max_concurrent_requests)) {
    std::cerr << "usage: pagebroker socket_path staging_directory storage_root --max-concurrent-requests max_concurrent_requests\n";
    return static_cast<int>(ExitCode::INVALID_ARGUMENTS);
  }
  return static_cast<int>(RunDaemon(argv[1], argv[2], argv[3], max_concurrent_requests));
}
