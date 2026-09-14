// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Focused coverage for RESTORE_CREATORS effective-context batching. The test
// drives each shim phase directly so context counters cover creator restore
// alone rather than host-carrier setup or cleanup.

#include <cuda.h>
#include <gtest/gtest.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#include <cstdio>
#include <cstdint>
#include <cstdlib>
#include <string>
#include <vector>

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

CUmemAllocationProp posix_props(CUdevice device = 0) {
  CUmemAllocationProp properties{};
  properties.type = CU_MEM_ALLOCATION_TYPE_PINNED;
  properties.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  properties.location.id = device;
  properties.requestedHandleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  return properties;
}

bool request_succeeds(uint16_t operation, const char* participant, std::string* error) {
  const struct cuinterpose_header reply = request_own_shim(operation, participant);
  *error = reply.message;
  return reply.status == 0;
}

TEST(RestoreCreatorsBatch, EntersEachEffectiveContextOnce) {
  fakeEnableTrackedBehavior();
  fakeResetModel();
  const CUcontext first_context = reinterpret_cast<CUcontext>(static_cast<uintptr_t>(7));
  const CUcontext second_context = reinterpret_cast<CUcontext>(static_cast<uintptr_t>(8));
  std::vector<CUmemGenericAllocationHandle> handles(6);

  CUmemAllocationProp properties = posix_props();
  ASSERT_EQ(cuCtxSetCurrent(first_context), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&handles[0], 1 << 20, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&handles[1], 1 << 19, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x10000000, 1 << 20, 0, handles[0], 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x20000000, 1 << 19, 0, handles[1], 0), CUDA_SUCCESS);

  ASSERT_EQ(cuCtxSetCurrent(second_context), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&handles[2], 1 << 18, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x30000000, 1 << 18, 0, handles[2], 0), CUDA_SUCCESS);

  ASSERT_EQ(cuCtxSetCurrent(nullptr), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&handles[3], 1 << 17, &properties, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&handles[4], 1 << 16, &properties, 0), CUDA_SUCCESS);
  properties = posix_props(1);
  ASSERT_EQ(cuMemCreate(&handles[5], 1 << 15, &properties, 0), CUDA_SUCCESS);
  for (size_t index : {0U, 3U}) {
    int ticket = -1;
    ASSERT_EQ(cuMemExportToShareableHandle(
                  &ticket, handles[index], CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0),
              CUDA_SUCCESS);
    ASSERT_GE(ticket, 0);
    close(ticket);
  }

  const struct cuinterpose_header identity = request_own_shim(CUINTERPOSE_IDENTIFY, nullptr);
  ASSERT_EQ(identity.status, 0) << identity.message;
  std::string error;
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_PREPARE_MULTICAST, identity.participant_id, &error)) << error;
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_SAVE_HOST_CARRIER, identity.participant_id, &error)) << error;
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_PREPARE, identity.participant_id, &error)) << error;
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_RESTORE_HOST_CARRIER, identity.participant_id, &error)) << error;
  // RESTORE_HOST_CARRIER replies before deferred arena cleanup. A following
  // request is handled only after that cleanup, making the counter reset exact.
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_IDENTIFY, nullptr, &error)) << error;

  fakeResetContextSetCalls();
  const int exports_before_restore = fakeExportCalls();
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_RESTORE_CREATORS, identity.participant_id, &error)) << error;
  EXPECT_EQ(fakeContextSetCalls(first_context), 1)
      << "two creators in one explicit context must share one restore entry";
  EXPECT_EQ(fakeContextSetCalls(second_context), 1)
      << "a distinct explicit context must have its own restore entry";
  EXPECT_EQ(fakeContextSetCalls(reinterpret_cast<CUcontext>(static_cast<uintptr_t>(0x100))), 1)
      << "two context-less device-0 creators must share one primary-context entry";
  EXPECT_EQ(fakeContextSetCalls(reinterpret_cast<CUcontext>(static_cast<uintptr_t>(0x101))), 1)
      << "a distinct fallback device must have its own primary-context entry";
  EXPECT_EQ(fakeContextSetCalls(nullptr), 4)
      << "each effective-context group must restore the control thread's prior context once";
  EXPECT_EQ(fakePrimaryContextsHeld(), 0);
  EXPECT_EQ(fakeExportCalls(), exports_before_restore + 2)
      << "shared creators in distinct context groups must both repopulate the export cache";

  ASSERT_TRUE(request_succeeds(CUINTERPOSE_RESTORE_IMPORTERS, identity.participant_id, &error)) << error;
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_RESTORE_MULTICAST_CREATORS, identity.participant_id, &error)) << error;
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_RESTORE_MULTICAST_IMPORTERS, identity.participant_id, &error)) << error;
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_RESTORE_MULTICAST_DEVICES, identity.participant_id, &error)) << error;
  ASSERT_TRUE(request_succeeds(CUINTERPOSE_RESTORE_MULTICAST, identity.participant_id, &error)) << error;

  ASSERT_EQ(cuMemUnmap(0x10000000, 1 << 20), CUDA_SUCCESS);
  ASSERT_EQ(cuMemUnmap(0x20000000, 1 << 19), CUDA_SUCCESS);
  ASSERT_EQ(cuMemUnmap(0x30000000, 1 << 18), CUDA_SUCCESS);
  for (CUmemGenericAllocationHandle handle : handles)
    EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

}  // namespace
