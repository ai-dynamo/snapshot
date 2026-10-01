// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "checkpoint.hpp"
#include <gtest/gtest.h>
#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

namespace {
std::vector<int> calls;
bool fail_prepare;
bool fail_complete;
CUresult unlock_result;
CUresult state_result;
CUprocessState reported_state;
bool completion_available;
int completion_queries;
CUresult query_result;
CUdriverProcAddressQueryResult query_status;
int driver_version;
bool requested_storage;
CUcheckpointGpuPair* restore_pairs;
CUcheckpointCustomStorageInfo view;

class GPUOperationTest : public testing::Test {
protected:
  void
  SetUp() override
  {
    calls.clear();
    fail_prepare = false;
    fail_complete = false;
    unlock_result = state_result = CUDA_SUCCESS;
    reported_state = CU_PROCESS_STATE_RUNNING;
    completion_available = true;
    completion_queries = 0;
    query_result = CUDA_SUCCESS;
    query_status = CU_GET_PROC_ADDRESS_SUCCESS;
    driver_version = 13040;
    requested_storage = false;
    view = {};
    view.handle = reinterpret_cast<CUcheckpointOperationHandle>(1);
    pid = fork();
    ASSERT_GE(pid, 0);
    if (!pid) {
      for (;;)
        pause();
    }
  }
  void
  TearDown() override
  {
    if (pid > 0) {
      kill(pid, SIGKILL);
      waitpid(pid, nullptr, 0);
    }
  }
  int pid = -1;
};
} // namespace

extern "C" {
CUresult CUDAAPI
cuInit(unsigned int)
{
  calls.push_back(0);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuGetErrorName(CUresult, const char** name)
{
  *name = "test failure";
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointOperationComplete(CUcheckpointOperationHandle handle)
{
  EXPECT_EQ(handle, view.handle);
  calls.push_back(4);
  return fail_complete ? CUDA_ERROR_UNKNOWN : CUDA_SUCCESS;
}
CUresult CUDAAPI
cuGetProcAddress(const char*, void** fn, int, cuuint64_t, CUdriverProcAddressQueryResult* query)
{
  ++completion_queries;
  if (query_result != CUDA_SUCCESS)
    return query_result;
  if (!completion_available) {
    *fn = nullptr;
    *query = query_status == CU_GET_PROC_ADDRESS_SUCCESS ? CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND : query_status;
    return CUDA_SUCCESS;
  }
  *fn = reinterpret_cast<void*>(&cuCheckpointOperationComplete);
  *query = CU_GET_PROC_ADDRESS_SUCCESS;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuDriverGetVersion(int* version)
{
  *version = driver_version;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointProcessLock(int, CUcheckpointLockArgs* args)
{
  EXPECT_EQ(args->timeoutMs, 10000U);
  calls.push_back(1);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointProcessCheckpoint(int, CUcheckpointCheckpointArgs* args)
{
  calls.push_back(2);
  requested_storage = args->customStorageInfo_out != nullptr;
  if (requested_storage)
    *args->customStorageInfo_out = &view;
  return fail_prepare ? CUDA_ERROR_UNKNOWN : CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointProcessRestore(int, CUcheckpointRestoreArgs* args)
{
  calls.push_back(3);
  restore_pairs = args->gpuPairs;
  requested_storage = args->customStorageInfo_out != nullptr;
  if (requested_storage)
    *args->customStorageInfo_out = &view;
  return fail_prepare ? CUDA_ERROR_UNKNOWN : CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointProcessUnlock(int, CUcheckpointUnlockArgs*)
{
  calls.push_back(5);
  return unlock_result;
}
CUresult CUDAAPI
cuCheckpointProcessGetState(int, CUprocessState* state)
{
  calls.push_back(6);
  *state = reported_state;
  return state_result;
}
}

TEST_F(GPUOperationTest, ReusesInitializedApiAcrossCaptureAndRestore)
{
  snapshot::cuda_checkpoint::CheckpointAPI api;
  EXPECT_EQ(completion_queries, 1);
  {
    snapshot::cuda_checkpoint::Operation operation(api, pid);
    operation.CheckTarget();
    operation.Lock();
    EXPECT_EQ(operation.Prepare(true, {}), &view);
    operation.Complete();
  }
  {
    snapshot::cuda_checkpoint::Operation operation(api, pid);
    CUcheckpointGpuPair pair{};
    EXPECT_EQ(operation.Prepare(false, {&pair, 1}), &view);
    EXPECT_EQ(restore_pairs, &pair);
    operation.Complete();
    operation.Unlock();
  }
  EXPECT_EQ(calls, (std::vector<int>{0, 1, 2, 4, 3, 4, 5}));
  EXPECT_EQ(kill(pid, 0), 0);
}

TEST_F(GPUOperationTest, FailedPreparationStillDrainsReturnedDriverHandle)
{
  snapshot::cuda_checkpoint::CheckpointAPI api;
  snapshot::cuda_checkpoint::Operation operation(api, pid);
  fail_prepare = true;
  EXPECT_THROW(operation.Prepare(false, {}), std::runtime_error);
  operation.Abort();
  int status = 0;
  ASSERT_EQ(waitpid(pid, &status, 0), pid);
  EXPECT_TRUE(WIFSIGNALED(status));
  EXPECT_EQ(WTERMSIG(status), SIGKILL);
  pid = -1;
  EXPECT_EQ(calls, (std::vector<int>{0, 3, 4}));
}

TEST_F(GPUOperationTest, UnstartedOperationDoesNotTerminateTarget)
{
  snapshot::cuda_checkpoint::CheckpointAPI api;
  snapshot::cuda_checkpoint::Operation operation(api, pid);
  operation.Abort();
  EXPECT_EQ(kill(pid, 0), 0);
  EXPECT_EQ(calls, (std::vector<int>{0}));
}

TEST_F(GPUOperationTest, DriverManagedUsesCachedAbsentCapabilityWithoutCustomCompletion)
{
  completion_available = false;
  snapshot::cuda_checkpoint::CheckpointAPI api;
  {
    snapshot::cuda_checkpoint::Operation operation(api, pid);
    operation.Lock();
    EXPECT_EQ(operation.Prepare(true, {}, false), nullptr);
    EXPECT_FALSE(requested_storage);
    operation.Complete();
  }
  {
    snapshot::cuda_checkpoint::Operation operation(api, pid);
    CUcheckpointGpuPair pair{};
    EXPECT_EQ(operation.Prepare(false, {&pair, 1}, false), nullptr);
    EXPECT_EQ(restore_pairs, &pair);
    EXPECT_FALSE(requested_storage);
    operation.Complete();
    operation.Unlock();
  }
  EXPECT_EQ(completion_queries, 1);
  EXPECT_EQ(calls, (std::vector<int>{0, 1, 2, 3, 5}));
}

TEST_F(GPUOperationTest, UnsupportedCustomStorageDoesNotTouchTarget)
{
  completion_available = false;
  snapshot::cuda_checkpoint::CheckpointAPI api;
  snapshot::cuda_checkpoint::Operation operation(api, pid);
  EXPECT_THROW(operation.Prepare(false, {}, true), std::runtime_error);
  operation.Abort();
  EXPECT_EQ(kill(pid, 0), 0);
  EXPECT_EQ(calls, (std::vector<int>{0}));
}

TEST_F(GPUOperationTest, FailedDriverManagedPreparationTerminatesWithoutCustomCompletion)
{
  snapshot::cuda_checkpoint::CheckpointAPI api;
  snapshot::cuda_checkpoint::Operation operation(api, pid);
  fail_prepare = true;
  EXPECT_THROW(operation.Prepare(false, {}, false), std::runtime_error);
  operation.Abort();
  int status = 0;
  ASSERT_EQ(waitpid(pid, &status, 0), pid);
  EXPECT_TRUE(WIFSIGNALED(status));
  EXPECT_EQ(WTERMSIG(status), SIGKILL);
  pid = -1;
  EXPECT_EQ(completion_queries, 1);
  EXPECT_EQ(calls, (std::vector<int>{0, 3}));
}

TEST_F(GPUOperationTest, FailedCompletionTerminatesTargetAndRequiresProcessTeardown)
{
  snapshot::cuda_checkpoint::CheckpointAPI api;
  snapshot::cuda_checkpoint::Operation operation(api, pid);
  operation.Prepare(false, {});
  fail_complete = true;
  EXPECT_THROW(operation.Complete(), snapshot::cuda_checkpoint::CompletionUncertain);
  EXPECT_THROW(operation.Complete(), snapshot::cuda_checkpoint::CompletionUncertain);
  EXPECT_THROW(operation.Unlock(), snapshot::cuda_checkpoint::CompletionUncertain);
  EXPECT_THROW(operation.Abort(), snapshot::cuda_checkpoint::CompletionUncertain);
  int status = 0;
  ASSERT_EQ(waitpid(pid, &status, 0), pid);
  EXPECT_TRUE(WIFSIGNALED(status));
  EXPECT_EQ(WTERMSIG(status), SIGKILL);
  pid = -1;
  EXPECT_EQ(calls, (std::vector<int>{0, 3, 4})); // Never reuse the uncertain handle.
}

TEST_F(GPUOperationTest, UnlockErrorRequiresConfirmationThatTargetIsRunning)
{
  snapshot::cuda_checkpoint::CheckpointAPI api;
  snapshot::cuda_checkpoint::Operation operation(api, pid);
  unlock_result = CUDA_ERROR_INVALID_VALUE;
  EXPECT_NO_THROW(operation.Unlock());
  reported_state = CU_PROCESS_STATE_LOCKED;
  EXPECT_THROW(operation.Unlock(), std::runtime_error);
  reported_state = CU_PROCESS_STATE_RUNNING;
  state_result = CUDA_ERROR_NOT_INITIALIZED;
  EXPECT_THROW(operation.Unlock(), std::runtime_error);
  EXPECT_EQ(calls, (std::vector<int>{0, 5, 6, 5, 6, 5, 6}));
}

TEST_F(GPUOperationTest, CustomStorageCapabilityIsCachedForBothAvailableAndAbsentDrivers)
{
  for (const bool available : {true, false}) {
    completion_available = available;
    const auto before = completion_queries;
    snapshot::cuda_checkpoint::CheckpointAPI api;
    EXPECT_EQ(api.SupportsCustomStorage(), available);
    for (int i = 0; i < 2; ++i) {
      if (available)
        EXPECT_NO_THROW(api.RequireCustomStorage());
      else
        EXPECT_THROW(api.RequireCustomStorage(), std::runtime_error);
    }
    EXPECT_EQ(completion_queries, before + 1);
  }
}

TEST_F(GPUOperationTest, OlderDriverAndMissingSymbolAreExplicitlyUnavailable)
{
  query_result = CUDA_ERROR_INVALID_VALUE;
  driver_version = 13000;
  EXPECT_FALSE(snapshot::cuda_checkpoint::CheckpointAPI().SupportsCustomStorage());
  query_result = CUDA_ERROR_NOT_SUPPORTED;
  EXPECT_FALSE(snapshot::cuda_checkpoint::CheckpointAPI().SupportsCustomStorage());
  query_result = CUDA_SUCCESS;
  completion_available = false;
  query_status = CU_GET_PROC_ADDRESS_VERSION_NOT_SUFFICIENT;
  EXPECT_FALSE(snapshot::cuda_checkpoint::CheckpointAPI().SupportsCustomStorage());
}

TEST_F(GPUOperationTest, UnexpectedCapabilityErrorsFailInitialization)
{
  query_result = CUDA_ERROR_UNKNOWN;
  EXPECT_THROW(snapshot::cuda_checkpoint::CheckpointAPI(), std::runtime_error);
  query_result = CUDA_ERROR_INVALID_VALUE;
  driver_version = 13040;
  EXPECT_THROW(snapshot::cuda_checkpoint::CheckpointAPI(), std::runtime_error);
}
