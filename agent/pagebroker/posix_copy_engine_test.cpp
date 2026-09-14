// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <filesystem>
#include <fstream>
#include <mutex>
#include <string>
#include <sys/stat.h>
#include <thread>
#include <unistd.h>

#include "posix_copy_engine.hpp"

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;
using namespace std::chrono_literals;

namespace {
void WriteCustomStorage(const fs::path& process,
                        size_t carrier_bytes,
                        char value,
                        char digest_digit = '0')
{
  fs::create_directories(process);
  std::ofstream(process / "manifest.txt")
      << "version 3\ndevice_count 1\ndevice 0 "
         "GPU-00000000-0000-0000-0000-000000000000 "
      << carrier_bytes << " device-0000.bin "
      << std::string(64, digest_digit) << "\n";
  std::ofstream(process / "device-0000.bin")
      << std::string(carrier_bytes, value);
}

void WriteSparseFile(const fs::path& path, size_t bytes)
{
  std::ofstream output(path, std::ios::binary);
  ASSERT_TRUE(output.is_open());
  ASSERT_GT(bytes, 0u);
  output.seekp(static_cast<std::streamoff>(bytes - 1));
  output.put('\0');
  ASSERT_TRUE(output.good());
}

bool ContainsLeaf(const fs::path& root, const std::string& leaf)
{
  if (!fs::exists(root))
    return false;
  for (const auto& entry : fs::recursive_directory_iterator(root)) {
    if (entry.path().filename() == leaf)
      return true;
  }
  return false;
}

class PosixCopyEngineTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    root_ = fs::temp_directory_path() /
            ("pagebroker-posix-copy-test-" + std::to_string(getpid()) + "-" +
             std::to_string(counter_++));
    storage_ = root_ / "storage";
    source_ = root_ / "source";
    fs::create_directories(storage_);
    fs::create_directories(source_ / "nested");
    std::ofstream(source_ / "nested" / "image") << "new";
  }

  void TearDown() override { fs::remove_all(root_); }

  StorageBackend Destination(const fs::path& path) const
  {
    StorageBackend destination;
    destination.mutable_filesystem()->set_directory(path.string());
    return destination;
  }

  static size_t CountTemporaryLeaves(const fs::path& parent,
                                     const std::string& prefix)
  {
    size_t count = 0;
    for (const auto& entry : fs::directory_iterator(parent)) {
      if (entry.path().filename().string().starts_with(prefix))
        ++count;
    }
    return count;
  }

  inline static unsigned counter_ = 0;
  fs::path root_;
  fs::path storage_;
  fs::path source_;
};

TEST_F(PosixCopyEngineTest,
       FailsClosedWhenTemporaryLeafIsReplacedBySymlink)
{
  const fs::path parent = storage_ / "destination-parent";
  const fs::path published = parent / "checkpoint";
  const fs::path outside = root_ / "outside";
  fs::create_directories(published);
  fs::create_directories(outside);
  std::ofstream(published / "old") << "old";
  std::ofstream(outside / "sentinel") << "outside";
  const std::string prefix = "checkpoint.pagebroker-partial.";
  bool attacked = false;

  PosixCopyEngine engine(storage_, {}, [&] {
    for (const auto& entry : fs::directory_iterator(parent)) {
      if (!entry.path().filename().string().starts_with(prefix))
        continue;
      fs::remove(entry.path());
      fs::create_directory_symlink(outside, entry.path());
      attacked = true;
      return;
    }
    FAIL() << "checkpoint temporary directory was not present";
  });

  EXPECT_THROW(engine.PublishCheckpoint(source_, Destination(published)),
               std::filesystem::filesystem_error);
  EXPECT_TRUE(attacked);
  EXPECT_TRUE(fs::exists(published / "old"));
  EXPECT_FALSE(fs::exists(published / "nested"));
  EXPECT_TRUE(fs::exists(outside / "sentinel"));
  EXPECT_FALSE(fs::exists(outside / "nested"));
  EXPECT_EQ(CountTemporaryLeaves(parent, prefix), 0u);
  EXPECT_FALSE(fs::exists(parent / "checkpoint.pagebroker-partial"));
  EXPECT_FALSE(fs::exists(parent / "checkpoint.pagebroker-previous"));
}

TEST_F(PosixCopyEngineTest, ParentPathSwapCannotRedirectCheckpointCopy)
{
  const fs::path parent = storage_ / "destination-parent";
  const fs::path held_parent = storage_ / "held-destination-parent";
  const fs::path published = parent / "checkpoint";
  const fs::path outside = root_ / "outside";
  fs::create_directories(published);
  fs::create_directories(outside);
  std::ofstream(published / "old") << "old";
  std::ofstream(outside / "sentinel") << "outside";
  bool attacked = false;

  PosixCopyEngine engine(storage_, {}, [&] {
    fs::rename(parent, held_parent);
    fs::create_directory_symlink(outside, parent);
    attacked = true;
  });

  EXPECT_NO_THROW(
      engine.PublishCheckpoint(source_, Destination(published)));
  EXPECT_TRUE(attacked);
  EXPECT_TRUE(fs::exists(held_parent / "checkpoint" / "nested" / "image"));
  EXPECT_FALSE(fs::exists(held_parent / "checkpoint" / "old"));
  EXPECT_TRUE(fs::exists(outside / "sentinel"));
  EXPECT_FALSE(fs::exists(outside / "checkpoint"));
  EXPECT_EQ(CountTemporaryLeaves(
                held_parent, "checkpoint.pagebroker-partial."),
            0u);
  EXPECT_FALSE(
      fs::exists(held_parent / "checkpoint.pagebroker-partial"));
  EXPECT_FALSE(
      fs::exists(held_parent / "checkpoint.pagebroker-previous"));
}

TEST_F(PosixCopyEngineTest,
       CleanupFailureIsObservableAndRecoveredOnNextAttempt)
{
  const fs::path parent = storage_ / "destination-parent";
  const fs::path published = parent / "checkpoint";
  const fs::path previous = parent / "checkpoint.pagebroker-previous";
  const fs::path marker = parent / "checkpoint.pagebroker-partial";
  fs::create_directories(published);
  std::ofstream(published / "old") << "old";
  bool fail_cleanup = true;
  PosixCopyEngine engine(
      storage_, {}, {}, [&](const std::string& operation) {
        return fail_cleanup && operation == "cleanup previous checkpoint"
                   ? EIO
                   : 0;
      });

  EXPECT_THROW(engine.PublishCheckpoint(source_, Destination(published)),
               std::system_error);
  EXPECT_TRUE(fs::exists(published / "nested" / "image"));
  EXPECT_TRUE(fs::exists(previous / "old"));
  EXPECT_TRUE(fs::is_directory(marker));

  fail_cleanup = false;
  EXPECT_FALSE(engine.CheckpointDestinationConflicts(
      Destination(published)));
  EXPECT_TRUE(fs::exists(published / "nested" / "image"));
  EXPECT_FALSE(fs::exists(previous));
  EXPECT_FALSE(fs::exists(marker));
}

TEST_F(PosixCopyEngineTest, RecoversStalePublicationMarker)
{
  const fs::path parent = storage_ / "destination-parent";
  const fs::path published = parent / "checkpoint";
  const fs::path previous = parent / "checkpoint.pagebroker-previous";
  const fs::path marker = parent / "checkpoint.pagebroker-partial";
  const fs::path temporary =
      parent / "checkpoint.pagebroker-partial.0123456789abcdef";
  fs::create_directories(previous);
  fs::create_directories(marker);
  fs::create_directories(temporary);
  std::ofstream(previous / "old") << "old";
  std::ofstream(temporary / "incomplete") << "incomplete";
  PosixCopyEngine engine(storage_);

  EXPECT_FALSE(engine.CheckpointDestinationConflicts(
      Destination(published)));
  EXPECT_TRUE(fs::exists(published / "old"));
  EXPECT_FALSE(fs::exists(previous));
  EXPECT_FALSE(fs::exists(temporary));
  EXPECT_FALSE(fs::exists(marker));
}

TEST_F(PosixCopyEngineTest,
       PublishAndRollbackFailureRetainRecoverablePreviousCheckpoint)
{
  const fs::path parent = storage_ / "destination-parent";
  const fs::path published = parent / "checkpoint";
  const fs::path previous = parent / "checkpoint.pagebroker-previous";
  const fs::path marker = parent / "checkpoint.pagebroker-partial";
  fs::create_directories(published);
  std::ofstream(published / "old") << "old";
  bool fail = true;
  PosixCopyEngine engine(
      storage_, {}, {}, [&](const std::string& operation) {
        if (!fail)
          return 0;
        if (operation == "publish checkpoint")
          return EIO;
        if (operation == "rollback previous checkpoint")
          return EPERM;
        return 0;
      });

  try {
    engine.PublishCheckpoint(source_, Destination(published));
    FAIL() << "publication unexpectedly succeeded";
  }
  catch (const std::runtime_error& error) {
    EXPECT_NE(std::string(error.what()).find("publication failed"),
              std::string::npos);
    EXPECT_NE(std::string(error.what()).find("rollback failed"),
              std::string::npos);
    EXPECT_NE(std::string(error.what()).find("recovery state retained"),
              std::string::npos);
  }
  EXPECT_FALSE(fs::exists(published));
  EXPECT_TRUE(fs::exists(previous / "old"));
  EXPECT_TRUE(fs::is_directory(marker));
  EXPECT_EQ(CountTemporaryLeaves(
                parent, "checkpoint.pagebroker-partial."),
            1u);

  fail = false;
  EXPECT_FALSE(engine.CheckpointDestinationConflicts(
      Destination(published)));
  EXPECT_TRUE(fs::exists(published / "old"));
  EXPECT_FALSE(fs::exists(previous));
  EXPECT_FALSE(fs::exists(marker));
  EXPECT_EQ(CountTemporaryLeaves(
                parent, "checkpoint.pagebroker-partial."),
            0u);
}

TEST_F(PosixCopyEngineTest,
       ParentFsyncFailureIsPropagatedWithPriorCheckpointRetained)
{
  const fs::path parent = storage_ / "destination-parent";
  const fs::path published = parent / "checkpoint";
  const fs::path previous = parent / "checkpoint.pagebroker-previous";
  const fs::path marker = parent / "checkpoint.pagebroker-partial";
  fs::create_directories(published);
  std::ofstream(published / "old") << "old";
  bool fail_sync = true;
  PosixCopyEngine engine(
      storage_, {}, {}, [&](const std::string& operation) {
        return fail_sync && operation == "sync published checkpoint" ? EIO
                                                                       : 0;
      });

  EXPECT_THROW(engine.PublishCheckpoint(source_, Destination(published)),
               std::system_error);
  EXPECT_TRUE(fs::exists(published / "nested" / "image"));
  EXPECT_TRUE(fs::exists(previous / "old"));
  EXPECT_TRUE(fs::is_directory(marker));

  fail_sync = false;
  EXPECT_FALSE(engine.CheckpointDestinationConflicts(
      Destination(published)));
  EXPECT_TRUE(fs::exists(published / "nested" / "image"));
  EXPECT_FALSE(fs::exists(previous));
  EXPECT_FALSE(fs::exists(marker));
}

TEST_F(PosixCopyEngineTest,
       DirectRestoreCopiesMetadataAndRetainsCarrierOnPinnedSource)
{
  const fs::path artifact = storage_ / "artifact";
  const fs::path process =
      artifact / "cuda-custom-storage" / "process-nspid-42";
  const fs::path destination = root_ / "staging";
  const fs::path held = storage_ / "artifact-held";
  const fs::path replacement = storage_ / "replacement";
  WriteCustomStorage(process, 4096, 'x');
  std::ofstream(artifact / "criu.img") << "criu";
  WriteCustomStorage(replacement / "cuda-custom-storage" /
                         "process-nspid-42",
                     4096, 'y', '1');
  std::ofstream(replacement / "criu.img") << "evil";

  StorageBackend source = Destination(artifact);
  bool renamed = false;
  PosixCopyEngine engine(storage_, [&] {
    fs::rename(artifact, held);
    fs::create_directory_symlink(replacement, artifact);
    renamed = true;
  });

  auto plan = engine.PrepareDirectRestore(source, {42}, 4);
  const uintmax_t expected_metadata =
      4 + fs::file_size(held / "cuda-custom-storage" /
                        "process-nspid-42" / "manifest.txt");
  EXPECT_EQ(plan.copied_bytes, expected_metadata);
  auto staged = engine.StageDirectRestore(std::move(plan), destination);

  EXPECT_TRUE(renamed);
  EXPECT_EQ(staged.copied_bytes, expected_metadata);
  EXPECT_EQ(staged.retained_bytes, 4096u);
  EXPECT_TRUE(fs::exists(destination / "criu.img"));
  EXPECT_TRUE(fs::exists(destination / "cuda-custom-storage" /
                         "process-nspid-42" / "manifest.txt"));
  EXPECT_FALSE(fs::exists(destination / "cuda-custom-storage" /
                          "process-nspid-42" / "device-0000.bin"));
  std::string staged_criu;
  std::ifstream(destination / "criu.img") >> staged_criu;
  EXPECT_EQ(staged_criu, "criu");
  struct stat opened{};
  struct stat held_stat{};
  ASSERT_EQ(fstat(staged.source_directory.get(), &opened), 0);
  ASSERT_EQ(stat(held.c_str(), &held_stat), 0);
  EXPECT_EQ(opened.st_dev, held_stat.st_dev);
  EXPECT_EQ(opened.st_ino, held_stat.st_ino);
}

TEST_F(PosixCopyEngineTest,
       DirectRestorePinsProcessManifestAndCarrierAcrossCoherentReplacement)
{
  const fs::path artifact = storage_ / "artifact";
  const fs::path process =
      artifact / "cuda-custom-storage" / "process-nspid-42";
  const fs::path held = storage_ / "process-nspid-42-held";
  const fs::path destination = root_ / "staging";
  WriteCustomStorage(process, 16, 'x', '0');
  std::ofstream(artifact / "criu.img") << "criu";

  PosixCopyEngine engine(storage_);
  auto plan = engine.PrepareDirectRestore(Destination(artifact), {42}, 4);
  fs::rename(process, held);
  WriteCustomStorage(process, 16, 'y', '1');
  auto staged = engine.StageDirectRestore(std::move(plan), destination);

  std::ifstream manifest_input(destination / "cuda-custom-storage" /
                               "process-nspid-42" / "manifest.txt");
  std::string manifest;
  std::getline(manifest_input, manifest, '\0');
  EXPECT_NE(manifest.find(std::string(64, '0')), std::string::npos);
  EXPECT_EQ(manifest.find(std::string(64, '1')), std::string::npos);
  ASSERT_EQ(staged.processes.size(), 1u);
  ASSERT_EQ(staged.processes[0].carriers.size(), 1u);
  char carrier = '\0';
  ASSERT_EQ(pread(staged.processes[0].carriers[0].descriptor.get(), &carrier,
                  1, 0),
            1);
  EXPECT_EQ(carrier, 'x');
}

TEST_F(PosixCopyEngineTest,
       DirectRestoreRejectsNonCudaDirectoryReplacementDuringStage)
{
  const fs::path artifact = storage_ / "artifact";
  const fs::path process =
      artifact / "cuda-custom-storage" / "process-nspid-42";
  const fs::path containers = artifact / "containers";
  const fs::path held = root_ / "containers-held";
  const fs::path replacement = root_ / "containers-replacement";
  const fs::path destination = root_ / "staging";
  WriteCustomStorage(process, 16, 'x', '0');
  fs::create_directories(containers / "main");
  fs::create_directories(replacement / "main");
  std::ofstream(containers / "main" / "pages.img") << "good";
  std::ofstream(replacement / "main" / "pages.img") << "evil";

  bool replaced = false;
  const auto replace_directory = [&](const std::string& operation) {
    if (operation != "open direct restore directory containers")
      return 0;
    fs::rename(containers, held);
    fs::rename(replacement, containers);
    replaced = true;
    return 0;
  };
  PosixCopyEngine engine(storage_, std::function<void()>{},
                         std::function<void()>{}, replace_directory);
  auto plan = engine.PrepareDirectRestore(Destination(artifact), {42}, 4);

  try {
    (void)engine.StageDirectRestore(std::move(plan), destination);
    FAIL() << "directory replacement unexpectedly succeeded";
  }
  catch (const fs::filesystem_error& error) {
    EXPECT_EQ(error.code().value(), ESTALE);
  }
  EXPECT_TRUE(replaced);
  EXPECT_FALSE(fs::exists(destination / "containers" / "main" /
                          "pages.img"));
}

TEST_F(PosixCopyEngineTest,
       DirectRestoreCopiesLargeMetadataFilesConcurrently)
{
  const fs::path artifact = storage_ / "artifact";
  const fs::path process =
      artifact / "cuda-custom-storage" / "process-nspid-42";
  const fs::path images = artifact / "containers" / "main";
  const fs::path destination = root_ / "staging";
  WriteCustomStorage(process, 16, 'x', '0');
  fs::create_directories(images);
  constexpr size_t image_bytes = 1024U * 1024U;
  WriteSparseFile(images / "pages-1.img", image_bytes);
  WriteSparseFile(images / "pages-2.img", image_bytes);

  std::mutex mutex;
  std::condition_variable condition;
  size_t entered = 0;
  size_t active = 0;
  size_t maximum_active = 0;
  const auto observe = [&](const std::string& operation) {
    if (operation != "copy direct restore file")
      return 0;
    std::unique_lock lock(mutex);
    ++entered;
    ++active;
    maximum_active = std::max(maximum_active, active);
    condition.notify_all();
    (void)condition.wait_for(lock, 2s, [&] { return entered >= 2; });
    --active;
    return 0;
  };
  PosixCopyEngine engine(storage_, std::function<void()>{},
                         std::function<void()>{}, observe);

  auto plan = engine.PrepareDirectRestore(Destination(artifact), {42}, 4);
  const uintmax_t admitted_bytes = plan.copied_bytes;
  const auto staged =
      engine.StageDirectRestore(std::move(plan), destination);

  EXPECT_GE(maximum_active, 2u);
  EXPECT_EQ(staged.copied_bytes, admitted_bytes);
  EXPECT_EQ(fs::file_size(destination / "containers" / "main" /
                          "pages-1.img"),
            image_bytes);
  EXPECT_EQ(fs::file_size(destination / "containers" / "main" /
                          "pages-2.img"),
            image_bytes);
}

TEST_F(PosixCopyEngineTest,
       DirectRestoreDrainsParallelCopiesBeforeReportingFailure)
{
  const fs::path artifact = storage_ / "artifact";
  const fs::path process =
      artifact / "cuda-custom-storage" / "process-nspid-42";
  const fs::path images = artifact / "containers" / "main";
  const fs::path destination = root_ / "staging";
  WriteCustomStorage(process, 16, 'x', '0');
  fs::create_directories(images);
  constexpr size_t image_bytes = 1024U * 1024U;
  WriteSparseFile(images / "pages-1.img", image_bytes);
  WriteSparseFile(images / "pages-2.img", image_bytes);

  std::mutex mutex;
  std::condition_variable condition;
  size_t calls = 0;
  bool blocker_entered = false;
  bool failure_seen = false;
  bool release = false;
  const auto fail_with_blocked_sibling = [&](const std::string& operation) {
    if (operation != "copy direct restore file")
      return 0;
    std::unique_lock lock(mutex);
    const size_t call = calls++;
    if (call == 0) {
      blocker_entered = true;
      condition.notify_all();
      condition.wait(lock, [&] { return release; });
      return 0;
    }
    failure_seen = true;
    condition.notify_all();
    return EIO;
  };
  PosixCopyEngine engine(storage_, std::function<void()>{},
                         std::function<void()>{},
                         fail_with_blocked_sibling);
  auto plan = engine.PrepareDirectRestore(Destination(artifact), {42}, 4);
  std::atomic_bool done = false;
  std::exception_ptr error;
  std::thread staging([&] {
    try {
      (void)engine.StageDirectRestore(std::move(plan), destination);
    }
    catch (...) {
      error = std::current_exception();
    }
    done.store(true, std::memory_order_release);
  });

  bool reached_failure = false;
  {
    std::unique_lock lock(mutex);
    reached_failure = condition.wait_for(
        lock, 2s, [&] { return blocker_entered && failure_seen; });
  }
  EXPECT_TRUE(reached_failure);
  if (reached_failure) {
    EXPECT_FALSE(done.load(std::memory_order_acquire));
  }
  {
    std::lock_guard lock(mutex);
    release = true;
  }
  condition.notify_all();
  staging.join();

  EXPECT_NE(error, nullptr);
}

TEST_F(PosixCopyEngineTest,
       DirectRestoreCopyOperationsAreBoundedAcrossTransactions)
{
  const fs::path first_artifact = storage_ / "artifact-a";
  const fs::path second_artifact = storage_ / "artifact-b";
  WriteCustomStorage(first_artifact / "cuda-custom-storage" /
                         "process-nspid-42",
                     16, 'x', '0');
  WriteCustomStorage(second_artifact / "cuda-custom-storage" /
                         "process-nspid-43",
                     16, 'y', '1');
  const fs::path first_images = first_artifact / "containers" / "main";
  const fs::path second_images = second_artifact / "containers" / "main";
  fs::create_directories(first_images);
  fs::create_directories(second_images);
  constexpr size_t file_count = 8;
  constexpr size_t image_bytes = 128U * 1024U;
  for (size_t index = 0; index < file_count; ++index) {
    const std::string name = "pages-" + std::to_string(index) + ".img";
    WriteSparseFile(first_images / name, image_bytes);
    WriteSparseFile(second_images / name, image_bytes);
  }

  std::mutex mutex;
  std::condition_variable condition;
  size_t active = 0;
  size_t maximum_active = 0;
  size_t permit_attempts = 0;
  bool release = false;
  const auto observe = [&](const std::string& operation) {
    if (operation == "acquire direct restore copy permit") {
      std::lock_guard lock(mutex);
      ++permit_attempts;
      condition.notify_all();
      return 0;
    }
    if (operation != "copy direct restore file")
      return 0;
    std::unique_lock lock(mutex);
    ++active;
    maximum_active = std::max(maximum_active, active);
    condition.notify_all();
    condition.wait(lock, [&] { return release; });
    --active;
    return 0;
  };
  PosixCopyEngine engine(storage_, std::function<void()>{},
                         std::function<void()>{}, observe);
  auto first_plan =
      engine.PrepareDirectRestore(Destination(first_artifact), {42}, 4);
  auto second_plan =
      engine.PrepareDirectRestore(Destination(second_artifact), {43}, 4);
  std::exception_ptr first_error;
  std::exception_ptr second_error;
  std::thread first([&] {
    try {
      (void)engine.StageDirectRestore(std::move(first_plan),
                                      root_ / "staging-a");
    }
    catch (...) {
      first_error = std::current_exception();
    }
  });

  bool first_saturated = false;
  size_t permit_attempt_baseline = 0;
  {
    std::unique_lock lock(mutex);
    first_saturated =
        condition.wait_for(lock, 2s, [&] { return active == 8; });
    permit_attempt_baseline = permit_attempts;
  }
  std::thread second;
  bool second_at_permit = false;
  bool exceeded_limit = false;
  if (first_saturated) {
    second = std::thread([&] {
      try {
        (void)engine.StageDirectRestore(std::move(second_plan),
                                        root_ / "staging-b");
      }
      catch (...) {
        second_error = std::current_exception();
      }
    });
    std::unique_lock lock(mutex);
    second_at_permit = condition.wait_for(
        lock, 2s, [&] { return permit_attempts > permit_attempt_baseline; });
    if (second_at_permit) {
      // The second request has returned from the test seam immediately before
      // the production semaphore acquire. Hold the first eight operations for
      // a bounded negative-observation window: a per-request limiter would
      // expose a ninth copy here.
      exceeded_limit = condition.wait_for(
          lock, 100ms, [&] { return active > 8; });
    }
  }
  {
    std::lock_guard lock(mutex);
    release = true;
  }
  condition.notify_all();
  first.join();
  if (second.joinable())
    second.join();

  EXPECT_TRUE(first_saturated);
  EXPECT_TRUE(second_at_permit);
  EXPECT_FALSE(exceeded_limit);
  EXPECT_EQ(maximum_active, 8u);
  EXPECT_EQ(first_error, nullptr);
  EXPECT_EQ(second_error, nullptr);
}

TEST_F(PosixCopyEngineTest,
       DirectRestoreRejectsDescriptorGrowthAfterPreflightAdmission)
{
  const fs::path artifact = storage_ / "artifact";
  const fs::path process =
      artifact / "cuda-custom-storage" / "process-nspid-42";
  WriteCustomStorage(process, 16, 'x', '0');

  bool grown = false;
  PosixCopyEngine engine(storage_, [&] {
    std::ofstream(process / "device-0001.bin") << std::string(16, 'y');
    std::ofstream(process / "manifest.txt")
        << "version 3\ndevice_count 2\n"
           "device 0 GPU-00000000-0000-0000-0000-000000000000 16 "
           "device-0000.bin "
        << std::string(64, '0')
        << "\ndevice 1 GPU-11111111-1111-1111-1111-111111111111 16 "
           "device-0001.bin "
        << std::string(64, '1') << "\n";
    grown = true;
  });

  const size_t admitted =
      engine.DirectRestoreDescriptorCount(Destination(artifact), {42});
  EXPECT_EQ(admitted, 4u);
  EXPECT_THROW(
      engine.PrepareDirectRestore(Destination(artifact), {42}, admitted),
      std::runtime_error);
  EXPECT_TRUE(grown);
}

TEST_F(PosixCopyEngineTest, RestoreGrowthStopsBeforeReservationIsExceeded)
{
  const fs::path artifact = storage_ / "artifact";
  const fs::path destination = root_ / "staging";
  fs::create_directories(artifact);
  std::ofstream(artifact / "image") << "image";
  bool grown = false;
  PosixCopyEngine engine(storage_, [&] {
    std::ofstream(artifact / "image", std::ios::app) << "growth";
    grown = true;
  });

  EXPECT_THROW(engine.StageRestore(Destination(artifact), destination, 5),
               StagingCapacityExceeded);
  EXPECT_TRUE(grown);
  EXPECT_LE(fs::file_size(destination / "image"), 5u);
}

TEST_F(PosixCopyEngineTest, DirectRestoreRejectsCarrierSymlink)
{
  const fs::path artifact = storage_ / "artifact";
  const fs::path process =
      artifact / "cuda-custom-storage" / "process-nspid-42";
  WriteCustomStorage(process, 3, 'x');
  fs::remove(process / "device-0000.bin");
  fs::create_symlink(source_ / "nested" / "image",
                     process / "device-0000.bin");

  EXPECT_THROW(
      PosixCopyEngine(storage_).StageDirectRestore(
          PosixCopyEngine(storage_).PrepareDirectRestore(
              Destination(artifact), {42}, 4),
          root_ / "staging"),
      std::runtime_error);
  EXPECT_FALSE(fs::exists(root_ / "staging" /
                          "cuda-custom-storage" / "process-nspid-42" /
                          "device-0000.bin"));
}

TEST_F(PosixCopyEngineTest,
       RegularRestoreReferenceHardlinksWithoutCopyingAndSurvivesSourceDelete)
{
  const fs::path artifact = storage_ / "artifact";
  fs::create_directories(artifact / "nested");
  std::ofstream(artifact / "nested" / "pages.img") << "checkpoint-pages";
  PosixCopyEngine engine(storage_, "release/node-a");

  const fs::path reference =
      engine.ReferenceRegularRestore(Destination(artifact), "transaction-a");
  struct stat source_stat{};
  struct stat reference_stat{};
  ASSERT_EQ(stat((artifact / "nested" / "pages.img").c_str(), &source_stat),
            0);
  ASSERT_EQ(stat((reference / "nested" / "pages.img").c_str(),
                 &reference_stat),
            0);
  EXPECT_EQ(source_stat.st_dev, reference_stat.st_dev);
  EXPECT_EQ(source_stat.st_ino, reference_stat.st_ino);

  fs::remove_all(artifact);
  std::ifstream retained(reference / "nested" / "pages.img");
  std::string contents;
  retained >> contents;
  EXPECT_EQ(contents, "checkpoint-pages");
}

TEST_F(PosixCopyEngineTest,
       RegularRestoreReferenceLinksBatchesAndPreservesDirectoryModes)
{
  const fs::path artifact = storage_ / "artifact";
  const fs::path nested = artifact / "nested";
  fs::create_directories(nested);
  constexpr size_t root_file_count = 96;
  constexpr size_t nested_file_count = 67;
  for (size_t index = 0; index < root_file_count; ++index)
    std::ofstream(artifact / ("root-" + std::to_string(index))) << index;
  for (size_t index = 0; index < nested_file_count; ++index)
    std::ofstream(nested / ("nested-" + std::to_string(index))) << index;
  ASSERT_EQ(chmod(artifact.c_str(), 0750), 0);
  ASSERT_EQ(chmod(nested.c_str(), 0710), 0);
  PosixCopyEngine engine(storage_, "release/node-a");

  const fs::path reference =
      engine.ReferenceRegularRestore(Destination(artifact), "transaction-a");

  const auto expect_hardlink = [](const fs::path& source,
                                  const fs::path& destination) {
    struct stat source_stat{};
    struct stat destination_stat{};
    ASSERT_EQ(stat(source.c_str(), &source_stat), 0);
    ASSERT_EQ(stat(destination.c_str(), &destination_stat), 0);
    EXPECT_EQ(source_stat.st_dev, destination_stat.st_dev);
    EXPECT_EQ(source_stat.st_ino, destination_stat.st_ino);
  };
  for (size_t index = 0; index < root_file_count; ++index) {
    const std::string name = "root-" + std::to_string(index);
    expect_hardlink(artifact / name, reference / name);
  }
  for (size_t index = 0; index < nested_file_count; ++index) {
    const std::string name = "nested-" + std::to_string(index);
    expect_hardlink(nested / name, reference / "nested" / name);
  }
  struct stat root_stat{};
  struct stat nested_stat{};
  ASSERT_EQ(stat(reference.c_str(), &root_stat), 0);
  ASSERT_EQ(stat((reference / "nested").c_str(), &nested_stat), 0);
  EXPECT_EQ(root_stat.st_mode & 0777, static_cast<mode_t>(0750));
  EXPECT_EQ(nested_stat.st_mode & 0777, static_cast<mode_t>(0710));
}

TEST_F(PosixCopyEngineTest,
       RegularRestoreReferenceHelpersAreBoundedAcrossTransactions)
{
  const fs::path first_artifact = storage_ / "artifact-a";
  const fs::path second_artifact = storage_ / "artifact-b";
  fs::create_directories(first_artifact);
  fs::create_directories(second_artifact);
  constexpr size_t file_count = 128;
  for (size_t index = 0; index < file_count; ++index) {
    const std::string name = "image-" + std::to_string(index);
    std::ofstream(first_artifact / name) << index;
    std::ofstream(second_artifact / name) << index;
  }

  std::mutex mutex;
  std::condition_variable condition;
  size_t active = 0;
  size_t max_active = 0;
  bool release = false;
  const auto observe = [&](const std::string& operation) {
    if (operation != "link regular restore reference")
      return 0;
    std::unique_lock lock(mutex);
    ++active;
    max_active = std::max(max_active, active);
    condition.notify_all();
    condition.wait(lock, [&] { return release; });
    --active;
    return 0;
  };
  PosixCopyEngine first_engine(storage_, "release/node-a", observe);
  PosixCopyEngine second_engine(storage_, "release/node-b", observe);
  std::exception_ptr first_error;
  std::exception_ptr second_error;
  std::thread first([&] {
    try {
      (void)first_engine.ReferenceRegularRestore(
          Destination(first_artifact), "transaction-a");
    }
    catch (...) {
      first_error = std::current_exception();
    }
  });

  bool first_saturated = false;
  {
    std::unique_lock lock(mutex);
    first_saturated =
        condition.wait_for(lock, 2s, [&] { return active == 8; });
  }
  std::thread second;
  bool both_active = false;
  if (first_saturated) {
    second = std::thread([&] {
      try {
        (void)second_engine.ReferenceRegularRestore(
            Destination(second_artifact), "transaction-b");
      }
      catch (...) {
        second_error = std::current_exception();
      }
    });
    std::unique_lock lock(mutex);
    both_active = condition.wait_for(lock, 2s, [&] { return active == 9; });
  }
  {
    std::lock_guard lock(mutex);
    release = true;
  }
  condition.notify_all();
  first.join();
  if (second.joinable())
    second.join();

  EXPECT_TRUE(first_saturated);
  EXPECT_TRUE(both_active);
  EXPECT_EQ(max_active, 9u);
  EXPECT_EQ(first_error, nullptr);
  EXPECT_EQ(second_error, nullptr);
}

TEST_F(PosixCopyEngineTest,
       RegularRestoreReferenceDrainsHelpersBeforeFailureCleanup)
{
  const fs::path artifact = storage_ / "artifact";
  fs::create_directories(artifact);
  constexpr size_t file_count = 128;
  for (size_t index = 0; index < file_count; ++index)
    std::ofstream(artifact / ("image-" + std::to_string(index))) << index;

  std::mutex mutex;
  std::condition_variable condition;
  size_t calls = 0;
  bool blocker_entered = false;
  bool failure_seen = false;
  bool release = false;
  const auto fail_with_blocked_sibling = [&](const std::string& operation) {
    if (operation != "link regular restore reference")
      return 0;
    std::unique_lock lock(mutex);
    const size_t call = calls++;
    if (call == 0) {
      blocker_entered = true;
      condition.notify_all();
      condition.wait(lock, [&] { return release; });
      return 0;
    }
    if (call == 1) {
      failure_seen = true;
      condition.notify_all();
      return EIO;
    }
    return 0;
  };
  PosixCopyEngine engine(storage_, "release/node-a",
                         fail_with_blocked_sibling);
  std::atomic_bool done = false;
  std::exception_ptr error;
  std::thread restore([&] {
    try {
      (void)engine.ReferenceRegularRestore(Destination(artifact),
                                           "transaction-a");
    }
    catch (...) {
      error = std::current_exception();
    }
    done.store(true, std::memory_order_release);
  });

  bool reached_failure = false;
  {
    std::unique_lock lock(mutex);
    reached_failure = condition.wait_for(
        lock, 2s, [&] { return blocker_entered && failure_seen; });
  }
  EXPECT_TRUE(reached_failure);
  if (reached_failure) {
    std::this_thread::sleep_for(20ms);
    EXPECT_FALSE(done.load(std::memory_order_acquire));
    EXPECT_TRUE(ContainsLeaf(storage_ / ".pagebroker-restore",
                             "transaction-a"));
  }
  {
    std::lock_guard lock(mutex);
    release = true;
  }
  condition.notify_all();
  restore.join();

  EXPECT_NE(error, nullptr);
  EXPECT_FALSE(ContainsLeaf(storage_ / ".pagebroker-restore",
                            "transaction-a"));
}

TEST_F(PosixCopyEngineTest,
       RegularRestoreReferenceRejectsSourceReplacementAndCleansTree)
{
  const fs::path artifact = storage_ / "artifact";
  fs::create_directories(artifact);
  constexpr size_t file_count = 64;
  for (size_t index = 0; index < file_count; ++index)
    std::ofstream(artifact / ("image-" + std::to_string(index))) << "old";

  std::mutex mutex;
  bool replaced = false;
  const auto replace_sources = [&](const std::string& operation) {
    if (operation != "link regular restore reference")
      return 0;
    std::lock_guard lock(mutex);
    if (replaced)
      return 0;
    for (size_t index = 0; index < file_count; ++index) {
      const std::string name = "image-" + std::to_string(index);
      fs::rename(artifact / name, artifact / (name + ".held"));
      std::ofstream(artifact / name) << "replacement";
    }
    replaced = true;
    return 0;
  };
  PosixCopyEngine engine(storage_, "release/node-a", replace_sources);

  try {
    (void)engine.ReferenceRegularRestore(Destination(artifact),
                                         "transaction-a");
    FAIL() << "source replacement unexpectedly succeeded";
  }
  catch (const fs::filesystem_error& error) {
    EXPECT_EQ(error.code().value(), ESTALE);
  }
  EXPECT_TRUE(replaced);
  EXPECT_FALSE(ContainsLeaf(storage_ / ".pagebroker-restore",
                            "transaction-a"));
}

TEST_F(PosixCopyEngineTest, RegularRestoreReferenceRejectsSymlinkAndCleansTree)
{
  const fs::path artifact = storage_ / "artifact";
  fs::create_directories(artifact);
  fs::create_symlink(root_ / "outside", artifact / "image");
  PosixCopyEngine engine(storage_, "release/node-a");

  EXPECT_THROW(
      engine.ReferenceRegularRestore(Destination(artifact), "transaction-a"),
      std::runtime_error);
  for (const auto& entry :
       fs::recursive_directory_iterator(storage_ / ".pagebroker-restore"))
    EXPECT_NE(entry.path().filename(), "transaction-a");
}

TEST_F(PosixCopyEngineTest, RestoreReferenceStartupCleanupIsOwnerScoped)
{
  const fs::path artifact = storage_ / "artifact";
  fs::create_directories(artifact);
  std::ofstream(artifact / "image") << "image";
  fs::path owner_a_reference;
  {
    PosixCopyEngine owner_a(storage_, "release/node-a");
    owner_a_reference =
        owner_a.ReferenceRegularRestore(Destination(artifact), "transaction-a");
    PosixCopyEngine owner_b(storage_, "release/node-b");
    EXPECT_TRUE(fs::exists(owner_a_reference / "image"));
    EXPECT_THROW(PosixCopyEngine(storage_, "release/node-a"),
                 std::filesystem::filesystem_error);
  }
  ASSERT_TRUE(fs::exists(owner_a_reference / "image"));
  PosixCopyEngine recovered_owner_a(storage_, "release/node-a");
  EXPECT_FALSE(fs::exists(owner_a_reference));
}
}  // namespace
