// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include "transfer/s3/s3_client.hpp"

#include <fcntl.h>
#include <gtest/gtest.h>
#include <pthread.h>
#include <sys/resource.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#include <cerrno>
#include <climits>
#include <fstream>
#include <future>
#include <system_error>
#include <thread>

#include "file_descriptor.hpp"
#include "tests/temporary_directory.hpp"

namespace {
using TestClock = std::chrono::steady_clock;
struct AdmissionWait {
  std::promise<pthread_mutex_t*> entered;
  TestClock::time_point deadline;
  bool expire_on_wake = false;
  bool registered = false;
  bool expired = false;
};
thread_local AdmissionWait* observed_wait = nullptr;

struct ObserveAdmissionWait {
  explicit ObserveAdmissionWait(AdmissionWait& wait) { observed_wait = &wait; }
  ~ObserveAdmissionWait() { observed_wait = nullptr; }
};
} // namespace

extern "C" TestClock::time_point __real__ZNSt6chrono3_V212steady_clock3nowEv() noexcept;
extern "C" TestClock::time_point __wrap__ZNSt6chrono3_V212steady_clock3nowEv() noexcept
{
  if (observed_wait && observed_wait->expired)
    return observed_wait->deadline + std::chrono::milliseconds(1);
  return __real__ZNSt6chrono3_V212steady_clock3nowEv();
}

extern "C" int __real_pthread_cond_clockwait(pthread_cond_t*, pthread_mutex_t*, clockid_t, const timespec*);
extern "C" int __wrap_pthread_cond_clockwait(pthread_cond_t* condition, pthread_mutex_t* mutex,
                                            clockid_t clock, const timespec* timeout)
{
  if (observed_wait && !observed_wait->registered) {
    observed_wait->registered = true;
    observed_wait->entered.set_value(mutex);
  }
  const auto result = __real_pthread_cond_clockwait(condition, mutex, clock, timeout);
  // Model expiry after notification but before the admission check, without a sleep.
  if (observed_wait && observed_wait->expire_on_wake && result == 0)
    observed_wait->expired = true;
  return result;
}

namespace snapshot::pagebroker {
namespace {
Cancellation
Deadline(std::chrono::milliseconds duration = std::chrono::seconds(30))
{
  return Cancellation(Cancellation::Clock::now() + duration);
}

class RcloneProcessTest : public ::testing::Test {
 protected:
  S3Connection Connection()
  {
    S3Connection connection;
    connection.access_key_id = "fake-access-key";
    connection.secret_access_key = "fake-secret-key";
    connection.session_token = "fake-session-token";
    return connection;
  }
  void Upload(const S3ObjectLocation& prefix, Cancellation cancellation = Deadline())
  {
    client.UploadFiles(directory.path(), prefix, {{"source", 8}}, cancellation);
  }
  void SetUp() override { std::ofstream(source) << "original"; }
  void ExpectReaped()
  {
    EXPECT_EQ(waitpid(-1, nullptr, WNOHANG), -1);
    EXPECT_EQ(errno, ECHILD);
  }
  test::TemporaryDirectory directory;
  std::filesystem::path source = directory.path() / "source";
  S3Client client{Connection()};
};

TEST_F(RcloneProcessTest, PassesLiteralArgumentsAndOnlySuppliedEnvironment)
{
  EXPECT_NO_THROW(Upload({"bucket", "literal/$(false); `false` with spaces quote'\""}, Deadline()));
}

TEST_F(RcloneProcessTest, DividesBufferBudgetBetweenRcloneProcesses)
{
  S3Limits limits;
  limits.part_size = 8 * 1024 * 1024 + 1;
  limits.active_files = 3;
  limits.buffer_budget = 27 * 1024 * 1024 + 2;
  S3Client limited_client(Connection(), limits);
  EXPECT_NO_THROW(limited_client.UploadFiles(directory.path(), {"bucket", "buffer-limit"},
                                              {{"source", 8}}, Deadline()));
  ExpectReaped();
}

TEST_F(RcloneProcessTest, PassesFileListAndClosesUnrelatedDescriptors)
{
  FileDescriptor input(open(source.c_str(), O_RDONLY));
  ASSERT_GE(input.get(), 0);
  FileDescriptor unrelated(fcntl(input.get(), F_DUPFD, 200));
  ASSERT_GE(unrelated.get(), 200);
  EXPECT_NO_THROW(Upload({"bucket", "descriptors/" + std::to_string(unrelated.get())}, Deadline()));
}

TEST_F(RcloneProcessTest, ClosesDescriptorsWhenPipeSetupFails)
{
  const auto pid = fork();
  ASSERT_GE(pid, 0);
  if (pid == 0) {
    // Keep descriptor limits and inherited-descriptor cleanup local to this child.
    [&] {
      ASSERT_EQ(syscall(SYS_close_range, 3U, UINT_MAX, 0U), 0);
      // Reserve standard streams even when the test runner started with one closed.
      for (int descriptor = 0; descriptor < 3; ++descriptor) {
        if (fcntl(descriptor, F_GETFD) < 0) {
          ASSERT_EQ(open("/dev/null", O_RDWR), descriptor);
        }
      }
      rlimit limit;
      ASSERT_EQ(getrlimit(RLIMIT_NOFILE, &limit), 0);
      // Input occupies 3 and 10; pipe ends occupy 4 and 5. Its read-end duplicate
      // occupies 11, leaving no slot for the write-end duplicate (minimum 10).
      limit.rlim_cur = 12;
      ASSERT_EQ(setrlimit(RLIMIT_NOFILE, &limit), 0);
      try {
        client.Head({"bucket", "pipe-failure"}, Deadline());
        FAIL() << "pipe setup unexpectedly succeeded";
      } catch (const std::system_error& error) {
        EXPECT_EQ(error.code().value(), EMFILE);
        EXPECT_NE(std::string(error.what()).find("duplicate rclone descriptor"), std::string::npos);
      } catch (...) {
        FAIL() << "pipe setup reported an unexpected exception";
      }
      for (int descriptor = 3; descriptor < 12; ++descriptor) {
        SCOPED_TRACE(descriptor);
        EXPECT_EQ(fcntl(descriptor, F_GETFD), -1);
        EXPECT_EQ(errno, EBADF);
      }
      ExpectReaped();
    }();
    _exit(::testing::Test::HasFailure() ? 1 : 0);
  }
  int status = 0;
  pid_t waited;
  do {
    waited = waitpid(pid, &status, 0);
  } while (waited < 0 && errno == EINTR);
  ASSERT_EQ(waited, pid);
  ASSERT_TRUE(WIFEXITED(status));
  EXPECT_EQ(WEXITSTATUS(status), 0);
}

TEST_F(RcloneProcessTest, DrainsDiagnosticsWithoutBlockingCompletion)
{
  EXPECT_NO_THROW(Upload({"bucket", "flood"}, Deadline()));
}

TEST_F(RcloneProcessTest, SendsNestedFilesInOneProcess)
{
  std::filesystem::create_directory(directory.path() / "nested");
  std::ofstream(directory.path() / "nested/# second") << "more";
  client.UploadFiles(directory.path(), {"bucket", "batch"}, {{"source", 8}, {"nested/# second", 4}}, Deadline());
  std::ifstream launches(directory.path() / "launches");
  std::string line;
  ASSERT_TRUE(std::getline(launches, line));
  EXPECT_EQ(line, "launch");
  EXPECT_FALSE(std::getline(launches, line));
  ExpectReaped();
}

TEST_F(RcloneProcessTest, EmptyBatchDoesNotLaunch)
{
  EXPECT_NO_THROW(client.UploadFiles(directory.path(), {"bucket", "exit127"}, {}, Deadline()));
  ExpectReaped();
}

TEST_F(RcloneProcessTest, KillsAndReapsOnOversizedOutput)
{
  EXPECT_THROW(client.Head({"bucket", "overflow"}, Deadline()), S3ResponseError);
  ExpectReaped();
}

TEST_F(RcloneProcessTest, EscalatesCancellationAndReapsBeforeReturning)
{
  const auto ready = source.string() + ".ready";
  Cancellation cancellation = Deadline();
  auto result = std::async(std::launch::async, [&] {
    try {
      Upload({"bucket", "stubborn"}, cancellation);
      ADD_FAILURE() << "cancelled upload succeeded";
      return CancellationError::Reason::DEADLINE_EXCEEDED;
    } catch (const CancellationError& error) {
      return error.reason;
    }
  });
  const auto readiness_deadline = Cancellation::Clock::now() + std::chrono::seconds(5);
  while (!std::filesystem::exists(ready) && Cancellation::Clock::now() < readiness_deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  EXPECT_TRUE(std::filesystem::exists(ready));
  cancellation.Cancel();
  EXPECT_EQ(result.wait_for(std::chrono::seconds(4)), std::future_status::ready);
  EXPECT_EQ(result.get(), CancellationError::Reason::CANCELLED);
  ExpectReaped();
}

TEST_F(RcloneProcessTest, ExpiredNotifiedWaiterDoesNotStrandAvailableSlot)
{
  S3Limits limits;
  limits.active_files = 1;
  S3Client limited_client(Connection(), limits);
  Cancellation owner_cancellation = Deadline();
  auto owner = std::async(std::launch::async, [&] {
    EXPECT_THROW(limited_client.UploadFiles(directory.path(), {"bucket", "stubborn"}, {{"source", 8}},
                                           owner_cancellation), CancellationError);
  });
  const auto ready = source.string() + ".ready";
  const auto ready_deadline = TestClock::now() + std::chrono::seconds(5);
  while (!std::filesystem::exists(ready) && TestClock::now() < ready_deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  EXPECT_TRUE(std::filesystem::exists(ready));

  auto wait_until_queued = [](AdmissionWait& wait) {
    auto entered = wait.entered.get_future();
    if (entered.wait_for(std::chrono::seconds(3)) != std::future_status::ready)
      return false;
    auto* mutex = entered.get();
    // The waiter holds this mutex until pthread_cond_clockwait atomically queues
    // it and unlocks. Acquiring it confirms queuing before starting the next waiter.
    if (pthread_mutex_lock(mutex) != 0)
      return false;
    return pthread_mutex_unlock(mutex) == 0;
  };
  AdmissionWait expiring;
  expiring.deadline = Deadline().Deadline();
  expiring.expire_on_wake = true;
  auto expired_waiter = std::async(std::launch::async, [&] {
    ObserveAdmissionWait observation(expiring);
    try {
      limited_client.Head({"bucket", "expired-waiter"}, Cancellation(expiring.deadline));
      return false;
    } catch (const CancellationError& error) {
      return error.reason == CancellationError::Reason::DEADLINE_EXCEEDED;
    }
  });
  EXPECT_TRUE(wait_until_queued(expiring));

  Cancellation live_cancellation = Deadline();
  AdmissionWait live;
  live.deadline = live_cancellation.Deadline();
  auto live_waiter = std::async(std::launch::async, [&] {
    ObserveAdmissionWait observation(live);
    limited_client.UploadFiles(directory.path(), {"bucket", "literal/$(false); `false` with spaces quote'\""},
                               {{"source", 8}}, live_cancellation);
  });
  EXPECT_TRUE(wait_until_queued(live));

  owner_cancellation.Cancel();
  EXPECT_NO_THROW(owner.get());
  EXPECT_EQ(expired_waiter.wait_for(std::chrono::seconds(3)), std::future_status::ready);
  EXPECT_EQ(live_waiter.wait_for(std::chrono::seconds(3)), std::future_status::ready);
  // Unblock futures even when a regression leaves a waiter asleep.
  live_cancellation.Cancel();
  EXPECT_TRUE(expired_waiter.get());
  EXPECT_TRUE(expiring.expired);
  EXPECT_NO_THROW(live_waiter.get());
  ExpectReaped();
}

TEST_F(RcloneProcessTest, DeadlineIssuesNoWrites)
{
  try {
    Upload({"bucket", "literal"}, Cancellation(Cancellation::Clock::now()));
    FAIL();
  } catch (const CancellationError& error) {
    EXPECT_EQ(error.reason, CancellationError::Reason::DEADLINE_EXCEEDED);
  }
  ExpectReaped();
}

TEST_F(RcloneProcessTest, MissingExecutableFailsAndReaps)
{
  const std::filesystem::path executable(PAGEBROKER_RCLONE_PATH);
  const auto saved = executable.string() + ".saved";
  std::filesystem::rename(executable, saved);
  std::exception_ptr failure;
  try {
    Upload({"bucket", "literal"}, Deadline());
  } catch (...) {
    failure = std::current_exception();
  }
  std::filesystem::rename(saved, executable);
  ASSERT_TRUE(failure);
  try {
    std::rethrow_exception(failure);
  } catch (const S3Error& error) {
    EXPECT_EQ(error.http_status, 0);
  }
  ExpectReaped();
}

TEST_F(RcloneProcessTest, ChildExit127FailsAndReaps)
{
  try {
    Upload({"bucket", "exit127"}, Deadline());
    FAIL();
  } catch (const S3Error& error) {
    EXPECT_EQ(error.http_status, 0);
  }
  ExpectReaped();
}
} // namespace
} // namespace snapshot::pagebroker
