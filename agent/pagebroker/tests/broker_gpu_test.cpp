// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "tests/broker_transfer_fixture.hpp"

#include <fcntl.h>

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
}  // namespace snapshot::pagebroker
