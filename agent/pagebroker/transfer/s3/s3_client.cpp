// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_client.hpp"

#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>

#include <aws/core/Aws.h>
#include <aws/core/auth/AWSCredentialsProvider.h>
#include <aws/core/auth/AWSCredentialsProviderChain.h>
#include <aws/core/client/DefaultRetryStrategy.h>
#include <aws/core/utils/stream/PreallocatedStreamBuf.h>
#include <aws/core/utils/threading/PooledThreadExecutor.h>
#include <aws/s3/S3Client.h>
#include <aws/s3/model/AbortMultipartUploadRequest.h>
#include <aws/s3/model/CompleteMultipartUploadRequest.h>
#include <aws/s3/model/CreateMultipartUploadRequest.h>
#include <aws/s3/model/GetObjectRequest.h>
#include <aws/s3/model/HeadObjectRequest.h>
#include <aws/s3/model/ListPartsRequest.h>
#include <aws/s3/model/PutObjectRequest.h>
#include <aws/s3/model/UploadPartRequest.h>

#include <algorithm>
#include <atomic>
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
  if (limits.part_size < 5 * kMiB || limits.part_size > 5 * 1024 * kMiB)
    return false;
  if (limits.buffer_budget < limits.part_size || limits.buffer_budget % limits.part_size != 0)
    return false;
  if (limits.buffer_budget > std::numeric_limits<std::size_t>::max())
    return false;
  if (limits.workers == 0 || limits.active_files == 0)
    return false;
  if (limits.request_retries > 10)
    return false;
  return limits.cleanup_rounds > 0 && limits.cleanup_rounds <= 10;
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
IsMultipartCleanupComplete(const Aws::S3::Model::ListPartsOutcome& outcome)
{
  if (!outcome.IsSuccess())
    return outcome.GetError().GetErrorType() == Aws::S3::S3Errors::NO_SUCH_UPLOAD;
  return outcome.GetResult().GetParts().empty() && !outcome.GetResult().GetIsTruncated();
}

bool
HasUntrackedMultipartUpload(S3RemoteState state, const std::string& upload_id,
    std::uint64_t size, const S3Limits& limits)
{
  if (state != S3RemoteState::UNKNOWN)
    return false;
  return upload_id.empty() && size > limits.part_size;
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
  explicit Operation(std::chrono::milliseconds timeout, TransferControl control = {})
      : deadline(std::min(Clock::now() + timeout, control.deadline)), cancellation(control.cancellation) {}

  bool Continue() const noexcept
  {
    return !cancelled.load() && !cancellation.stop_requested() && Clock::now() < deadline;
  }

  void Check() const
  {
    if (Clock::now() >= deadline)
      throw S3Error({"upload", "DeadlineExceeded"});
    if (cancelled.load() || cancellation.stop_requested())
      throw S3Error({"upload", "Cancelled"});
  }

  void Fail(std::exception_ptr error) noexcept
  {
    std::lock_guard lock(mutex);
    if (!failure)
      failure = error;
    cancelled = true;
  }

  void Finish() noexcept
  {
    std::lock_guard lock(mutex);
    --pending;
    changed.notify_all();
  }

  void Drain()
  {
    std::unique_lock lock(mutex);
    if (!changed.wait_until(lock, deadline, [&] { return pending == 0; })) {
      if (!failure)
        failure = std::make_exception_ptr(S3Error({"upload", "DeadlineExceeded"}));
      cancelled = true;
      // A timeout never permits freeing request bodies still used by the SDK.
      changed.wait(lock, [&] { return pending == 0; });
    }
    if (failure)
      std::rethrow_exception(failure);
  }

  const Clock::time_point deadline;
  const std::stop_token cancellation;
  std::atomic<bool> cancelled = false;
  std::mutex mutex;
  std::condition_variable changed;
  std::size_t pending = 0;
  std::exception_ptr failure;
};

// A lease bounds admission, HTTP requests, or a preallocated upload buffer.
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
    changed_.wait_until(lock, operation.cancellation, operation.deadline, [&] { return !available_.empty() || !operation.Continue(); });
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
  return {operation, error.GetExceptionName().c_str(), static_cast<int>(error.GetResponseCode()),
          error.GetRequestId().c_str()};
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

void
ReadPart(int descriptor, unsigned char* buffer, std::size_t size, off_t offset, const Operation& operation)
{
  std::size_t read = 0;
  while (read < size) {
    operation.Check();
    const auto count = pread(descriptor, buffer + read, size - read, offset + read);
    if (count < 0) {
      if (errno == EINTR)
        continue;
      throw std::system_error(errno, std::generic_category(), "read S3 upload source");
    }
    if (count == 0)
      throw S3Error({"read source", "SourceShortened"});
    read += count;
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
      : connection(connection), limits(limits), files(limits.active_files), requests(limits.workers),
        buffer_slots(limits.buffer_budget / limits.part_size)
  {
    executor = std::make_shared<Aws::Utils::Threading::PooledThreadExecutor>(limits.workers);
    // Explicit region and credentials need no metadata discovery, including
    // during configuration construction before we assign the region below.
    Aws::S3::S3ClientConfiguration config(Aws::Client::ClientConfigurationInitValues{
        !connection.region.empty() && !connection.access_key_id.empty()});
    if (!connection.region.empty())
      config.region = connection.region;
    config.endpointOverride = connection.endpoint;
    config.useVirtualAddressing = connection.use_virtual_addressing;
    config.caFile = connection.ca_file;
    config.verifySSL = true;
    config.maxConnections = limits.workers;
    config.connectTimeoutMs = limits.connect_timeout.count();
    config.httpRequestTimeoutMs = limits.request_timeout.count();
    config.requestTimeoutMs = limits.request_timeout.count();
    config.executor = executor;
    config.retryStrategy = std::make_shared<Aws::Client::DefaultRetryStrategy>(limits.request_retries);
    auto make_client = [&] {
      if (connection.access_key_id.empty())
        return std::make_unique<Aws::S3::S3Client>(config);
      return std::make_unique<Aws::S3::S3Client>(
          Aws::Auth::AWSCredentials(connection.access_key_id, connection.secret_access_key, connection.session_token), nullptr, config);
    };
    client = make_client();
    // Retrying initiation after a lost response can create an upload whose ID
    // we never learn. Do not retry this non-idempotent operation automatically.
    config.retryStrategy = std::make_shared<Aws::Client::DefaultRetryStrategy>(0);
    initiation_client = make_client();
  }

  ~Impl()
  {
    executor->WaitUntilStopped();
  }

  void AllocateBuffers(const Operation& operation)
  {
    std::call_once(buffers_once, [&] {
      std::vector<std::vector<unsigned char>> allocated;
      for (std::uint64_t i = 0; i < limits.buffer_budget / limits.part_size; ++i) {
        operation.Check();
        allocated.emplace_back(limits.part_size);
      }
      buffers = std::move(allocated);
    });
    operation.Check();
  }

  template<class Request, class Call>
  auto MetadataRequest(Request& request, const S3ObjectLocation& object, TransferControl control, Call call)
  {
    object.Validate();
    control.Check();
    request.WithBucket(object.bucket).WithKey(object.key);
    auto operation = std::make_shared<Operation>(limits.operation_timeout, control);
    return RequestResult(request, operation, call);
  }

  template<class Request, class Call>
  auto RequestResult(Request& request, const std::shared_ptr<Operation>& operation, Call call)
  {
    auto slot = requests.Acquire(*operation);
    operation->Check();
    request.SetContinueRequestHandler([operation](const Aws::Http::HttpRequest*) { return operation->Continue(); });
    return call(request);
  }

  template<class Request, class Call>
  void SendBody(Request& request, const std::shared_ptr<Operation>& operation,
                unsigned char* bytes, std::size_t size, const char* name, Call call)
  {
    Aws::Utils::Stream::PreallocatedStreamBuf stream_buffer(bytes, size);
    auto body = Aws::MakeShared<Aws::IOStream>(kAllocationTag, &stream_buffer);
    request.SetBody(body);
    request.SetContentLength(size);
    request.SetContentType("application/octet-stream");
    // The synchronous SDK call (including retry rewinds) finishes before either
    // the stream buffer or the leased bytes can be released.
    const auto outcome = RequestResult(request, operation, call);
    request.SetBody(nullptr);
    if (!outcome.IsSuccess())
      throw S3Error(RequestFailure(name, outcome.GetError()));
  }

  void Cleanup(S3Error& error) noexcept
  {
    if (error.upload_id.empty() || error.remote_state == S3RemoteState::COMPLETED)
      return;
    error.cleanup_state = S3CleanupState::UNCONFIRMED;
    try {
      auto cleanup = std::make_shared<Operation>(std::min(limits.operation_timeout, limits.request_timeout * (2 * limits.cleanup_rounds + 1)));
      for (unsigned round = 0; round < limits.cleanup_rounds; ++round) {
        Aws::S3::Model::AbortMultipartUploadRequest abort;
        abort.WithBucket(error.destination.bucket).WithKey(error.destination.key).WithUploadId(error.upload_id);
        auto aborted = RequestResult(abort, cleanup, [&](const auto& request) { return client->AbortMultipartUpload(request); });
        if (!aborted.IsSuccess() && aborted.GetError().GetErrorType() != Aws::S3::S3Errors::NO_SUCH_UPLOAD) {
          error.cleanup_failure = RequestFailure("AbortMultipartUpload", aborted.GetError());
          continue;
        }
        Aws::S3::Model::ListPartsRequest list;
        list.WithBucket(error.destination.bucket).WithKey(error.destination.key).WithUploadId(error.upload_id).WithMaxParts(1);
        auto listed = RequestResult(list, cleanup, [&](const auto& request) { return client->ListParts(request); });
        if (IsMultipartCleanupComplete(listed)) {
          error.cleanup_state = S3CleanupState::CONFIRMED;
          error.cleanup_failure.reset();
          return;
        }
        error.cleanup_failure = listed.IsSuccess() ? S3RequestFailure{"ListParts", "PartsRemain"} :
                                                    RequestFailure("ListParts", listed.GetError());
      }
    }
    catch (const S3Error& failure) {
      try {
        error.cleanup_failure = failure.primary;
      }
      catch (...) {
      }
    }
    catch (...) {
      // Keep the primary error even if reporting cleanup itself cannot allocate.
    }
  }

  void UploadParts(int source, std::uint64_t size, const S3ObjectLocation& destination,
                   const std::string& upload_id, const std::shared_ptr<Operation>& operation,
                   std::vector<Aws::S3::Model::CompletedPart>& completed)
  {
    for (std::size_t part = 0; part < completed.size(); ++part) {
      auto buffer = std::make_shared<Slots::Lease>(buffer_slots.Acquire(*operation));
      const auto offset = part * limits.part_size;
      const auto length = std::min(limits.part_size, size - offset);
      ReadPart(source, buffers[buffer->slot].data(), length, offset, *operation);
      {
        std::lock_guard lock(operation->mutex);
        ++operation->pending;
      }
      try {
        const bool submitted = executor->Submit([this, operation, buffer, destination, upload_id, part, length, &completed]() mutable {
          try {
            Aws::S3::Model::UploadPartRequest request;
            request.WithBucket(destination.bucket).WithKey(destination.key).WithUploadId(upload_id).WithPartNumber(part + 1);
            SendBody(request, operation, buffers[buffer->slot].data(), length, "UploadPart", [&](const auto& value) {
              auto outcome = client->UploadPart(value);
              if (outcome.IsSuccess())
                completed[part].WithPartNumber(part + 1).WithETag(outcome.GetResult().GetETag());
              return outcome;
            });
          }
          catch (...) {
            operation->Fail(std::current_exception());
          }
          buffer.reset();
          operation->Finish();
        });
        if (!submitted)
          throw S3Error({"schedule part", "ExecutorStopped"});
      }
      catch (...) {
        operation->Finish();
        throw;
      }
    }
  }

  void UploadMultipart(int source, const struct stat& status, const S3ObjectLocation& destination,
                       const std::shared_ptr<Operation>& operation,
                       std::string& upload_id, S3RemoteState& remote_state)
  {
    const auto size = static_cast<std::uint64_t>(status.st_size);
    std::vector<Aws::S3::Model::CompletedPart> completed(size / limits.part_size + (size % limits.part_size != 0));
    try {
      Aws::S3::Model::CreateMultipartUploadRequest create;
      create.WithBucket(destination.bucket).WithKey(destination.key).WithContentType("application/octet-stream");
      remote_state = S3RemoteState::UNKNOWN;
      auto started = RequestResult(create, operation, [&](const auto& request) { return initiation_client->CreateMultipartUpload(request); });
      if (!started.IsSuccess())
        throw S3Error(RequestFailure("CreateMultipartUpload", started.GetError()));
      upload_id = started.GetResult().GetUploadId();
      if (upload_id.empty())
        throw S3Error({"CreateMultipartUpload", "MissingUploadId"});
      remote_state = S3RemoteState::NOT_COMPLETED;
      UploadParts(source, size, destination, upload_id, operation, completed);
      operation->Drain();
      CheckSource(source, status);
      Aws::S3::Model::CompletedMultipartUpload parts;
      for (const auto& part : completed)
        parts.AddParts(part);
      Aws::S3::Model::CompleteMultipartUploadRequest complete;
      complete.WithBucket(destination.bucket).WithKey(destination.key).WithUploadId(upload_id).WithMultipartUpload(parts);
      operation->Check();
      remote_state = S3RemoteState::UNKNOWN;
      auto finished = RequestResult(complete, operation, [&](const auto& request) { return client->CompleteMultipartUpload(request); });
      if (!finished.IsSuccess())
        throw S3Error(RequestFailure("CompleteMultipartUpload", finished.GetError()));
    }
    catch (...) {
      // Workers reference completed until drained, including failures while scheduling parts.
      operation->Fail(std::current_exception());
      operation->Drain();
      throw;
    }
  }

  S3UploadResult UploadFile(const std::filesystem::path& path, const S3ObjectLocation& destination,
                            const S3UploadOptions& options)
  {
    destination.Validate();
    if (path.native().find('\0') != std::string::npos)
      throw std::invalid_argument("S3 source path cannot contain NUL");
    auto operation = std::make_shared<Operation>(limits.operation_timeout, options.control);
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
    const auto digest = utils::ComputeFileSha256(source.get(), [&] { operation->Check(); });
    CheckSource(source.get(), status);
    operation->Check();
    AllocateBuffers(*operation);
    std::string upload_id;
    auto remote_state = S3RemoteState::NO_WRITES_ISSUED;
    try {
      if (size <= limits.part_size) {
        auto buffer = buffer_slots.Acquire(*operation);
        ReadPart(source.get(), buffers[buffer.slot].data(), size, 0, *operation);
        Aws::S3::Model::PutObjectRequest request;
        request.WithBucket(destination.bucket).WithKey(destination.key);
        remote_state = S3RemoteState::UNKNOWN;
        SendBody(request, operation, buffers[buffer.slot].data(), size, "PutObject",
                 [&](const auto& value) { return client->PutObject(value); });
      } else {
        UploadMultipart(source.get(), status, destination, operation, upload_id, remote_state);
      }
      remote_state = S3RemoteState::COMPLETED;
      CheckSource(source.get(), status);
      return {destination, size, digest};
    }
    catch (...) {
      try {
        RethrowUploadError(std::current_exception());
      }
      catch (S3Error& error) {
        error.destination = destination;
        error.remote_state = remote_state;
        error.upload_id = upload_id;
        if (HasUntrackedMultipartUpload(remote_state, upload_id, size, limits))
          error.cleanup_state = S3CleanupState::UNCONFIRMED;
        Cleanup(error);
        throw;
      }
    }
  }

  // Declaration order ensures all SDK-owned objects die before ShutdownAPI.
  SdkLease sdk;
  const S3Connection connection;
  const S3Limits limits;
  Slots files;
  Slots requests;
  Slots buffer_slots;
  std::once_flag buffers_once;
  std::vector<std::vector<unsigned char>> buffers;
  std::shared_ptr<Aws::Utils::Threading::PooledThreadExecutor> executor;
  std::unique_ptr<Aws::S3::S3Client> client;
  std::unique_ptr<Aws::S3::S3Client> initiation_client;
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
                       const S3UploadOptions& options)
{
  return impl_->UploadFile(source, destination, options);
}

std::optional<std::uint64_t>
S3Client::Head(const S3ObjectLocation& object, TransferControl control)
{
  Aws::S3::Model::HeadObjectRequest request;
  const auto result = impl_->MetadataRequest(request, object, control, [&](const auto& value) { return impl_->client->HeadObject(value); });
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
S3Client::Get(const S3ObjectLocation& object, std::size_t max_bytes, TransferControl control)
{
  if (max_bytes >= static_cast<std::size_t>(std::numeric_limits<std::ptrdiff_t>::max()))
    throw std::invalid_argument("invalid S3 metadata byte limit");
  const auto size = Head(object, control);
  if (!size)
    return std::nullopt;
  if (*size > max_bytes)
    throw S3ResponseError("S3 object exceeds size limit");
  BoundedBuffer buffer(max_bytes);
  Aws::S3::Model::GetObjectRequest request;
  request.SetResponseStreamFactory([&buffer] {
    buffer.Reset();
    return Aws::New<Aws::IOStream>(kAllocationTag, &buffer);
  });
  auto result = impl_->MetadataRequest(request, object, control, [&](const auto& value) { return impl_->client->GetObject(value); });
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
S3Client::Put(const S3ObjectLocation& object, const std::string& bytes, std::size_t max_bytes, TransferControl control)
{
  if (bytes.size() > max_bytes)
    throw std::invalid_argument("metadata exceeds size limit");
  Aws::S3::Model::PutObjectRequest request;
  request.WithIfNoneMatch("*");
  request.SetContentLength(bytes.size());
  request.SetContentType("application/json");
  auto body = Aws::MakeShared<Aws::StringStream>(kAllocationTag);
  body->write(bytes.data(), bytes.size());
  request.SetBody(body);
  const auto result = impl_->MetadataRequest(request, object, control, [&](const auto& value) { return impl_->client->PutObject(value); });
  if (!result.IsSuccess())
    throw S3Error(RequestFailure("PutObject", result.GetError()));
}

}  // namespace snapshot::pagebroker
