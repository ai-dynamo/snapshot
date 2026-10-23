// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_client.hpp"

#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#include <algorithm>
#include <array>
#include <cerrno>
#include <charconv>
#include <climits>
#include <cstdlib>
#include <iterator>
#include <limits>
#include <nlohmann/json.hpp>
#include <sstream>
#include <system_error>
#include <utility>

#include "file_descriptor.hpp"

#ifndef PAGEBROKER_RCLONE_PATH
#define PAGEBROKER_RCLONE_PATH "/usr/local/bin/rclone"
#endif

namespace snapshot::pagebroker {
namespace {
using Clock = std::chrono::steady_clock;
constexpr std::uint64_t kMiB = 1024 * 1024;
constexpr char kSizeField[] = "Size";
constexpr char kDirectoryField[] = "IsDir";
constexpr char kMessageField[] = "msg";
constexpr char kStatusPrefix[] = "StatusCode: ";
constexpr char kRemotePrefix[] = "pagebroker:";
constexpr char kInputPath[] = "/proc/self/fd/0";
constexpr std::size_t kMetadataStatLimit = 16 * 1024;

constexpr const char* kCommonArguments[] = {"--config",
                                            "/dev/null",
                                            "--use-json-log",
                                            "--log-level",
                                            "ERROR",
                                            "--stats",
                                            "0",
                                            "--s3-no-check-bucket",
                                            "--s3-no-head",
                                            "--s3-encoding",
                                            "None",
                                            "--buffer-size",
                                            "0",
                                            "--transfers",
                                            "1",
                                            "--checkers",
                                            "1",
                                            "--retries",
                                            "1"};
constexpr const char* kCopyArguments[] = {"--copy-links", "--ignore-times", "--no-check-dest"};
constexpr const char* kReadArguments[] = {"--files-from-raw", kInputPath, "--no-traverse"};
constexpr const char* kStatArguments[] = {"--no-modtime", "--no-mimetype", "--files-only", "--recursive"};
constexpr const char* kHttpArguments[] = {"--s3-use-data-integrity-protections=false",
                                          "--s3-use-unsigned-payload=true"};
constexpr const char* kCreateOnlyArguments[] = {"--header-upload", "If-None-Match: *"};
constexpr char kPathStyleFlag[] = "--s3-force-path-style=";
constexpr char kUploadCutoffFlag[] = "--s3-upload-cutoff";
constexpr char kChunkSizeFlag[] = "--s3-chunk-size";
constexpr char kUploadConcurrencyFlag[] = "--s3-upload-concurrency";
constexpr char kLowLevelRetriesFlag[] = "--low-level-retries";
constexpr char kConnectTimeoutFlag[] = "--contimeout";
constexpr char kTimeoutFlag[] = "--timeout";
constexpr char kCaFlag[] = "--ca-cert";
constexpr char kCopyCommand[] = "copyto";
constexpr char kStatCommand[] = "lsjson";
constexpr char kReadCommand[] = "cat";
constexpr char kRemoteTypeEnv[] = "RCLONE_CONFIG_PAGEBROKER_TYPE=s3";
constexpr char kRemoteProviderEnv[] = "RCLONE_CONFIG_PAGEBROKER_PROVIDER=AWS";
constexpr char kExplicitAuthEnv[] = "RCLONE_CONFIG_PAGEBROKER_ENV_AUTH=false";
constexpr char kRemoteAccessKeyEnv[] = "RCLONE_CONFIG_PAGEBROKER_ACCESS_KEY_ID=";
constexpr char kRemoteSecretKeyEnv[] = "RCLONE_CONFIG_PAGEBROKER_SECRET_ACCESS_KEY=";
constexpr char kRemoteTokenEnv[] = "RCLONE_CONFIG_PAGEBROKER_SESSION_TOKEN=";
constexpr char kRemoteRegionEnv[] = "RCLONE_CONFIG_PAGEBROKER_REGION=";
constexpr char kRemoteEndpointEnv[] = "RCLONE_CONFIG_PAGEBROKER_ENDPOINT=";
constexpr char kDisableImdsEnv[] = "AWS_EC2_METADATA_DISABLED=true";
constexpr char kDisableAwsConfigEnv[] = "AWS_CONFIG_FILE=/dev/null";
constexpr char kDisableAwsFileEnv[] = "AWS_SHARED_CREDENTIALS_FILE=/dev/null";
constexpr char kDirectoryBucketSuffix[] = "--x-s3";

constexpr char kFalse[] = "false";
constexpr char kTrue[] = "true";
constexpr char kBytesSuffix[] = "B";
constexpr char kMillisecondsSuffix[] = "ms";

bool
IsSupportedObjectLocation(const S3ObjectLocation& object)
{
  const auto& bucket = object.bucket;
  if (bucket.size() < 3 || bucket.size() > 63)
    return false;
  if (object.key.empty() || object.key.size() > 1024)
    return false;
  if (bucket.find_first_not_of("abcdefghijklmnopqrstuvwxyz0123456789.-") != std::string::npos)
    return false;
  if (bucket.front() == '.' || bucket.front() == '-')
    return false;
  if (bucket.back() == '.' || bucket.back() == '-')
    return false;
  return bucket.find("..") == std::string::npos && !bucket.ends_with(kDirectoryBucketSuffix);
}

bool
SameTimestamp(const timespec& left, const timespec& right)
{
  return left.tv_sec == right.tv_sec && left.tv_nsec == right.tv_nsec;
}

void
CheckSource(int descriptor, const struct stat& original)
{
  struct stat current;
  if (fstat(descriptor, &current) < 0)
    throw std::system_error(errno, std::generic_category(), "stat S3 upload source");
  if (current.st_size != original.st_size || !SameTimestamp(current.st_mtim, original.st_mtim) ||
      !SameTimestamp(current.st_ctim, original.st_ctim))
    throw S3Error();
}

// Only expose HTTP status, never rclone's raw logs or server text.
int
HttpStatus(const std::string& logs)
{
  int http_status = 0;
  std::istringstream lines(logs);
  std::string line;
  while (std::getline(lines, line)) {
    const auto json = nlohmann::json::parse(line, nullptr, false);
    if (!json.is_object() || !json.contains(kMessageField) || !json[kMessageField].is_string())
      continue;
    const auto message = json[kMessageField].get<std::string>();
    const auto status = message.find(kStatusPrefix);
    if (status != std::string::npos) {
      int value = 0;
      const auto* begin = message.data() + status + sizeof(kStatusPrefix) - 1;
      const auto result = std::from_chars(begin, message.data() + message.size(), value);
      if (result.ec == std::errc{} && value >= 100 && value <= 599)
        http_status = value;
    }
  }
  return http_status;
}

FileDescriptor
InputBytes(const std::string& bytes)
{
  FileDescriptor file(memfd_create("pagebroker-s3-input", MFD_CLOEXEC));
  if (file.get() < 0)
    throw std::system_error(errno, std::generic_category(), "create S3 input");
  std::size_t offset = 0;
  while (offset < bytes.size()) {
    const auto count = write(file.get(), bytes.data() + offset, bytes.size() - offset);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      throw std::system_error(errno, std::generic_category(), "write S3 input");
    offset += count;
  }
  if (lseek(file.get(), 0, SEEK_SET) < 0)
    throw std::system_error(errno, std::generic_category(), "rewind S3 input");
  return file;
}

std::string
Remote(const S3ObjectLocation& object)
{
  return kRemotePrefix + object.bucket + "/" + object.key;
}

std::vector<std::string>
CopyArguments(const S3ObjectLocation& object, bool create_only)
{
  std::vector<std::string> command{kCopyCommand, kInputPath, Remote(object)};
  command.insert(command.end(), std::begin(kCopyArguments), std::end(kCopyArguments));
  if (create_only)
    command.insert(command.end(), std::begin(kCreateOnlyArguments), std::end(kCreateOnlyArguments));
  return command;
}

// Exact-key lookups avoid bucket listing and wildcard interpretation.
std::vector<std::string>
ReadArguments(const char* command, const S3ObjectLocation& object)
{
  std::vector<std::string> arguments{command, kRemotePrefix + object.bucket};
  arguments.insert(arguments.end(), std::begin(kReadArguments), std::end(kReadArguments));
  return arguments;
}

constexpr char kAccessKeyEnv[] = "AWS_ACCESS_KEY_ID";
constexpr char kSecretKeyEnv[] = "AWS_SECRET_ACCESS_KEY";
constexpr char kTokenEnv[] = "AWS_SESSION_TOKEN";
constexpr char kRegionEnv[] = "AWS_REGION";
constexpr char kDefaultRegionEnv[] = "AWS_DEFAULT_REGION";

std::string
Environment(const char* name)
{
  const auto* value = std::getenv(name);
  return value ? value : "";
}

constexpr char kExecutable[] = PAGEBROKER_RCLONE_PATH;
constexpr std::size_t kDiagnosticsLimit = 64 * 1024;
constexpr auto kTerminationGrace = std::chrono::seconds(2);
constexpr int kPollMilliseconds = 20;

FileDescriptor
HighDescriptor(int descriptor)
{
  const auto duplicate = fcntl(descriptor, F_DUPFD_CLOEXEC, 10);
  if (duplicate < 0)
    throw std::system_error(errno, std::generic_category(), "duplicate rclone descriptor");
  return FileDescriptor(duplicate);
}

struct Pipe {
  Pipe() : read_end(-1), write_end(-1)
  {
    int descriptors[2];
    if (pipe2(descriptors, O_CLOEXEC) < 0)
      throw std::system_error(errno, std::generic_category(), "create rclone pipe");
    FileDescriptor read(descriptors[0]), write(descriptors[1]);
    read_end = HighDescriptor(read.get());
    write_end = HighDescriptor(write.get());
    if (fcntl(read_end.get(), F_SETFL, O_NONBLOCK) < 0)
      throw std::system_error(errno, std::generic_category(), "configure rclone pipe");
  }
  FileDescriptor read_end, write_end;
};

// Fallback on local errors also kills and reaps. No detached children or threads
// may retain the source after UploadFile returns.
class Child {
 public:
  explicit Child(pid_t pid) : pid_(pid) {}
  ~Child()
  {
    if (pid_ > 0) {
      kill(pid_, SIGKILL);
      while (waitpid(pid_, nullptr, 0) < 0 && errno == EINTR) {
      }
    }
  }
  bool Poll(int& status)
  {
    const auto result = waitpid(pid_, &status, WNOHANG);
    if (result < 0 && errno != EINTR)
      throw std::system_error(errno, std::generic_category(), "wait for rclone");
    if (result == pid_) {
      pid_ = -1;
      return true;
    }
    return false;
  }
  void Signal(int signal) const
  {
    if (pid_ > 0)
      kill(pid_, signal);
  }

 private:
  pid_t pid_;
};

// Limit work per poll so a noisy child cannot starve cancellation checks. Logs
// are kept only in a bounded tail and never forwarded directly to callers.
bool
Drain(int descriptor, std::string& bytes, std::size_t limit, bool diagnostics)
{
  std::array<char, 8192> buffer;
  for (unsigned i = 0; i < 16; ++i) {
    const auto count = read(descriptor, buffer.data(), buffer.size());
    if (count == 0)
      return true;
    if (count < 0) {
      if (errno == EINTR)
        continue;
      if (errno == EAGAIN)
        return false;
      throw std::system_error(errno, std::generic_category(), "read rclone output");
    }
    if (!diagnostics && static_cast<std::size_t>(count) > limit - bytes.size())
      throw S3ResponseError("rclone response exceeds size limit");
    bytes.append(buffer.data(), count);
    if (diagnostics && bytes.size() > limit)
      bytes.erase(0, bytes.size() - limit);
  }
  return false;
}

} // namespace

S3Connection
ResolveS3Credentials(S3Connection connection)
{
  connection.Validate();
  if (connection.access_key_id.empty()) {
    connection.access_key_id = Environment(kAccessKeyEnv);
    connection.secret_access_key = Environment(kSecretKeyEnv);
    connection.session_token = Environment(kTokenEnv);
  }
  if (connection.access_key_id.empty() || connection.secret_access_key.empty())
    throw std::invalid_argument("external S3 credentials are missing");
  if (connection.region.empty()) {
    connection.region = Environment(kRegionEnv);
    if (connection.region.empty())
      connection.region = Environment(kDefaultRegionEnv);
  }
  if (connection.ca_file.empty())
    connection.ca_file = Environment(kAwsCaBundleEnv);
  connection.Validate();
  return connection;
}

void
S3Connection::Validate() const
{
  for (const auto* value : {&region, &endpoint, &ca_file, &access_key_id, &secret_access_key, &session_token}) {
    if (value->find('\0') != std::string::npos || value->find_first_of("\n\r") != std::string::npos)
      throw std::invalid_argument("S3 settings cannot contain NUL or line breaks");
  }
  if (access_key_id.empty() != secret_access_key.empty() || (!session_token.empty() && access_key_id.empty()))
    throw std::invalid_argument("S3 explicit credentials require an access key and secret key");
  if (!endpoint.empty() && !endpoint.starts_with(kHttpsEndpointPrefix) && !endpoint.starts_with(kHttpEndpointPrefix))
    throw std::invalid_argument("S3 endpoint requires an http or https scheme");
}

void
S3ObjectLocation::Validate() const
{
  for (const auto* value : {&bucket, &key}) {
    if (value->find('\0') != std::string::npos || value->find_first_of("\n\r") != std::string::npos)
      throw std::invalid_argument("S3 object location cannot contain NUL or line breaks");
  }
  if (!IsSupportedObjectLocation(*this))
    throw std::invalid_argument("S3 upload requires an ordinary bucket name and a supported nonempty key");
  // rclone treats keys as paths. Reject spelling it would normalize rather than
  // reading or overwriting a different object. Checkpoint keys are canonical.
  std::istringstream components(key);
  std::string component;
  while (std::getline(components, component, '/')) {
    if (component.empty() || component == "." || component == "..")
      throw std::invalid_argument("S3 object key must be a canonical relative path");
  }
  if (key.back() == '/')
    throw std::invalid_argument("S3 object key cannot end in a slash");
}

void
S3Limits::Validate() const
{
  if (part_size < 8 * kMiB || part_size > 5 * 1024 * kMiB || active_files == 0 ||
      buffer_budget / part_size < active_files || buffer_budget > std::numeric_limits<std::size_t>::max() ||
      request_retries > 10)
    throw std::invalid_argument("invalid S3 upload resource limits");
  for (auto timeout : {connect_timeout, request_timeout}) {
    if (timeout.count() <= 0 || timeout > std::chrono::hours(24) || timeout.count() > std::numeric_limits<long>::max())
      throw std::invalid_argument("S3 timeouts must be positive and at most 24 hours");
  }
}

void
S3Limits::ValidateFileSize(std::uint64_t size) const
{
  Validate();
  if (size > static_cast<std::uint64_t>(std::numeric_limits<off_t>::max()) ||
      size / part_size + (size % part_size != 0) > 10'000)
    throw std::invalid_argument("S3 file exceeds the configured 10000-part limit");
}

// A permit bounds admitted operations through preparation, execution and cleanup.
struct S3Client::Operation {
  Operation(S3Client& owner, Cancellation cancellation) : owner(owner), cancellation(cancellation)
  {
    if (cancellation.Deadline() == Clock::time_point::max())
      throw std::invalid_argument("S3 operations require a caller deadline");
    std::unique_lock lock(owner.mutex_);
    owner.changed_.wait_until(lock, this->cancellation.Token(), this->cancellation.Deadline(), [&] {
      return owner.active_ < owner.limits_.active_files || this->cancellation.IsCancelled();
    });
    cancellation.ThrowIfCancelled();
    ++owner.active_;
  }
  ~Operation()
  {
    std::lock_guard lock(owner.mutex_);
    --owner.active_;
    owner.changed_.notify_one();
  }
  Operation(const Operation&) = delete;
  Operation& operator=(const Operation&) = delete;

  S3Client& owner;
  const Cancellation cancellation;
};

S3Client::S3Client(S3Connection connection, S3Limits limits) : limits_(limits)
{
  connection.Validate();
  limits_.Validate();
  if (connection.access_key_id.empty())
    throw std::invalid_argument("rclone requires resolved startup S3 credentials");
  if (!connection.ca_file.empty() && !std::filesystem::is_regular_file(connection.ca_file))
    throw std::invalid_argument("S3 CA certificate file does not exist");
  // The child receives only the startup snapshot, never ambient credential sources.
  environment_ = {kRemoteTypeEnv,
                  kRemoteProviderEnv,
                  kExplicitAuthEnv,
                  kRemoteAccessKeyEnv + connection.access_key_id,
                  kRemoteSecretKeyEnv + connection.secret_access_key,
                  kRemoteTokenEnv + connection.session_token,
                  kRemoteRegionEnv + connection.region,
                  kRemoteEndpointEnv + connection.endpoint,
                  kDisableImdsEnv,
                  kDisableAwsConfigEnv,
                  kDisableAwsFileEnv};
  // At most four multipart buffers per process, within the aggregate payload budget.
  const auto concurrency = std::min<std::uint64_t>(4, limits.buffer_budget / limits.active_files / limits.part_size);
  arguments_.assign(std::begin(kCommonArguments), std::end(kCommonArguments));
  const std::string part_bytes = std::to_string(limits.part_size) + kBytesSuffix;
  arguments_.insert(arguments_.end(),
                    {std::string(kPathStyleFlag) + (connection.use_virtual_addressing ? kFalse : kTrue),
                     kUploadCutoffFlag, part_bytes, kChunkSizeFlag, part_bytes, kUploadConcurrencyFlag,
                     std::to_string(concurrency), kLowLevelRetriesFlag, std::to_string(limits.request_retries + 1),
                     kConnectTimeoutFlag, std::to_string(limits.connect_timeout.count()) + kMillisecondsSuffix,
                     kTimeoutFlag, std::to_string(limits.request_timeout.count()) + kMillisecondsSuffix});
  if (connection.endpoint.starts_with(kHttpEndpointPrefix))
    arguments_.insert(arguments_.end(), std::begin(kHttpArguments), std::end(kHttpArguments));
  if (!connection.ca_file.empty())
    arguments_.insert(arguments_.end(), {kCaFlag, connection.ca_file});
}

std::string
S3Client::Execute(Operation& operation, std::vector<std::string> command, int input, std::size_t output_limit)
{
  command.insert(command.end(), arguments_.begin(), arguments_.end());
  std::vector<char*> argv{const_cast<char*>(kExecutable)};
  for (const auto& argument : command)
    argv.push_back(const_cast<char*>(argument.c_str()));
  argv.push_back(nullptr);
  std::vector<char*> envp;
  for (const auto& variable : environment_)
    envp.push_back(const_cast<char*>(variable.c_str()));
  envp.push_back(nullptr);
  auto stdin_fd = HighDescriptor(input);
  Pipe output, diagnostics;
  const auto parent = getpid();
  const int in = stdin_fd.get(), out = output.write_end.get(), err = diagnostics.write_end.get();
  // Build all state in the parent. The child does no allocation, logging,
  // locking, filesystem traversal or C++ cleanup between fork and execve.
  sigset_t empty_mask;
  sigemptyset(&empty_mask);
  struct sigaction default_action {};
  default_action.sa_handler = SIG_DFL;
  sigemptyset(&default_action.sa_mask);
  operation.cancellation.ThrowIfCancelled();
  const auto pid = fork();
  if (pid < 0)
    throw S3Error();
  if (pid == 0) {
    if (dup2(in, STDIN_FILENO) < 0 || dup2(out, STDOUT_FILENO) < 0 || dup2(err, STDERR_FILENO) < 0 ||
        syscall(SYS_close_range, 3U, UINT_MAX, 0U) < 0 || sigprocmask(SIG_SETMASK, &empty_mask, nullptr) < 0 ||
        sigaction(SIGTERM, &default_action, nullptr) < 0 || sigaction(SIGINT, &default_action, nullptr) < 0 ||
        sigaction(SIGPIPE, &default_action, nullptr) < 0 || prctl(PR_SET_PDEATHSIG, SIGKILL) < 0)
      _exit(127);
    if (getppid() != parent)
      _exit(127);
    execve(kExecutable, argv.data(), envp.data());
    _exit(127);
  }
  Child child(pid);
  output.write_end = FileDescriptor(-1);
  diagnostics.write_end = FileDescriptor(-1);
  std::string output_bytes, diagnostic_bytes;
  bool output_done = false, diagnostics_done = false, reaped = false;
  int status = 0;
  auto kill_at = Clock::time_point::max();
  while (!reaped || !output_done || !diagnostics_done) {
    if (operation.cancellation.IsCancelled() && kill_at == Clock::time_point::max()) {
      child.Signal(SIGTERM);
      kill_at = Clock::now() + kTerminationGrace;
    }
    if (Clock::now() >= kill_at)
      child.Signal(SIGKILL);
    if (!output_done)
      output_done = Drain(output.read_end.get(), output_bytes, output_limit, false);
    if (!diagnostics_done)
      diagnostics_done = Drain(diagnostics.read_end.get(), diagnostic_bytes, kDiagnosticsLimit, true);
    if (!reaped)
      reaped = child.Poll(status);
    if (!reaped || !output_done || !diagnostics_done) {
      pollfd descriptors[] = {{output_done ? -1 : output.read_end.get(), POLLIN, 0},
                              {diagnostics_done ? -1 : diagnostics.read_end.get(), POLLIN, 0}};
      if (poll(descriptors, 2, kPollMilliseconds) < 0 && errno != EINTR)
        throw std::system_error(errno, std::generic_category(), "poll rclone output");
    }
  }
  const auto exit_code = WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status);
  operation.cancellation.ThrowIfCancelled();
  if (exit_code != 0)
    throw S3Error(HttpStatus(diagnostic_bytes));
  return output_bytes;
}

std::uint64_t
S3Client::UploadFile(const std::filesystem::path& path, const S3ObjectLocation& destination, Cancellation cancellation)
{
  destination.Validate();
  if (path.native().find('\0') != std::string::npos)
    throw std::invalid_argument("S3 source path cannot contain NUL");
  Operation operation(*this, cancellation);
  FileDescriptor source(open(path.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK));
  if (source.get() < 0)
    throw std::system_error(errno, std::generic_category(), "open S3 upload source");
  struct stat status;
  if (fstat(source.get(), &status) < 0)
    throw std::system_error(errno, std::generic_category(), "stat S3 upload source");
  if (!S_ISREG(status.st_mode) || status.st_size < 0)
    throw std::invalid_argument("S3 upload source must be a regular file");
  const auto size = static_cast<std::uint64_t>(status.st_size);
  limits_.ValidateFileSize(size);
  // copyto opens the validated inode through procfs, retaining seekability for retries.
  Execute(operation, CopyArguments(destination, false), source.get(), 0);
  CheckSource(source.get(), status);
  cancellation.ThrowIfCancelled();
  return size;
}

std::optional<std::uint64_t>
S3Client::Head(const S3ObjectLocation& object, Cancellation cancellation)
{
  object.Validate();
  Operation operation(*this, cancellation);
  const auto filter = InputBytes(object.key + "\n");
  auto command = ReadArguments(kStatCommand, object);
  command.insert(command.end(), std::begin(kStatArguments), std::end(kStatArguments));
  const auto bytes = Execute(operation, std::move(command), filter.get(), kMetadataStatLimit);
  const auto entries = nlohmann::json::parse(bytes, nullptr, false);
  if (!entries.is_array() || entries.size() > 1)
    throw S3ResponseError("invalid S3 object size response");
  if (entries.empty())
    return std::nullopt;
  const auto& value = entries.front();
  if (!value.is_object() || !value.contains(kSizeField) || !value[kSizeField].is_number_integer() ||
      !value.contains(kDirectoryField) || value[kDirectoryField] != false || value[kSizeField].get<std::int64_t>() < 0)
    throw S3ResponseError("invalid S3 object size response");
  return value[kSizeField].get<std::uint64_t>();
}

std::optional<std::string>
S3Client::Get(const S3ObjectLocation& object, std::size_t max_bytes, Cancellation cancellation)
{
  if (max_bytes >= static_cast<std::size_t>(std::numeric_limits<std::ptrdiff_t>::max()))
    throw std::invalid_argument("invalid S3 metadata byte limit");
  const auto size = Head(object, cancellation);
  if (!size)
    return std::nullopt;
  if (*size > max_bytes)
    throw S3ResponseError("S3 object exceeds size limit");
  if (*size == 0)
    return std::string{};
  // An exact files-from list plus no-traverse uses NewObject, never ListBucket,
  // even if the object disappears between HEAD and GET. No glob interpretation.
  std::string bytes;
  {
    Operation operation(*this, cancellation);
    const auto filter = InputBytes(object.key + "\n");
    bytes = Execute(operation, ReadArguments(kReadCommand, object), filter.get(), *size);
  }
  if (bytes.empty() && !Head(object, cancellation))
    return std::nullopt;
  if (bytes.size() != *size)
    throw S3ResponseError("S3 object size changed or exceeds limit");
  return bytes;
}

void
S3Client::Put(const S3ObjectLocation& object, const std::string& bytes, std::size_t max_bytes,
              Cancellation cancellation)
{
  object.Validate();
  if (bytes.size() > max_bytes || bytes.size() > limits_.part_size)
    throw std::invalid_argument("metadata exceeds size limit or single-PUT threshold");
  Operation operation(*this, cancellation);
  const auto source = InputBytes(bytes);
  Execute(operation, CopyArguments(object, true), source.get(), 0);
}
} // namespace snapshot::pagebroker
