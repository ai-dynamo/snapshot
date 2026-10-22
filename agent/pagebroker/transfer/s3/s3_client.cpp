// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_client.hpp"

#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>

#include <aws/core/Aws.h>
#include <aws/io/io.h>
#include <aws/core/auth/AWSCredentialsProvider.h>
#include <aws/core/auth/AWSCredentialsProviderChain.h>
#include <aws/core/client/DefaultRetryStrategy.h>
#include <aws/core/config/ConfigAndCredentialsCacheManager.h>
#include <aws/s3-crt/S3CrtClient.h>
#include <aws/s3-crt/model/GetObjectRequest.h>
#include <aws/s3-crt/model/HeadObjectRequest.h>
#include <aws/s3-crt/model/PutObjectRequest.h>

#include <algorithm>
#include <cctype>
#include <cmath>
#include <cstdlib>
#include <fstream>
#include <future>
#include <cerrno>
#include <condition_variable>
#include <exception>
#include <limits>
#include <mutex>
#include <sstream>
#include <system_error>
#include <utility>
#include <vector>

#include "file_descriptor.hpp"

namespace snapshot::pagebroker {
namespace {
using Clock = std::chrono::steady_clock;
constexpr std::uint64_t kMiB = 1024 * 1024;
constexpr char kAllocationTag[] = "PageBrokerS3";

class SdkLease {
 public:
  SdkLease() { UpdateUsers(true); }
  ~SdkLease() { UpdateUsers(false); }
  SdkLease(const SdkLease&) = delete;
  SdkLease& operator=(const SdkLease&) = delete;

 private:
  static void UpdateUsers(bool acquire)
  {
    static std::mutex mutex;
    static unsigned users = 0;
    static Aws::SDKOptions options;
    std::lock_guard lock(mutex);
    // Model Streamer's hidden SDK copy manages its own lifetime.
    if (acquire) {
      if (users == 0)
        Aws::InitAPI(options);
      ++users;
    } else if (--users == 0)
      Aws::ShutdownAPI(options);
  }
};

class BoundedBuffer : public std::streambuf {
 public:
  explicit BoundedBuffer(std::size_t limit) : bytes_(limit + 1) { Reset(); }
  void Reset() { setp(bytes_.data(), bytes_.data() + bytes_.size()); }
  std::string Bytes() const { return {pbase(), pptr()}; }

 private:
  std::vector<char> bytes_;
};

bool
HasInvalidText(const std::string& value)
{
  if (value.find('\0') != std::string::npos)
    return true;
  return value.find_first_of("\n\r") != std::string::npos;
}

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
  return bucket.find("..") == std::string::npos && !bucket.ends_with("--x-s3");
}

bool
HasCompleteCredentials(const S3Connection& connection)
{
  if (connection.access_key_id.empty() != connection.secret_access_key.empty())
    return false;
  return connection.session_token.empty() || !connection.access_key_id.empty();
}

bool
HasSupportedEndpoint(const std::string& endpoint)
{
  if (endpoint.empty())
    return true;
  return endpoint.starts_with("https://") || endpoint.starts_with("http://");
}

bool
HasValidResourceLimits(const S3Limits& limits)
{
  if (limits.part_size < 8 * kMiB || limits.part_size > 5 * 1024 * kMiB)
    return false;
  if (limits.buffer_budget < 1024 * kMiB)
    return false;
  if (limits.buffer_budget < 2 * limits.part_size)
    return false;
  if (limits.buffer_budget > std::numeric_limits<std::size_t>::max())
    return false;
  if (limits.active_files == 0)
    return false;
  if (limits.request_retries > 10)
    return false;
  return std::isfinite(limits.target_gbps) && limits.target_gbps > 0 && limits.target_gbps <= 1000;
}

bool
IsValidTimeout(std::chrono::milliseconds timeout)
{
  if (timeout.count() <= 0 || timeout > std::chrono::hours(24))
    return false;
  return timeout.count() <= std::numeric_limits<long>::max();
}

bool
FitsMultipartLimits(std::uint64_t size, std::uint64_t part_size)
{
  if (size > static_cast<std::uint64_t>(std::numeric_limits<off_t>::max()))
    return false;
  return size / part_size + (size % part_size != 0) <= 10'000;
}

bool
HasConsistentObjectSize(std::size_t actual, std::uint64_t expected, std::size_t maximum, long long declared)
{
  if (actual != expected || actual > maximum)
    return false;
  return declared == static_cast<long long>(actual);
}

void
ValidateText(const std::string& value)
{
  if (HasInvalidText(value))
    throw std::invalid_argument("S3 settings cannot contain NUL or line breaks");
}

struct Operation {
  Operation(const char* name, std::chrono::milliseconds timeout, Cancellation cancellation = {})
      : name(name), cancellation(cancellation.WithDeadline(Clock::now() + timeout)) {}

  bool Continue() const noexcept
  {
    return !cancellation.IsCancelled();
  }

  void Check() const
  {
    if (const auto reason = cancellation.Reason())
      throw S3Error({name, *reason == CancellationError::Reason::DEADLINE_EXCEEDED ? kS3DeadlineExceededCode : kS3CancelledCode});
  }

  const std::string name;
  const Cancellation cancellation;
};

// A lease bounds admitted file transfers and metadata operations, not CRT parts.
class Slots {
 public:
  struct Lease {
    Lease(Slots& pool, std::size_t slot) : pool(&pool), slot(slot) {}
    Lease(Lease&& other) noexcept : pool(std::exchange(other.pool, nullptr)), slot(other.slot) {}
    Lease(const Lease&) = delete;
    ~Lease()
    {
      if (pool) {
        std::lock_guard lock(pool->mutex_);
        pool->available_.push_back(slot);
        pool->changed_.notify_all();
      }
    }
    Slots* pool;
    std::size_t slot;
  };

  explicit Slots(std::size_t count)
  {
    available_.reserve(count);
    for (std::size_t i = 0; i < count; ++i)
      available_.push_back(i);
  }

  Lease Acquire(const Operation& operation)
  {
    std::unique_lock lock(mutex_);
    changed_.wait_until(lock, operation.cancellation.Token(), operation.cancellation.Deadline(),
                        [&] { return !available_.empty() || !operation.Continue(); });
    operation.Check();
    const auto slot = available_.back();
    available_.pop_back();
    return {*this, slot};
  }

 private:
  std::mutex mutex_;
  std::condition_variable_any changed_;
  std::vector<std::size_t> available_;
};

template<class Error>
S3RequestFailure
RequestFailure(const char* operation, const Error& error)
{
  // SDK/server messages can echo caller inputs; retain structured diagnostics
  // instead of logging response bodies, authorization headers, or credentials.
  std::string code = error.GetExceptionName().c_str();
  if (code.empty())
    code = "SDKError(" + std::to_string(static_cast<int>(error.GetErrorType())) + ")";
  return {operation, std::move(code), static_cast<int>(error.GetResponseCode()), error.GetRequestId().c_str()};
}

[[noreturn]] void
RethrowUploadError(std::exception_ptr failure)
{
  try {
    std::rethrow_exception(failure);
  }
  catch (const S3Error&) {
    throw;
  }
  catch (...) {
    std::throw_with_nested(S3Error({"upload", "LocalFailure"}));
  }
}

bool
SameTimestamp(const timespec& left, const timespec& right)
{
  return left.tv_sec == right.tv_sec && left.tv_nsec == right.tv_nsec;
}

bool
SourceUnchanged(const struct stat& current, const struct stat& original)
{
  if (current.st_size != original.st_size)
    return false;
  if (!SameTimestamp(current.st_mtim, original.st_mtim))
    return false;
  return SameTimestamp(current.st_ctim, original.st_ctim);
}

void
CheckSource(int descriptor, const struct stat& original)
{
  struct stat current;
  if (fstat(descriptor, &current) < 0)
    throw std::system_error(errno, std::generic_category(), "stat S3 upload source");
  if (!SourceUnchanged(current, original))
    throw S3Error({"read source", "SourceChanged"});
}
// Adapted from runai-model-streamer cpp/s3/client/client.cc (ae93548):
// keep the endpoint scheme separate, and use the same AWS credential constructors.
bool
StartsWithCaseInsensitive(const Aws::String& value, const Aws::String& prefix)
{
  if (value.size() < prefix.size())
    return false;
  for (std::size_t i = 0; i < prefix.size(); ++i) {
    if (std::tolower(static_cast<unsigned char>(value[i])) !=
        std::tolower(static_cast<unsigned char>(prefix[i])))
      return false;
  }
  return true;
}

Aws::S3Crt::S3CrtClientConfiguration
ClientConfiguration(const S3Connection& connection, const S3Limits& limits)
{
  Aws::S3Crt::S3CrtClientConfiguration config(Aws::Client::ClientConfigurationInitValues{
      !connection.region.empty() && !connection.access_key_id.empty()});
  if (!connection.region.empty())
    config.region = connection.region;
  config.endpointOverride = connection.endpoint;
  for (const auto& prefix : {Aws::String("https://"), Aws::String("http://")}) {
    if (StartsWithCaseInsensitive(config.endpointOverride, prefix)) {
      config.endpointOverride.erase(0, prefix.size());
      config.scheme = prefix == "https://" ? Aws::Http::Scheme::HTTPS : Aws::Http::Scheme::HTTP;
      if (config.endpointOverride.empty() || config.endpointOverride.front() == ':')
        throw std::invalid_argument("S3 endpoint has no host");
      break;
    }
  }
  config.useVirtualAddressing = connection.use_virtual_addressing;
  config.caFile = connection.ca_file;
  if (config.caFile.empty()) {
    if (const auto* ca = std::getenv(kAwsCaBundleEnv))
      config.caFile = ca;
    else
      config.caFile = Aws::Config::GetCachedConfigValue(Aws::Auth::GetConfigProfileName(), "ca_bundle");
  }
  if (!config.caFile.empty() && !std::filesystem::is_regular_file(config.caFile))
    throw std::invalid_argument("S3 CA certificate file does not exist");
  config.verifySSL = true;
  config.partSize = limits.part_size;
  config.multipartUploadThreshold = limits.part_size;
  config.memoryLimitBytes = limits.buffer_budget;
  config.throughputTargetGbps = limits.target_gbps;
  config.connectTimeoutMs = limits.connect_timeout.count();
  // HeadObject uses the SDK HTTP client, while GET/PUT use CRT. Bound both
  // retry paths so metadata failures honor the same per-request policy.
  config.retryStrategy = std::make_shared<Aws::Client::DefaultRetryStrategy>(limits.request_retries);
  config.httpRequestTimeoutMs = limits.request_timeout.count();
  // Like Model Streamer, requestTimeoutMs is a low-speed interval, not a
  // total operation deadline. CRT imposes a minimum three-second interval.
  config.requestTimeoutMs = limits.request_timeout.count();
  config.lowSpeedLimit = 1;
  using Retry = Aws::S3Crt::S3CrtClientConfiguration::CrtRetryStrategyConfig::CrtRetryStrategyType;
  config.crtRetryStrategyConfig.crtRetryStrategyType = Retry::EXPONENTIAL_BACKOFF;
  config.crtRetryStrategyConfig.config.maxRetries = std::max(1U, limits.request_retries);
  if (limits.request_retries == 0) {
    config.crtConfigFactories.retryStrategyCreateFn = [](const auto& settings) {
      auto* strategy = Aws::S3Crt::S3CrtClientConfiguration::S3CrtConfigFactories::defaultFactories.retryStrategyCreateFn(settings);
      if (!strategy)
        throw std::runtime_error("cannot create S3 retry strategy");
      // The pinned CRT's NO_RETRY denies the initial token too, so no request
      // reaches S3. Retain CRT's asynchronous admission and token lifetime,
      // overriding only the public retry hook to refuse subsequent attempts.
      static auto no_retries = [strategy] {
        auto callbacks = *strategy->vtable;
        callbacks.schedule_retry = [](aws_retry_token*, aws_retry_error_type,
                                      aws_retry_strategy_on_retry_ready_fn*, void*) {
          return aws_raise_error(AWS_IO_MAX_RETRIES_EXCEEDED);
        };
        return callbacks;
      }();
      strategy->vtable = &no_retries;
      return strategy;
    };
  }
  return config;
}

}  // namespace

S3Connection
ResolveS3Credentials(S3Connection connection)
{
  connection.Validate();
  if (!connection.access_key_id.empty())
    return connection;
  SdkLease sdk;
  Aws::Auth::DefaultAWSCredentialsProviderChain providers;
  const auto credentials = providers.GetAWSCredentials();
  connection.access_key_id = credentials.GetAWSAccessKeyId().c_str();
  connection.secret_access_key = credentials.GetAWSSecretKey().c_str();
  connection.session_token = credentials.GetSessionToken().c_str();
  if (connection.access_key_id.empty() || connection.secret_access_key.empty())
    throw std::invalid_argument("external S3 credentials are missing");
  connection.Validate();
  return connection;
}

void
S3ObjectLocation::Validate() const
{
  ValidateText(bucket);
  ValidateText(key);
  if (!IsSupportedObjectLocation(*this))
    throw std::invalid_argument("S3 upload requires an ordinary bucket name and a supported nonempty key");
}

void
S3Connection::Validate() const
{
  for (const auto* value : {&region, &endpoint, &ca_file, &access_key_id, &secret_access_key, &session_token})
    ValidateText(*value);
  if (!HasCompleteCredentials(*this))
    throw std::invalid_argument("S3 explicit credentials require an access key and secret key");
  if (!HasSupportedEndpoint(endpoint))
    throw std::invalid_argument("S3 endpoint requires an http or https scheme");
}

void
S3Limits::Validate() const
{
  if (!HasValidResourceLimits(*this))
    throw std::invalid_argument("invalid S3 upload resource limits");
  for (auto timeout : {connect_timeout, request_timeout, operation_timeout}) {
    if (!IsValidTimeout(timeout))
      throw std::invalid_argument("S3 timeouts must be positive and at most 24 hours");
  }
}

void
S3Limits::ValidateFileSize(std::uint64_t size) const
{
  Validate();
  if (!FitsMultipartLimits(size, part_size))
    throw std::invalid_argument("S3 file exceeds the configured 10000-part limit");
}

S3Error::S3Error(S3RequestFailure failure)
    : std::runtime_error(failure.operation + ": " + failure.code), primary(std::move(failure))
{
}

struct S3Client::Impl {
  Impl(const S3Connection& connection, S3Limits limits)
      : limits(limits), files(limits.active_files)
  {
    const auto config = ClientConfiguration(connection, limits);
    if (connection.access_key_id.empty())
      client = std::make_unique<Aws::S3Crt::S3CrtClient>(config);
    else
      client = std::make_unique<Aws::S3Crt::S3CrtClient>(
          Aws::Auth::AWSCredentials(connection.access_key_id, connection.secret_access_key, connection.session_token), config);
  }

  template<class Request, class Call>
  auto MetadataRequest(Request& request, const S3ObjectLocation& object, Cancellation cancellation, Call call)
  {
    object.Validate();
    request.WithBucket(object.bucket).WithKey(object.key);
    auto operation = std::make_shared<Operation>(request.GetServiceRequestName(), limits.operation_timeout, cancellation);
    return RequestResult(request, operation, call);
  }

  template<class Request, class Call>
  auto RequestResult(Request& request, const std::shared_ptr<Operation>& operation, Call call)
  {
    auto slot = files.Acquire(*operation);
    operation->Check();
    request.SetContinueRequestHandler([operation](const Aws::Http::HttpRequest*) { return operation->Continue(); });
    auto outcome = call(request);
    operation->Check();
    return outcome;
  }

  Aws::S3Crt::Model::PutObjectOutcome Upload(Aws::S3Crt::Model::PutObjectRequest& request,
                                           const std::shared_ptr<Operation>& operation)
  {
    request.SetContinueRequestHandler([operation](const Aws::Http::HttpRequest*) { return operation->Continue(); });
    auto completion = std::make_shared<std::promise<Aws::S3Crt::Model::PutObjectOutcome>>();
    auto finished = completion->get_future();
    client->PutObjectAsync(request, [completion](const auto*, const auto&, const auto& outcome, const auto&) {
      completion->set_value(outcome);
    });
    // The pinned SDK invokes this completion from its request shutdown callback.
    // Cancellation/deadline callbacks stop work; draining still owns the stream.
    // A stalled connection is interrupted by CRT's low-speed timeout.
    return finished.get();
  }

  S3UploadResult UploadFile(const std::filesystem::path& path, const S3ObjectLocation& destination,
                            Cancellation cancellation)
  {
    destination.Validate();
    if (path.native().find('\0') != std::string::npos)
      throw std::invalid_argument("S3 source path cannot contain NUL");
    auto operation = std::make_shared<Operation>("PutObject", limits.operation_timeout, cancellation);
    auto admitted = files.Acquire(*operation);
    FileDescriptor source(open(path.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK));
    if (source.get() < 0)
      throw std::system_error(errno, std::generic_category(), "open S3 upload source");
    struct stat status;
    if (fstat(source.get(), &status) < 0)
      throw std::system_error(errno, std::generic_category(), "stat S3 upload source");
    if (!S_ISREG(status.st_mode) || status.st_size < 0)
      throw std::invalid_argument("S3 upload source must be a regular file");
    const auto size = static_cast<std::uint64_t>(status.st_size);
    limits.ValidateFileSize(size);
    CheckSource(source.get(), status);
    operation->Check();
    auto remote_state = S3RemoteState::NO_WRITES_ISSUED;
    try {
      // Open the already-validated inode, not the caller's path a second time.
      // A seekable stream lets CRT rewind retries without retaining the whole file.
      auto body = Aws::MakeShared<Aws::FStream>(kAllocationTag,
          ("/proc/self/fd/" + std::to_string(source.get())).c_str(), std::ios::in | std::ios::binary);
      if (!*body)
        throw S3Error({"read source", "OpenFailed"});
      Aws::S3Crt::Model::PutObjectRequest request;
      request.WithBucket(destination.bucket).WithKey(destination.key).WithContentLength(size);
      request.SetContentType("application/octet-stream");
      request.SetBody(body);
      operation->Check();
      remote_state = S3RemoteState::UNKNOWN;
      const auto outcome = Upload(request, operation);
      if (outcome.IsSuccess())
        remote_state = S3RemoteState::COMPLETED;
      operation->Check();
      CheckSource(source.get(), status);
      if (!outcome.IsSuccess())
        throw S3Error(RequestFailure("PutObject", outcome.GetError()));
      return {destination, size};
    }
    catch (...) {
      try {
        RethrowUploadError(std::current_exception());
      }
      catch (S3Error& error) {
        error.destination = destination;
        error.remote_state = remote_state;
        // CRT owns the multipart ID and aborts on failure. Its public outcome
        // does not verify cleanup, including lost initiation/completion replies.
        if (remote_state == S3RemoteState::UNKNOWN && size > limits.part_size)
          error.cleanup_state = S3CleanupState::UNCONFIRMED;
        throw;
      }
    }
  }

  // Declaration order ensures all SDK-owned objects die before ShutdownAPI.
  SdkLease sdk;
  const S3Limits limits;
  Slots files;
  std::unique_ptr<Aws::S3Crt::S3CrtClient> client;
};

S3Client::S3Client(S3Connection connection, S3Limits limits)
{
  connection.Validate();
  limits.Validate();
  impl_ = std::make_unique<Impl>(connection, limits);
}

S3Client::~S3Client() noexcept = default;

S3UploadResult
S3Client::UploadFile(const std::filesystem::path& source, const S3ObjectLocation& destination,
                       Cancellation cancellation)
{
  return impl_->UploadFile(source, destination, cancellation);
}

std::optional<std::uint64_t>
S3Client::Head(const S3ObjectLocation& object, Cancellation cancellation)
{
  Aws::S3Crt::Model::HeadObjectRequest request;
  const auto result = impl_->MetadataRequest(request, object, cancellation, [&](const auto& value) { return impl_->client->HeadObject(value); });
  if (!result.IsSuccess()) {
    if (result.GetError().GetResponseCode() == Aws::Http::HttpResponseCode::NOT_FOUND)
      return std::nullopt;
    throw S3Error(RequestFailure("HeadObject", result.GetError()));
  }
  if (result.GetResult().GetContentLength() < 0)
    throw S3ResponseError("negative S3 object size");
  return result.GetResult().GetContentLength();
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
  // CRT issues a ranged GET, which is unsatisfiable for a zero-length object.
  if (*size == 0)
    return std::string{};
  BoundedBuffer buffer(*size);
  Aws::S3Crt::Model::GetObjectRequest request;
  request.SetResponseStreamFactory([&buffer] {
    buffer.Reset();
    return Aws::New<Aws::IOStream>(kAllocationTag, &buffer);
  });
  auto result = impl_->MetadataRequest(request, object, cancellation, [&](const auto& value) { return impl_->client->GetObject(value); });
  if (!result.IsSuccess()) {
    if (result.GetError().GetResponseCode() == Aws::Http::HttpResponseCode::NOT_FOUND)
      return std::nullopt;
    throw S3Error(RequestFailure("GetObject", result.GetError()));
  }
  auto bytes = buffer.Bytes();
  if (!HasConsistentObjectSize(bytes.size(), *size, max_bytes, result.GetResult().GetContentLength()))
    throw S3ResponseError("S3 object size changed or exceeds limit");
  return bytes;
}

void
S3Client::Put(const S3ObjectLocation& object, const std::string& bytes, std::size_t max_bytes, Cancellation cancellation)
{
  if (bytes.size() > max_bytes || bytes.size() > impl_->limits.part_size)
    throw std::invalid_argument("metadata exceeds size limit or single-PUT threshold");
  Aws::S3Crt::Model::PutObjectRequest request;
  request.WithIfNoneMatch("*");
  request.SetContentLength(bytes.size());
  request.SetContentType("application/json");
  auto body = Aws::MakeShared<Aws::StringStream>(kAllocationTag);
  body->write(bytes.data(), bytes.size());
  request.SetBody(body);
  auto remote_state = S3RemoteState::NO_WRITES_ISSUED;
  try {
    const auto result = impl_->MetadataRequest(request, object, cancellation, [&](const auto& value) {
      remote_state = S3RemoteState::UNKNOWN;
      auto outcome = impl_->client->PutObject(value);
      if (outcome.IsSuccess())
        remote_state = S3RemoteState::COMPLETED;
      return outcome;
    });
    if (!result.IsSuccess())
      throw S3Error(RequestFailure("PutObject", result.GetError()));
  }
  catch (S3Error& error) {
    error.destination = object;
    error.remote_state = remote_state;
    throw;
  }
}

}  // namespace snapshot::pagebroker
