// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// A creator-restore failure after one allocation in a context group succeeded
// must restore the control thread's prior context and fail closed without a
// second attempt to remap or re-export the partial batch.

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

TEST(RestoreCreatorsFailure, LeavesContextAndNeverRetriesPartialBatch) {
  fakeEnableTrackedBehavior();
  fakeResetModel();
  const CUcontext context = reinterpret_cast<CUcontext>(static_cast<uintptr_t>(7));
  const CUmemAllocationProp properties = posix_props();
  CUmemGenericAllocationHandle handles[3] = {};
  const CUdeviceptr addresses[] = {0x10000000, 0x20000000, 0x30000000};

  ASSERT_EQ(cuCtxSetCurrent(context), CUDA_SUCCESS);
  for (size_t index = 0; index < 3; index++) {
    ASSERT_EQ(cuMemCreate(&handles[index], 1 << 20, &properties, 0), CUDA_SUCCESS);
    ASSERT_EQ(cuMemMap(addresses[index], 1 << 20, 0, handles[index], 0), CUDA_SUCCESS);
    int ticket = -1;
    ASSERT_EQ(cuMemExportToShareableHandle(
                  &ticket, handles[index], CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0),
              CUDA_SUCCESS);
    ASSERT_GE(ticket, 0);
    close(ticket);
  }

  const struct cuinterpose_header identity = request_own_shim(CUINTERPOSE_IDENTIFY, nullptr);
  ASSERT_EQ(identity.status, 0) << identity.message;
  struct cuinterpose_header reply =
      request_own_shim(CUINTERPOSE_PREPARE_MULTICAST, identity.participant_id);
  ASSERT_EQ(reply.status, 0) << reply.message;
  reply = request_own_shim(CUINTERPOSE_SAVE_HOST_CARRIER, identity.participant_id);
  ASSERT_EQ(reply.status, 0) << reply.message;
  reply = request_own_shim(CUINTERPOSE_PREPARE, identity.participant_id);
  ASSERT_EQ(reply.status, 0) << reply.message;
  reply = request_own_shim(CUINTERPOSE_RESTORE_HOST_CARRIER, identity.participant_id);
  ASSERT_EQ(reply.status, 0) << reply.message;
  reply = request_own_shim(CUINTERPOSE_IDENTIFY, nullptr);
  ASSERT_EQ(reply.status, 0) << reply.message;

  fakeResetContextSetCalls();
  const int exports_before = fakeExportCalls();
  fakeFailOnNthCall("cuMemMap", 2);
  reply = request_own_shim(CUINTERPOSE_RESTORE_CREATORS, identity.participant_id);
  ASSERT_NE(reply.status, 0);
  EXPECT_NE(std::string(reply.message).find("cuMemMap failed during restore"), std::string::npos)
      << reply.message;
  EXPECT_EQ(fakeContextSetCalls(context), 1)
      << "one context entry must cover the partial creator batch";
  EXPECT_EQ(fakeContextSetCalls(nullptr), 1)
      << "the failed batch must restore the control thread's prior context";
  EXPECT_EQ(fakeMappedCount(), 1)
      << "one creator remapped before the injected second-map failure";
  EXPECT_EQ(fakeExportCalls(), exports_before + 1)
      << "only the successfully remapped creator may be re-exported";

  const struct cuinterpose_header failed_identity = request_own_shim(CUINTERPOSE_IDENTIFY, nullptr);
  EXPECT_NE(failed_identity.status, 0);
  EXPECT_EQ(failed_identity.phase, CUINTERPOSE_PHASE_FAILED);
  const int mappings_after_failure = fakeMappedCount();
  const int exports_after_failure = fakeExportCalls();
  const struct cuinterpose_header retry =
      request_own_shim(CUINTERPOSE_RESTORE_CREATORS, identity.participant_id);
  EXPECT_NE(retry.status, 0);
  EXPECT_EQ(fakeMappedCount(), mappings_after_failure)
      << "failed creator restore must never retry mapping";
  EXPECT_EQ(fakeExportCalls(), exports_after_failure)
      << "failed creator restore must never retry export";
}

}  // namespace
