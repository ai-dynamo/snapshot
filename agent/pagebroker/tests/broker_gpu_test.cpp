// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "tests/broker_transfer_fixture.hpp"

#include <fcntl.h>
#include <signal.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#include <cerrno>
#include <exception>
#include <functional>

namespace snapshot::pagebroker {
namespace {
// Inject GPU execution failures at the broker boundary without requiring CUDA.
thread_local std::function<void(Cancellation&)> gpu_failure;
struct ScopedGpuFailure {
  explicit ScopedGpuFailure(std::function<void(Cancellation&)> failure) { gpu_failure = std::move(failure); }
  ~ScopedGpuFailure() { gpu_failure = {}; }
};

// Report CustomStorage as available so a GPU request reaches execution without CUDA.
thread_local bool gpu_available = false;
struct ScopedGpuAvailability {
  ScopedGpuAvailability() { gpu_available = true; }
  ~ScopedGpuAvailability() { gpu_available = false; }
};

// The broker accepts only a live process other than itself as a GPU target.
class TargetProcess {
 public:
  TargetProcess() : pid_(fork())
  {
    while (pid_ == 0)
      pause();
  }
  ~TargetProcess()
  {
    if (pid_ > 0) {
      kill(pid_, SIGKILL);
      while (waitpid(pid_, nullptr, 0) < 0 && errno == EINTR) {}
    }
  }
  pid_t pid() const { return pid_; }
  std::vector<FileDescriptor> Descriptors() const
  {
    std::vector<FileDescriptor> descriptors;
    descriptors.emplace_back(static_cast<int>(syscall(SYS_pidfd_open, pid_, 0)));
    return descriptors;
  }

 private:
  const pid_t pid_;
};

void TargetGpu(Request& request, pid_t pid)
{
  auto* checkpoint = request.mutable_checkpoint_gpu();
  checkpoint->mutable_context()->add_captured_pids(pid);
  checkpoint->mutable_context()->add_visible_devices("GPU-00112233-4455-6677-8899-aabbccddeeff");
  auto* target = checkpoint->add_targets();
  target->set_captured_pid(pid);
  target->set_target_pid(pid);
}
}

using GpuResults = std::vector<gpu::ParticipantResult>;
GpuResults RealCheckpoint(gpu::GpuEngine*, gpu::Artifact&, const std::vector<gpu::Participant>&, Cancellation&)
    asm("__real__ZN8snapshot10pagebroker3gpu9GpuEngine10CheckpointERNS1_8ArtifactERKSt6vectorINS1_11ParticipantESaIS6_EERNS0_12CancellationE");
GpuResults WrappedCheckpoint(gpu::GpuEngine*, gpu::Artifact&, const std::vector<gpu::Participant>&, Cancellation&)
    asm("__wrap__ZN8snapshot10pagebroker3gpu9GpuEngine10CheckpointERNS1_8ArtifactERKSt6vectorINS1_11ParticipantESaIS6_EERNS0_12CancellationE");
GpuResults RealRestore(gpu::GpuEngine*, gpu::Artifact&, const std::vector<gpu::Participant>&, Cancellation&)
    asm("__real__ZN8snapshot10pagebroker3gpu9GpuEngine7RestoreERNS1_8ArtifactERKSt6vectorINS1_11ParticipantESaIS6_EERNS0_12CancellationE");
GpuResults WrappedRestore(gpu::GpuEngine*, gpu::Artifact&, const std::vector<gpu::Participant>&, Cancellation&)
    asm("__wrap__ZN8snapshot10pagebroker3gpu9GpuEngine7RestoreERNS1_8ArtifactERKSt6vectorINS1_11ParticipantESaIS6_EERNS0_12CancellationE");

bool RealAvailable(const gpu::GpuEngine*) asm("__real__ZNK8snapshot10pagebroker3gpu9GpuEngine9AvailableEv");
bool WrappedAvailable(const gpu::GpuEngine*) asm("__wrap__ZNK8snapshot10pagebroker3gpu9GpuEngine9AvailableEv");

bool WrappedAvailable(const gpu::GpuEngine* engine)
{
  return gpu_available || RealAvailable(engine);
}

GpuResults WrappedCheckpoint(gpu::GpuEngine* engine, gpu::Artifact& artifact,
    const std::vector<gpu::Participant>& targets, Cancellation& cancellation)
{
  if (gpu_failure)
    gpu_failure(cancellation);
  return RealCheckpoint(engine, artifact, targets, cancellation);
}

GpuResults WrappedRestore(gpu::GpuEngine* engine, gpu::Artifact& artifact,
    const std::vector<gpu::Participant>& targets, Cancellation& cancellation)
{
  if (gpu_failure)
    gpu_failure(cancellation);
  return RealRestore(engine, artifact, targets, cancellation);
}

TEST_F(BrokerTransferTest, GpuExecutionPreservesCancellationAndOtherFailureCodes)
{
  const struct {
    std::exception_ptr error;
    Failure::Code code;
  } failures[] = {
      {nullptr, Failure::TRANSACTION_EXPIRED},
      {nullptr, Failure::INTERNAL_ERROR},
      {std::make_exception_ptr(std::runtime_error("GPU failure")), Failure::INTERNAL_ERROR},
      {std::make_exception_ptr(gpu::TargetConflict("target busy")), Failure::TRANSACTION_CONFLICT},
  };
  for (auto direction : {gpu::Direction::Checkpoint, gpu::Direction::Restore}) {
    for (const auto& failure : failures) {
      SCOPED_TRACE(static_cast<int>(direction));
      SCOPED_TRACE(Failure::Code_Name(failure.code));
      Broker broker(staging_, storage_, std::make_shared<gpu::GpuEngine>());
      auto operation = std::make_shared<Transaction::GpuOperation>();
      operation->request.direction = direction;
      // The wrapper fails before accessing artifact contents in either direction.
      test::TemporaryDirectory capture;
      FileDescriptor directory(open(capture.path().c_str(), O_DIRECTORY | O_RDONLY | O_CLOEXEC));
      ASSERT_GE(directory.get(), 0);
      operation->artifact = std::make_unique<gpu::Artifact>(directory.get(), gpu::Direction::Checkpoint,
          std::vector<uint32_t>{1}, std::vector<std::string>{"GPU-00112233-4455-6677-8899-aabbccddeeff"});
      Cancellation request, transaction;
      if (!failure.error) {
        if (failure.code == Failure::TRANSACTION_EXPIRED)
          transaction = transaction.WithDeadline(Cancellation::Clock::now() - std::chrono::seconds(1));
        else
          transaction.Cancel();
      }
      operation->cancellation = std::make_shared<Cancellation>(Cancellation::Join(request, transaction));
      unsigned calls = 0;
      ScopedGpuFailure inject([&](Cancellation& cancellation) {
        ++calls;
        if (failure.error)
          std::rethrow_exception(failure.error);
        cancellation.ThrowIfCancelled();
      });
      const auto response = ExecuteGpu(broker, operation);
      ASSERT_TRUE(response.has_failure());
      EXPECT_EQ(response.failure().code(), failure.code);
      EXPECT_EQ(calls, 1u);
      EXPECT_FALSE(request.IsCancelled());
      EXPECT_EQ(operation->state, failure.code == Failure::TRANSACTION_CONFLICT
          ? Transaction::GpuOperation::State::Ready : Transaction::GpuOperation::State::Finished);
      EXPECT_EQ(operation->result.SerializeAsString(), response.SerializeAsString());
    }
  }
}

TEST_F(BrokerTransferTest, GpuRequestStopsWhenEitherItsRequestOrItsTransactionIsCancelled)
{
  enum class Source { Request, Transaction };
  for (const auto source : {Source::Request, Source::Transaction}) {
    SCOPED_TRACE(source == Source::Request ? "request" : "transaction");
    Broker broker(staging_, storage_, std::make_shared<gpu::GpuEngine>());
    ScopedGpuAvailability available;
    ASSERT_TRUE(broker.HandleRequest(Checkpoint(true)).has_direct_checkpoint_directory());
    const auto transaction = Find(broker, "checkpoint");
    TargetProcess target;
    ASSERT_GT(target.pid(), 0);
    auto work = RequestFor("checkpoint");
    TargetGpu(work, target.pid());
    const auto request = std::make_shared<Cancellation>();
    unsigned calls = 0;
    ScopedGpuFailure inject([&](Cancellation& operation) {
      ++calls;
      EXPECT_FALSE(operation.IsCancelled());
      if (source == Source::Request)
        request->Cancel();
      else
        transaction->CancelWork();
      EXPECT_TRUE(operation.IsCancelled());
      operation.ThrowIfCancelled();
    });
    const auto response = broker.HandleGpuRequest(work, request, target.Descriptors());
    ASSERT_TRUE(response.has_failure()) << response.DebugString();
    EXPECT_EQ(response.failure().code(), Failure::INTERNAL_ERROR);
    EXPECT_EQ(response.failure().message(), "operation cancelled");
    EXPECT_EQ(calls, 1u);
    EXPECT_EQ(request->IsCancelled(), source == Source::Request);
    EXPECT_EQ(transaction->cancellation().IsCancelled(), source == Source::Transaction);
    auto abort = RequestFor("checkpoint");
    abort.mutable_abort();
    const auto aborted = broker.HandleRequest(abort);
    EXPECT_TRUE(aborted.has_abort_complete()) << aborted.DebugString();
    EXPECT_FALSE(fs::exists(CheckpointOutput(true)));
  }
}

TEST_F(BrokerTransferTest, GpuRetryFollowsItsOwnRequestNotThePreviousOne)
{
  Broker broker(staging_, storage_, std::make_shared<gpu::GpuEngine>());
  ScopedGpuAvailability available;
  ASSERT_TRUE(broker.HandleRequest(Checkpoint(true)).has_direct_checkpoint_directory());
  TargetProcess target;
  ASSERT_GT(target.pid(), 0);
  auto work = RequestFor("checkpoint");
  TargetGpu(work, target.pid());
  const auto previous = std::make_shared<Cancellation>();
  {
    ScopedGpuFailure conflict([](Cancellation&) { throw gpu::TargetConflict("target busy"); });
    const auto response = broker.HandleGpuRequest(work, previous, target.Descriptors());
    ASSERT_TRUE(response.has_failure()) << response.DebugString();
    ASSERT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT);
  }
  const auto retry = std::make_shared<Cancellation>();
  ScopedGpuFailure inject([&](Cancellation& operation) {
    previous->Cancel();
    EXPECT_FALSE(operation.IsCancelled());
    retry->Cancel();
    EXPECT_TRUE(operation.IsCancelled());
    operation.ThrowIfCancelled();
  });
  const auto response = broker.HandleGpuRequest(work, retry, target.Descriptors());
  ASSERT_TRUE(response.has_failure()) << response.DebugString();
  EXPECT_EQ(response.failure().code(), Failure::INTERNAL_ERROR);
  EXPECT_EQ(response.failure().message(), "operation cancelled");
  EXPECT_FALSE(Find(broker, "checkpoint")->cancellation().IsCancelled());
}
}  // namespace snapshot::pagebroker
