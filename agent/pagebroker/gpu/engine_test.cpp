// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "engine.hpp"
#include "storage_manifest.hpp"
#include "file_descriptor.hpp"
#include "fatal_cleanup.hpp"

#include <gtest/gtest.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>
#include <array>
#include <climits>
#include <cerrno>
#include <cstdlib>
#include <filesystem>
#include <future>
#include <latch>
#include <thread>
#include <system_error>
#include <vector>

namespace snapshot::pagebroker::gpu {
namespace {
void BlockDuringFatalCleanup()
{
  alarm(5);
  std::atomic<bool> stopping{false};
  FatalCleanupShutdown shutdown(stopping, std::chrono::milliseconds{100});
  SignalFatalCleanup();
  // Cleanup has not returned or called ReportFatalCleanup. The watchdog must
  // still cancel admission and terminate this process within its deadline.
  while (!stopping.load()) {
    std::this_thread::yield();
  }
  std::fprintf(stderr, "fatal cleanup watchdog started\n");
  for (;;) {
    pause();
  }
}

TEST(FatalCleanup, WatchdogBoundsBlockedCleanupAfterSignal)
{
  ASSERT_EXIT(BlockDuringFatalCleanup(), ::testing::ExitedWithCode(EXIT_FAILURE),
              "fatal cleanup watchdog started");
}

namespace fs = std::filesystem;
constexpr char kDevice[] = "GPU-00112233-4455-6677-8899-aabbccddeeff";

class ArtifactTest : public ::testing::Test {
protected:
  void SetUp() override
  {
    char directory[] = "/tmp/pagebroker-artifact-XXXXXX";
    ASSERT_NE(mkdtemp(directory), nullptr);
    root_ = directory;
    directory_ = FileDescriptor(open(root_.c_str(), O_DIRECTORY | O_CLOEXEC));
    ASSERT_GE(directory_.get(), 0);
  }

  void TearDown() override
  {
    std::error_code error;
    fs::remove_all(root_, error);
    EXPECT_FALSE(error);
  }

  fs::path root_;
  FileDescriptor directory_{-1};
};

TEST(EngineValidation, PreservesInheritedCancellationReason)
{
  for (bool deadline : {false, true}) {
    Cancellation parent(deadline ? Cancellation::Clock::now() : Cancellation::Clock::time_point::max());
    Cancellation child(&parent);
    Cancellation grandchild(&child);
    if (!deadline) {
      EXPECT_NO_THROW(grandchild.ThrowIfCancelled());
      parent.Cancel();
    }
    for (const auto* token : {&parent, &child, &grandchild}) {
      EXPECT_TRUE(token->IsCancelled());
      try {
        token->ThrowIfCancelled();
        FAIL() << "expected cancellation";
      } catch (const std::runtime_error& error) {
        EXPECT_STREQ(error.what(), deadline ? "operation deadline exceeded" : "operation cancelled");
      }
    }
  }
}

TEST(EngineValidation, CancelsContendedDeviceWaitAndReusesMutex)
{
  std::timed_mutex mutex;
  std::unique_lock owner(mutex);
  Cancellation cancellation;
  std::latch started(1);
  auto waiter = std::async(std::launch::async, [&] {
    started.count_down();
    EXPECT_THROW(AcquireDevice(mutex, cancellation), std::runtime_error);
  });
  started.wait();
  EXPECT_EQ(waiter.wait_for(std::chrono::milliseconds{200}), std::future_status::timeout);
  cancellation.Cancel();
  // Keep the real owner locked until after checking that cancellation finished.
  const auto status = waiter.wait_for(std::chrono::seconds{2});
  owner.unlock();
  waiter.get();
  EXPECT_EQ(status, std::future_status::ready);
  Cancellation retry;
  EXPECT_TRUE(AcquireDevice(mutex, retry).owns_lock());
}

TEST(EngineValidation, RejectsInvalidParticipantSets)
{
  const std::array<uint32_t, 2> captured{1, 2};
  const std::array<Participant, 2> valid{{{2, 1002}, {1, 1001}}};
  EXPECT_NO_THROW(ValidateParticipants(captured, valid));
  EXPECT_THROW(ValidateParticipants({}, {}), std::invalid_argument);
  EXPECT_THROW(ValidateParticipants(captured, std::span(valid).first(1)), std::invalid_argument);
  for (const auto& invalid : std::vector<std::vector<Participant>>{
           {{1, 1001}, {2, 1001}},
           {{1, 1001}, {1, 1002}},
           {{1, 1001}, {3, 1002}},
           {{1, 1001}, {2, static_cast<uint32_t>(INT_MAX) + 1}},
       }) {
    EXPECT_THROW(ValidateParticipants(captured, invalid), std::invalid_argument);
  }
}

TEST_F(ArtifactTest, RollsBackCreatedDirectoriesWithoutRemovingExistingData)
{
  const auto gpu_root = root_ / kDataDirectory;
  fs::create_directories(gpu_root / "2");
  fs::permissions(gpu_root, fs::perms::owner_all);
  EXPECT_THROW(Artifact(directory_.get(), Direction::Checkpoint, {1, 2}, {kDevice}), std::system_error);
  EXPECT_FALSE(fs::exists(gpu_root / "1"));
  EXPECT_TRUE(fs::is_directory(gpu_root / "2"));

  fs::remove(gpu_root / "2");
  EXPECT_NO_THROW(Artifact(directory_.get(), Direction::Checkpoint, {1, 2}, {kDevice}));
  EXPECT_TRUE(fs::is_directory(gpu_root / "1"));
  EXPECT_TRUE(fs::is_directory(gpu_root / "2"));
}

TEST_F(ArtifactTest, RejectsWritableDirectoryBeforeCreatingGpuData)
{
  ASSERT_EQ(fchmod(directory_.get(), S_IRWXU | S_IWGRP), 0);
  EXPECT_THROW(Artifact(directory_.get(), Direction::Checkpoint, {1}, {kDevice}), std::invalid_argument);
  EXPECT_FALSE(fs::exists(root_ / kDataDirectory));
}

TEST_F(ArtifactTest, DuplicateRetainsDirectoryAfterOriginalCloses)
{
  auto copy = FileDescriptor::Duplicate(directory_.get());
  directory_ = FileDescriptor(-1);
  ASSERT_EQ(mkdirat(copy.get(), "retained", S_IRWXU), 0);
  EXPECT_TRUE(fs::is_directory(root_ / "retained"));

  const auto flags = fcntl(copy.get(), F_GETFD);
  ASSERT_GE(flags, 0);
  EXPECT_NE(flags & FD_CLOEXEC, 0);
  const auto descriptor = copy.get();
  copy = FileDescriptor(-1);
  EXPECT_EQ(fcntl(descriptor, F_GETFD), -1);
  EXPECT_EQ(errno, EBADF);
  EXPECT_THROW(FileDescriptor::Duplicate(-1), std::system_error);
}

TEST_F(ArtifactTest, PreparesRestoreFilesBeforeParticipantsAndReleasesDescriptors)
{
  Artifact checkpoint(directory_.get(), Direction::Checkpoint, {1}, {kDevice});
  const auto participant = root_ / kDataDirectory / "1";
  const auto extent = participant / storage::DeviceFilename(0);
  FileDescriptor file(open(extent.c_str(), O_CREAT | O_EXCL | O_RDWR | O_CLOEXEC, 0600));
  ASSERT_GE(file.get(), 0);
  ASSERT_EQ(ftruncate(file.get(), 4096), 0);
  std::string error;
  ASSERT_TRUE(storage::WriteManifest(participant, {{kDevice, 4096, storage::DeviceFilename(0)}}, &error)) << error;
  const auto descriptors = [] {
    return std::distance(fs::directory_iterator("/proc/self/fd"), fs::directory_iterator{});
  };
  const auto before = descriptors();
  {
    auto preparation = std::make_shared<RestorePreparation>(directory_.get(), 3);
    // Preparation retains its source even if the caller closes its descriptor.
    auto source = FileDescriptor::Duplicate(directory_.get());
    directory_ = FileDescriptor(-1);
    ASSERT_NO_THROW(Artifact(source.get(), Direction::Restore, {1}, {kDevice}, {}, preparation));
    EXPECT_GE(descriptors(), before + 4);
    // A pathname replacement cannot silently select the stale prepared inode.
    ASSERT_EQ(unlink(extent.c_str()), 0);
    FileDescriptor replacement(open(extent.c_str(), O_CREAT | O_EXCL | O_RDWR | O_CLOEXEC, 0600));
    ASSERT_EQ(ftruncate(replacement.get(), 4096), 0);
    EXPECT_THROW(Artifact(source.get(), Direction::Restore, {1}, {kDevice}, {}, preparation), std::invalid_argument);
  }
  EXPECT_EQ(descriptors(), before - 1);
}

TEST_F(ArtifactTest, PreparationErrorsRemainObservableAndCpuOnlyCleanupIsSafe)
{
  EXPECT_NO_THROW(RestorePreparation(directory_.get(), 2));
  Artifact checkpoint(directory_.get(), Direction::Checkpoint, {1}, {kDevice});
  auto preparation = std::make_shared<RestorePreparation>(directory_.get(), 2);
  for (int retry = 0; retry < 2; ++retry) {
    EXPECT_THROW(Artifact(directory_.get(), Direction::Restore, {1}, {kDevice}, {}, preparation), std::runtime_error);
  }
}

} // namespace
} // namespace snapshot::pagebroker::gpu
