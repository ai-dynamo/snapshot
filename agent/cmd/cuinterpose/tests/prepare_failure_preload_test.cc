// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// PREPARE is destructive once teardown starts. A driver failure therefore
// fails the shim closed and subsequent requests must never retry teardown.

#include <cuda.h>
#include <gtest/gtest.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#include <atomic>
#include <cstdio>
#include <cstdint>
#include <cstdlib>
#include <string>
#include <thread>

#include "../protocol.h"
#include "fake_cuda.h"

namespace {

struct cuinterpose_header request_own_shim(uint16_t operation, const char* participant) {
  struct cuinterpose_header request{};
  struct cuinterpose_header reply{};
  reply.status = -1;
  request.magic = CUINTERPOSE_MAGIC;
  request.version = CUINTERPOSE_VERSION;
  request.operation = operation;
  if (participant != nullptr)
    snprintf(request.participant_id, sizeof(request.participant_id), "%s", participant);

  const std::string path = std::string(getenv("SNAPSHOT_CONTROL_DIR")) + "/" +
                           CUINTERPOSE_SOCKET_PREFIX + std::to_string(getpid()) + ".sock";
  int fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (fd < 0)
    return reply;
  struct sockaddr_un address{};
  address.sun_family = AF_UNIX;
  snprintf(address.sun_path, sizeof(address.sun_path), "%s", path.c_str());
  if (connect(fd, reinterpret_cast<struct sockaddr*>(&address), sizeof(address)) == 0 &&
      send(fd, &request, sizeof(request), MSG_NOSIGNAL) == static_cast<ssize_t>(sizeof(request))) {
    size_t received = 0;
    while (received < sizeof(reply)) {
      ssize_t count = recv(fd, reinterpret_cast<char*>(&reply) + received, sizeof(reply) - received, 0);
      if (count <= 0)
        break;
      received += static_cast<size_t>(count);
    }
  }
  close(fd);
  return reply;
}

TEST(PrepareFailure, FailsClosedWithoutRetryingTeardown) {
  fakeEnableTrackedBehavior();
  fakeResetModel();
  ASSERT_EQ(cuCtxSetCurrent(reinterpret_cast<CUcontext>(static_cast<uintptr_t>(1))), CUDA_SUCCESS);
  CUmemAllocationProp properties{};
  properties.type = CU_MEM_ALLOCATION_TYPE_PINNED;
  properties.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  properties.requestedHandleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  CUmemGenericAllocationHandle first = 0, second = 0;
  ASSERT_EQ(cuMemCreate(&first, 1 << 20, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x10000000, 1 << 20, 0, first, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuCtxSetCurrent(reinterpret_cast<CUcontext>(static_cast<uintptr_t>(2))), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&second, 1 << 19, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x20000000, 1 << 19, 0, second, 0), CUDA_SUCCESS);

  struct cuinterpose_header identity = request_own_shim(CUINTERPOSE_IDENTIFY, nullptr);
  ASSERT_EQ(identity.status, 0) << identity.message;
  struct cuinterpose_header carriers =
      request_own_shim(CUINTERPOSE_PREPARE_MULTICAST, identity.participant_id);
  ASSERT_EQ(carriers.status, 0) << carriers.message;
  struct cuinterpose_header saved =
      request_own_shim(CUINTERPOSE_SAVE_HOST_CARRIER, identity.participant_id);
  ASSERT_EQ(saved.status, 0) << saved.message;

  std::atomic<bool> stop_observer{false};
  std::atomic<unsigned int> observations{0};
  std::atomic<unsigned int> incoherent_observations{0};
  std::atomic<unsigned int> failed_observations{0};
  std::thread observer([&] {
    while (!stop_observer.load(std::memory_order_acquire)) {
      struct cuinterpose_header observed = request_own_shim(CUINTERPOSE_IDENTIFY, nullptr);
      const bool preparing = observed.status == 0 && observed.phase == CUINTERPOSE_PHASE_PREPARING;
      const bool failed = observed.status != 0 && observed.phase == CUINTERPOSE_PHASE_FAILED &&
                          std::string(observed.message).find("cuMemUnmap failed during prepare") != std::string::npos;
      if (!preparing && !failed)
        incoherent_observations.fetch_add(1, std::memory_order_relaxed);
      if (failed)
        failed_observations.fetch_add(1, std::memory_order_relaxed);
      observations.fetch_add(1, std::memory_order_release);
    }
  });
  for (int attempt = 0; attempt < 2000 && observations.load(std::memory_order_acquire) < 16; attempt++)
    usleep(1000);

  int unmaps_before_prepare = fakeUnmapCalls();
  fakeFailOnNthCall("cuMemUnmap", 2);
  struct cuinterpose_header failed = request_own_shim(CUINTERPOSE_PREPARE, identity.participant_id);
  for (int attempt = 0; attempt < 2000 && failed_observations.load(std::memory_order_acquire) == 0; attempt++)
    usleep(1000);
  stop_observer.store(true, std::memory_order_release);
  observer.join();

  ASSERT_NE(failed.status, 0);
  EXPECT_GE(observations.load(std::memory_order_relaxed), 16u)
      << "the concurrent IDENTIFY observer did not start before PREPARE";
  EXPECT_NE(std::string(failed.message).find("cuMemUnmap failed during prepare"), std::string::npos)
      << failed.message;
  EXPECT_EQ(fakeUnmapCalls(), unmaps_before_prepare + 2);
  EXPECT_EQ(fakeMappedCount(), 1) << "PREPARE must expose the partial teardown that makes retry unsafe";
  EXPECT_FALSE(fakeIsMapped(0x10000000)) << "the first context was torn down before the injected failure";
  EXPECT_TRUE(fakeIsMapped(0x20000000)) << "the second unmap failed before mutating its mapping";
  EXPECT_EQ(incoherent_observations.load(std::memory_order_relaxed), 0u)
      << "IDENTIFY must return one coherent state snapshot while PREPARE transitions to failed";
  EXPECT_GT(failed_observations.load(std::memory_order_relaxed), 0u)
      << "the concurrent observer did not see the terminal failed state";

  struct cuinterpose_header failed_identity = request_own_shim(CUINTERPOSE_IDENTIFY, nullptr);
  EXPECT_NE(failed_identity.status, 0);
  EXPECT_EQ(failed_identity.phase, CUINTERPOSE_PHASE_FAILED);
  int unmaps_after_failure = fakeUnmapCalls();
  struct cuinterpose_header retry = request_own_shim(CUINTERPOSE_PREPARE, identity.participant_id);
  EXPECT_NE(retry.status, 0);
  EXPECT_EQ(fakeUnmapCalls(), unmaps_after_failure) << "failed PREPARE must never reach driver teardown again";
}

}  // namespace
