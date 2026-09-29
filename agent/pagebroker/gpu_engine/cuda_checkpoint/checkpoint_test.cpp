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
bool completion_available;
int completion_queries;
bool requested_storage;
CUcheckpointGpuPair* restore_pairs;
CUcheckpointCustomStorageInfo view;

class NativeOperationTest : public testing::Test {
 protected:
  void SetUp() override {
    calls.clear();
    fail_prepare = false;
    fail_complete = false;
    completion_available = true;
    completion_queries = 0;
    requested_storage = false;
    view = {};
    view.handle = reinterpret_cast<CUcheckpointOperationHandle>(1);
    pid = fork();
    ASSERT_GE(pid, 0);
    if (!pid) { for (;;) pause(); }
  }
  void TearDown() override {
    if (pid > 0) { kill(pid, SIGKILL); waitpid(pid, nullptr, 0); }
  }
  int pid = -1;
};
}  // namespace

extern "C" {
CUresult CUDAAPI cuInit(unsigned int) { calls.push_back(0); return CUDA_SUCCESS; }
CUresult CUDAAPI cuGetErrorName(CUresult, const char** name) { *name = "test failure"; return CUDA_SUCCESS; }
CUresult CUDAAPI cuCheckpointOperationComplete(CUcheckpointOperationHandle handle) {
  EXPECT_EQ(handle, view.handle);
  calls.push_back(4);
  return fail_complete ? CUDA_ERROR_UNKNOWN : CUDA_SUCCESS;
}
CUresult CUDAAPI cuGetProcAddress(const char*, void** fn, int, cuuint64_t, CUdriverProcAddressQueryResult* query) {
  ++completion_queries;
  if (!completion_available) {
    *fn = nullptr;
    return CUDA_ERROR_NOT_SUPPORTED;
  }
  *fn = reinterpret_cast<void*>(&cuCheckpointOperationComplete);
  *query = CU_GET_PROC_ADDRESS_SUCCESS;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuCheckpointProcessLock(int, CUcheckpointLockArgs*) { calls.push_back(1); return CUDA_SUCCESS; }
CUresult CUDAAPI cuCheckpointProcessCheckpoint(int, CUcheckpointCheckpointArgs* args) {
  calls.push_back(2);
  requested_storage = args->customStorageInfo_out != nullptr;
  if (requested_storage) *args->customStorageInfo_out = &view;
  return fail_prepare ? CUDA_ERROR_UNKNOWN : CUDA_SUCCESS;
}
CUresult CUDAAPI cuCheckpointProcessRestore(int, CUcheckpointRestoreArgs* args) {
  calls.push_back(3);
  restore_pairs = args->gpuPairs;
  requested_storage = args->customStorageInfo_out != nullptr;
  if (requested_storage) *args->customStorageInfo_out = &view;
  return fail_prepare ? CUDA_ERROR_UNKNOWN : CUDA_SUCCESS;
}
CUresult CUDAAPI cuCheckpointProcessUnlock(int, CUcheckpointUnlockArgs*) { calls.push_back(5); return CUDA_SUCCESS; }
CUresult CUDAAPI cuCheckpointProcessGetState(int, CUprocessState* state) {
  *state = CU_PROCESS_STATE_RUNNING;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuCheckpointProcessGetRestoreThreadId(int, int* tid) {
  *tid = 42;
  return CUDA_SUCCESS;
}
}

TEST_F(NativeOperationTest, ReusesInitializedApiAcrossCaptureAndRestore) {
  snapshot::cuda_checkpoint::CheckpointAPI api;
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

TEST_F(NativeOperationTest, FailedPreparationStillDrainsReturnedDriverHandle) {
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

TEST_F(NativeOperationTest, UnstartedOperationDoesNotTerminateTarget) {
  snapshot::cuda_checkpoint::CheckpointAPI api;
  snapshot::cuda_checkpoint::Operation operation(api, pid);
  operation.Abort();
  EXPECT_EQ(kill(pid, 0), 0);
  EXPECT_EQ(calls, (std::vector<int>{0}));
}

TEST_F(NativeOperationTest, DriverManagedDoesNotResolveOrCompleteCustomStorage) {
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
  EXPECT_EQ(completion_queries, 0);
  EXPECT_EQ(calls, (std::vector<int>{0, 1, 2, 3, 5}));
}

TEST_F(NativeOperationTest, UnsupportedCustomStorageDoesNotTouchTarget) {
  completion_available = false;
  snapshot::cuda_checkpoint::CheckpointAPI api;
  snapshot::cuda_checkpoint::Operation operation(api, pid);
  EXPECT_THROW(operation.Prepare(false, {}, true), std::runtime_error);
  operation.Abort();
  EXPECT_EQ(kill(pid, 0), 0);
  EXPECT_EQ(calls, (std::vector<int>{0}));
}

TEST_F(NativeOperationTest, FailedDriverManagedPreparationTerminatesWithoutCustomCompletion) {
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
  EXPECT_EQ(completion_queries, 0);
  EXPECT_EQ(calls, (std::vector<int>{0, 3}));
}

TEST_F(NativeOperationTest, FailedCompletionTerminatesTargetAndRequiresProcessTeardown) {
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
  EXPECT_EQ(calls, (std::vector<int>{0, 3, 4}));  // Never reuse the uncertain handle.
}
