// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Focused coverage for PREPARE's context batching. The test drives the shim's
// phases directly so its context-switch counters cover PREPARE alone, without
// depending on the carrier implementation's own context entries.

#include <cuda.h>
#include <gtest/gtest.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#include <cstdio>
#include <cstdint>
#include <cstdlib>
#include <string>

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

CUmemAllocationProp posix_props() {
  CUmemAllocationProp properties{};
  properties.type = CU_MEM_ALLOCATION_TYPE_PINNED;
  properties.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  properties.requestedHandleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  return properties;
}

TEST(PrepareBatch, EntersEachEffectiveContextOnce) {
  fakeEnableTrackedBehavior();
  fakeResetModel();
  CUmemAllocationProp properties = posix_props();
  CUcontext first_context = reinterpret_cast<CUcontext>(static_cast<uintptr_t>(7));
  CUcontext second_context = reinterpret_cast<CUcontext>(static_cast<uintptr_t>(8));
  CUmemGenericAllocationHandle first = 0, second = 0, third = 0;
  CUmemGenericAllocationHandle device_zero_a = 0, device_zero_b = 0, device_one = 0;

  ASSERT_EQ(cuCtxSetCurrent(first_context), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&first, 1 << 20, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&second, 1 << 19, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x10000000, 1 << 20, 0, first, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x20000000, 1 << 19, 0, second, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuCtxSetCurrent(second_context), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&third, 1 << 18, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x30000000, 1 << 18, 0, third, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuCtxSetCurrent(nullptr), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&device_zero_a, 1 << 17, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&device_zero_b, 1 << 16, &properties, 0), CUDA_SUCCESS);
  properties.location.id = 1;
  ASSERT_EQ(cuMemCreate(&device_one, 1 << 15, &properties, 0), CUDA_SUCCESS);

  struct cuinterpose_header identity = request_own_shim(CUINTERPOSE_IDENTIFY, nullptr);
  ASSERT_EQ(identity.status, 0) << identity.message;
  struct cuinterpose_header carriers =
      request_own_shim(CUINTERPOSE_PREPARE_MULTICAST, identity.participant_id);
  ASSERT_EQ(carriers.status, 0) << carriers.message;
  struct cuinterpose_header saved =
      request_own_shim(CUINTERPOSE_SAVE_HOST_CARRIER, identity.participant_id);
  ASSERT_EQ(saved.status, 0) << saved.message;

  fakeResetContextSetCalls();
  struct cuinterpose_header prepared = request_own_shim(CUINTERPOSE_PREPARE, identity.participant_id);
  ASSERT_EQ(prepared.status, 0) << prepared.message;
  EXPECT_EQ(fakeContextSetCalls(first_context), 1)
      << "two allocations in one effective context must share one PREPARE entry";
  EXPECT_EQ(fakeContextSetCalls(second_context), 1)
      << "the other effective context must have its own PREPARE entry";
  EXPECT_EQ(fakeContextSetCalls(reinterpret_cast<CUcontext>(static_cast<uintptr_t>(0x100))), 1)
      << "two context-less device-0 allocations must share one primary-context entry";
  EXPECT_EQ(fakeContextSetCalls(reinterpret_cast<CUcontext>(static_cast<uintptr_t>(0x101))), 1)
      << "the context-less device-1 allocation must use a separate primary-context entry";
}

}  // namespace
