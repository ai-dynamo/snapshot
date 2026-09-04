/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// Multicast objects through the shim over the fake driver: two processes (this
// one and a forked child) build a two-device multicast group the way NCCL or
// PyTorch symmetric memory does, then the real coordinator takes it apart and
// puts it back. Also the effective extent (the driver accepting more than was
// asked for), unbinding and rebinding by address, pass-through of non-POSIX
// objects, and the order in which PREPARE_MULTICAST closes the cached
// descriptor and releases the object.

#include <cuda.h>
#include <dlfcn.h>
#include <fcntl.h>
#include <gtest/gtest.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <unistd.h>

#include <cstdio>
#include <cstring>
#include <filesystem>
#include <string>
#include <vector>

#include "../export.h"
#include "../protocol.h"
#include "coordinator_driver.h"
#include "fake_cuda.h"

// cuda.h maps these names to the _v2 entry points; the tests call the names.
#undef cuMulticastBindAddr
#undef cuMulticastBindMem
extern "C" {
CUresult cuMulticastBindMem(
    CUmemGenericAllocationHandle, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
CUresult cuMulticastBindAddr(CUmemGenericAllocationHandle, size_t, CUdeviceptr, size_t, unsigned long long);
}

namespace {

constexpr size_t kMiB = 1 << 20;

struct cuinterpose_debug_stats stats() {
  using fn = void (*)(struct cuinterpose_debug_stats*);
  static fn read = reinterpret_cast<fn>(dlsym(RTLD_DEFAULT, "cuinterpose_debug_stats"));
  struct cuinterpose_debug_stats s{};
  if (read) read(&s);
  return s;
}

CUmemAllocationProp posix_props(int device) {
  CUmemAllocationProp prop{};
  prop.type = CU_MEM_ALLOCATION_TYPE_PINNED;
  prop.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  prop.location.id = device;
  prop.requestedHandleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  return prop;
}

CUmulticastObjectProp multicast_props(unsigned devices, size_t size) {
  CUmulticastObjectProp prop{};
  prop.numDevices = devices;
  prop.size = size;
  prop.handleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  return prop;
}

bool is_logical(CUmemGenericAllocationHandle handle) { return (handle >> 48) == 0xd94d; }

// A mapped, device-accessible allocation.
CUmemGenericAllocationHandle mapped_allocation(int device, CUdeviceptr address, size_t size) {
  CUmemAllocationProp prop = posix_props(device);
  CUmemGenericAllocationHandle handle = 0;
  if (cuMemCreate(&handle, size, &prop, 0) != CUDA_SUCCESS) return 0;
  if (cuMemMap(address, size, 0, handle, 0) != CUDA_SUCCESS) return 0;
  CUmemAccessDesc access{};
  access.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  access.location.id = device;
  access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
  if (cuMemSetAccess(address, size, &access, 1) != CUDA_SUCCESS) return 0;
  return handle;
}

class Multicast : public ::testing::Test {
 protected:
  static void SetUpTestSuite() { fakeEnableTrackedBehavior(); }
  void SetUp() override {
    ASSERT_NE(getenv("CUINTERPOSE_COORDINATOR"), nullptr) << "set CUINTERPOSE_COORDINATOR";
    fakeResetModel();
    char tmpl[] = "/tmp/cuinterpose-multicast-XXXXXX";
    ASSERT_NE(mkdtemp(tmpl), nullptr);
    checkpoint = tmpl;
  }
  void TearDown() override {
    std::error_code ignored;
    std::filesystem::remove_all(checkpoint, ignored);
  }
  std::string checkpoint;
};

enum Command : int { kJoin = 1, kCheckRestored = 2, kRelease = 3, kQuit = 4 };

// Rank 0 (this process) creates a two-device object, attaches device 0, binds
// its own memory with cuMulticastBindMem and maps the object. Rank 1 (the
// child) imports the object through a ticket, attaches device 1, binds its
// slice with cuMulticastBindAddr_v2 and maps it too. Both survive the round
// trip: the fake driver on each side shows the object, its devices, its
// binding and its mapping again, and the export cache holds the object again.
TEST_F(Multicast, TwoRanksCheckpointAndRestoreAGroup) {
#if CUDA_VERSION < 13010
  GTEST_SKIP() << "needs the device-explicit bind entry points";
#else
  CUmemGenericAllocationHandle member = mapped_allocation(0, 0x10000000, kMiB);
  ASSERT_NE(member, 0u);
  CUmulticastObjectProp props = multicast_props(2, kMiB);
  CUmemGenericAllocationHandle group = 0;
  ASSERT_EQ(cuMulticastCreate(&group, &props), CUDA_SUCCESS);
  EXPECT_TRUE(is_logical(group)) << "the application sees a logical handle";
  ASSERT_EQ(cuMulticastAddDevice(group, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMulticastBindMem(group, 0, member, 0, kMiB, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x70000000, kMiB, 0, group, 0), CUDA_SUCCESS);
  CUmemAccessDesc access{};
  access.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
  ASSERT_EQ(cuMemSetAccess(0x70000000, kMiB, &access, 1), CUDA_SUCCESS);
  int member_ticket = -1, group_ticket = -1;
  ASSERT_EQ(cuMemExportToShareableHandle(&member_ticket, member, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemExportToShareableHandle(&group_ticket, group, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);
  EXPECT_EQ(stats().multicasts, 1u);
  EXPECT_EQ(stats().cached_exports, 2u) << "one real export each for the member and the object";
  EXPECT_EQ(fakeMulticastObjects(), 1);
  EXPECT_EQ(fakeMulticastDevices(), 1);
  EXPECT_EQ(fakeMulticastBindings(0), 1);
  EXPECT_EQ(fakeExportCalls(), 2);

  int to_child[2], from_child[2];
  ASSERT_EQ(pipe(to_child), 0);
  ASSERT_EQ(pipe(from_child), 0);
  pid_t child = fork();
  ASSERT_GE(child, 0);
  if (child == 0) {
    close(to_child[1]);
    close(from_child[0]);
    fakeResetModel();
    CUmemGenericAllocationHandle imported_member = 0, imported_group = 0;
    for (;;) {
      int command = 0;
      if (read(to_child[0], &command, sizeof(command)) != static_cast<ssize_t>(sizeof(command))) _exit(2);
      int result = 0;
      switch (command) {
        case kJoin: {
          if (cuMemImportFromShareableHandle(&imported_member, reinterpret_cast<void*>(static_cast<intptr_t>(member_ticket)),
                                             CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) != CUDA_SUCCESS)
            result |= 1;
          if (cuMemMap(0x30000000, kMiB, 0, imported_member, 0) != CUDA_SUCCESS) result |= 2;
          CUmemAccessDesc peer{};
          peer.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
          peer.location.id = 1;
          peer.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
          if (cuMemSetAccess(0x30000000, kMiB, &peer, 1) != CUDA_SUCCESS) result |= 4;
          if (cuMemImportFromShareableHandle(&imported_group, reinterpret_cast<void*>(static_cast<intptr_t>(group_ticket)),
                                             CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) != CUDA_SUCCESS)
            result |= 8;
          if (!is_logical(imported_group)) result |= 16;
          if (cuMulticastAddDevice(imported_group, 1) != CUDA_SUCCESS) result |= 32;
          if (cuMulticastBindAddr_v2(imported_group, 1, 0, 0x30000000, kMiB, 0) != CUDA_SUCCESS) result |= 64;
          if (cuMemMap(0x80000000, kMiB, 0, imported_group, 0) != CUDA_SUCCESS) result |= 128;
          if (cuMemSetAccess(0x80000000, kMiB, &peer, 1) != CUDA_SUCCESS) result |= 256;
          if (stats().multicasts != 1 || fakeMulticastObjects() != 1 || fakeMulticastDevices() != 1 ||
              fakeMulticastBindings(2) != 1)
            result |= 512;
          break;
        }
        case kCheckRestored: {
          struct cuinterpose_debug_stats s = stats();
          if (s.phase != CUINTERPOSE_PHASE_ACTIVE) result |= 1;
          if (s.multicasts != 1 || s.allocations != 1) result |= 2;
          if (fakeMulticastObjects() != 1 || fakeMulticastDevices() != 1 || fakeMulticastBindings(2) != 1) result |= 4;
          if (fakeMappedCount() != 2) result |= 8;  // the member and the object are mapped again
          CUmemAllocationProp got{};
          if (cuMemGetAllocationPropertiesFromHandle(&got, imported_group) != CUDA_SUCCESS) result |= 16;
          break;
        }
        case kRelease:
          if (cuMemUnmap(0x80000000, kMiB) != CUDA_SUCCESS) result |= 1;
          if (cuMulticastUnbind(imported_group, 1, 0, kMiB) != CUDA_SUCCESS) result |= 2;
          if (cuMemRelease(imported_group) != CUDA_SUCCESS) result |= 4;
          if (stats().multicasts != 0 || fakeMulticastObjects() != 0) result |= 8;
          if (cuMemUnmap(0x30000000, kMiB) != CUDA_SUCCESS) result |= 16;
          if (cuMemRelease(imported_member) != CUDA_SUCCESS) result |= 32;
          if (stats().allocations != 0) result |= 64;
          break;
        case kQuit:
          _exit(0);
      }
      if (write(from_child[1], &result, sizeof(result)) != static_cast<ssize_t>(sizeof(result))) _exit(3);
    }
  }
  close(to_child[0]);
  close(from_child[1]);
  auto tell = [&](int command) {
    int result = -1;
    EXPECT_EQ(write(to_child[1], &command, sizeof(command)), static_cast<ssize_t>(sizeof(command)));
    EXPECT_EQ(read(from_child[0], &result, sizeof(result)), static_cast<ssize_t>(sizeof(result)));
    return result;
  };
  EXPECT_EQ(tell(kJoin), 0) << "child could not join the group";

  Outcome prepare = coordinate("--prepare", checkpoint, {getpid(), child});
  EXPECT_EQ(prepare.status, 0) << prepare.err << prepare.out;
  EXPECT_NE(prepare.out.find("phase=prepare_multicast status=ok"), std::string::npos) << prepare.out;
  EXPECT_EQ(fakeMulticastObjects(), 0) << "the object was released for the native checkpoint";
  EXPECT_EQ(fakeMulticastBindings(0), 0);
  EXPECT_EQ(fakeMappedCount(), 0);
  EXPECT_EQ(stats().multicasts, 1u) << "the record stays for restore";
  EXPECT_EQ(stats().cached_exports, 0u);
  EXPECT_EQ(stats().phase, static_cast<uint32_t>(CUINTERPOSE_PHASE_PREPARED));

  Outcome restore = coordinate("--restore", checkpoint, {getpid(), child});
  EXPECT_EQ(restore.status, 0) << restore.err << restore.out;
  EXPECT_NE(restore.out.find("phase=restore_multicast status=ok"), std::string::npos) << restore.out;
  EXPECT_EQ(stats().phase, static_cast<uint32_t>(CUINTERPOSE_PHASE_ACTIVE));
  EXPECT_EQ(fakeMulticastObjects(), 1);
  EXPECT_EQ(fakeMulticastDevices(), 1);
  EXPECT_EQ(fakeMulticastBindings(1), 1) << "the BindMem binding is back";
  EXPECT_EQ(fakeMappedCount(), 2) << "the member and the object are mapped again";
  EXPECT_EQ(stats().cached_exports, 2u) << "both descriptors were exported again";
  EXPECT_EQ(fakeExportCalls(), 4);
  EXPECT_EQ(tell(kCheckRestored), 0) << "child did not see its group restored";

  // Sharing goes on: the old ticket imports again (aliasing the driver handle here).
  CUmemGenericAllocationHandle again = 0;
  EXPECT_EQ(cuMemImportFromShareableHandle(&again, reinterpret_cast<void*>(static_cast<intptr_t>(group_ticket)),
                                           CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR),
            CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(again), CUDA_SUCCESS);

  EXPECT_EQ(tell(kRelease), 0);
  int quit = kQuit;
  EXPECT_EQ(write(to_child[1], &quit, sizeof(quit)), static_cast<ssize_t>(sizeof(quit)));
  int status = 0;
  waitpid(child, &status, 0);
  EXPECT_TRUE(WIFEXITED(status) && WEXITSTATUS(status) == 0) << "child status " << status;

  close(member_ticket);
  close(group_ticket);
  EXPECT_EQ(cuMemUnmap(0x70000000, kMiB), CUDA_SUCCESS);
  EXPECT_EQ(cuMulticastUnbind(group, 0, 0, kMiB), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(group), CUDA_SUCCESS);
  EXPECT_EQ(stats().multicasts, 0u);
  EXPECT_EQ(fakeMulticastObjects(), 0);
  EXPECT_EQ(cuMemUnmap(0x10000000, kMiB), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(member), CUDA_SUCCESS);
  EXPECT_EQ(stats().allocations, 0u);
  EXPECT_EQ(fakeLiveAllocations(), 0);
  EXPECT_EQ(stats().cached_exports, 0u);
#endif
}

// r615 gives a multicast object more capacity than cuMulticastCreate asked
// for, and NCCL binds and maps into that extra room. The shim reports the
// extent actually used, so the coordinator's bounds checks accept it and the
// same binding comes back after restore.
TEST_F(Multicast, EffectiveExtentFollowsWhatTheDriverAccepted) {
  CUmemGenericAllocationHandle member = mapped_allocation(0, 0x10000000, 2 * kMiB);
  ASSERT_NE(member, 0u);
  CUmulticastObjectProp props = multicast_props(1, kMiB);  // the fake rounds capacity up to 4 MiB
  CUmemGenericAllocationHandle group = 0;
  ASSERT_EQ(cuMulticastCreate(&group, &props), CUDA_SUCCESS);
  ASSERT_EQ(cuMulticastAddDevice(group, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMulticastBindMem(group, kMiB, member, 0, kMiB, 0), CUDA_SUCCESS) << "binding beyond the requested size";
  ASSERT_EQ(cuMemMap(0x70000000, 2 * kMiB, 0, group, 0), CUDA_SUCCESS) << "mapping beyond the requested size";

  Outcome prepare = coordinate("--prepare", checkpoint, {getpid()});
  EXPECT_EQ(prepare.status, 0) << prepare.err << prepare.out;
  Outcome restore = coordinate("--restore", checkpoint, {getpid()});
  EXPECT_EQ(restore.status, 0) << restore.err << restore.out;
  EXPECT_EQ(fakeMulticastBindings(1), 1);
  EXPECT_EQ(fakeMappedCount(), 2);

  EXPECT_EQ(cuMemUnmap(0x70000000, 2 * kMiB), CUDA_SUCCESS);
  EXPECT_EQ(cuMulticastUnbind(group, 0, kMiB, kMiB), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(group), CUDA_SUCCESS);
  EXPECT_EQ(cuMemUnmap(0x10000000, 2 * kMiB), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(member), CUDA_SUCCESS);
  EXPECT_EQ(stats().multicasts, 0u);
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

// PyTorch's symmetric memory unbinds the slice cuMulticastBindMem bound and
// binds it again by address. The record follows, and restore replays the
// address binding.
TEST_F(Multicast, UnbindThenBindByAddressIsRestored) {
  CUmemGenericAllocationHandle member = mapped_allocation(0, 0x10000000, kMiB);
  ASSERT_NE(member, 0u);
  CUmulticastObjectProp props = multicast_props(1, kMiB);
  CUmemGenericAllocationHandle group = 0;
  ASSERT_EQ(cuMulticastCreate(&group, &props), CUDA_SUCCESS);
  ASSERT_EQ(cuMulticastAddDevice(group, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMulticastBindMem(group, 0, member, 0, kMiB, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMulticastUnbind(group, 0, 0, kMiB), CUDA_SUCCESS);
  EXPECT_EQ(fakeMulticastBindings(0), 0);
  ASSERT_EQ(cuMulticastBindAddr(group, 0, 0x10000000, kMiB, 0), CUDA_SUCCESS);
  EXPECT_EQ(fakeMulticastBindings(2), 1);
  ASSERT_EQ(cuMemMap(0x70000000, kMiB, 0, group, 0), CUDA_SUCCESS);

  Outcome prepare = coordinate("--prepare", checkpoint, {getpid()});
  EXPECT_EQ(prepare.status, 0) << prepare.err << prepare.out;
  EXPECT_EQ(fakeMulticastBindings(0), 0);
  Outcome restore = coordinate("--restore", checkpoint, {getpid()});
  EXPECT_EQ(restore.status, 0) << restore.err << restore.out;
  EXPECT_EQ(fakeMulticastBindings(2), 1) << "the address binding came back";
  EXPECT_EQ(fakeMulticastBindings(1), 0) << "the unbound BindMem binding did not";

  EXPECT_EQ(cuMemUnmap(0x70000000, kMiB), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(group), CUDA_SUCCESS);
  EXPECT_EQ(fakeMulticastObjects(), 0) << "releasing the last handle of an unmapped object destroys it";
  EXPECT_EQ(cuMemUnmap(0x10000000, kMiB), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(member), CUDA_SUCCESS);
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

// Handle types other than the POSIX descriptor are not tracked, for multicast
// objects as for allocations: the application gets the driver's handle and
// every later call passes straight through.
TEST_F(Multicast, NonPosixObjectsPassThrough) {
  CUmulticastObjectProp props = multicast_props(1, kMiB);
  props.handleTypes = CU_MEM_HANDLE_TYPE_FABRIC;
  CUmemGenericAllocationHandle raw = 0;
  ASSERT_EQ(cuMulticastCreate(&raw, &props), CUDA_SUCCESS);
  EXPECT_FALSE(is_logical(raw));
  EXPECT_EQ(stats().multicasts, 0u);
  EXPECT_EQ(cuMulticastAddDevice(raw, 0), CUDA_SUCCESS);
  EXPECT_EQ(fakeMulticastDevices(), 1);
  EXPECT_EQ(cuMemRelease(raw), CUDA_SUCCESS);
  EXPECT_EQ(fakeMulticastObjects(), 0);
}

// Memory the shim does not track cannot be bound to a tracked object: it could
// not be bound again after restore, so the bind is refused up front.
TEST_F(Multicast, BindingUntrackedMemoryIsRefused) {
  CUmemAllocationProp untracked = posix_props(0);
  untracked.requestedHandleTypes = CU_MEM_HANDLE_TYPE_NONE;
  CUmemGenericAllocationHandle raw = 0;
  ASSERT_EQ(cuMemCreate(&raw, kMiB, &untracked, 0), CUDA_SUCCESS);
  EXPECT_FALSE(is_logical(raw));
  CUmulticastObjectProp props = multicast_props(1, kMiB);
  CUmemGenericAllocationHandle group = 0;
  ASSERT_EQ(cuMulticastCreate(&group, &props), CUDA_SUCCESS);
  ASSERT_EQ(cuMulticastAddDevice(group, 0), CUDA_SUCCESS);
  EXPECT_EQ(cuMulticastBindMem(group, 0, raw, 0, kMiB, 0), CUDA_ERROR_NOT_SUPPORTED);
  EXPECT_EQ(fakeMulticastBindings(0), 0) << "the driver never saw the bind";
  EXPECT_EQ(cuMemRelease(group), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(raw), CUDA_SUCCESS);
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

// One control request to this process's own socket, as the coordinator would send it.
struct cuinterpose_header request_own_shim(uint16_t operation, const char* participant) {
  struct cuinterpose_header request{};
  struct cuinterpose_header reply{};
  reply.status = -1;
  request.magic = CUINTERPOSE_MAGIC;
  request.version = CUINTERPOSE_VERSION;
  request.operation = operation;
  if (participant != nullptr) snprintf(request.participant_id, sizeof(request.participant_id), "%s", participant);
  std::string path = std::string(getenv("SNAPSHOT_CONTROL_DIR")) + "/" + CUINTERPOSE_SOCKET_PREFIX + std::to_string(getpid()) + ".sock";
  int fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (fd < 0) return reply;
  struct sockaddr_un address{};
  address.sun_family = AF_UNIX;
  snprintf(address.sun_path, sizeof(address.sun_path), "%s", path.c_str());
  if (connect(fd, reinterpret_cast<struct sockaddr*>(&address), sizeof(address)) == 0 &&
      send(fd, &request, sizeof(request), MSG_NOSIGNAL) == static_cast<ssize_t>(sizeof(request))) {
    size_t got = 0;
    while (got < sizeof(reply)) {
      ssize_t n = recv(fd, reinterpret_cast<char*>(&reply) + got, sizeof(reply) - got, 0);
      if (n <= 0) break;
      got += static_cast<size_t>(n);
    }
  }
  close(fd);
  return reply;
}

// The cached export descriptor is an independent reference to the object in
// the driver. PREPARE_MULTICAST must close it before releasing the object, or
// the object would outlive the teardown. This test drives that one phase by
// hand and leaves the shim mid-checkpoint, so it runs last in this binary
// (gtest runs tests in definition order).
TEST_F(Multicast, PrepareMulticastClosesTheCachedDescriptorBeforeReleasingTheObject) {
  CUmemGenericAllocationHandle member = mapped_allocation(0, 0x10000000, kMiB);
  ASSERT_NE(member, 0u);
  CUmulticastObjectProp props = multicast_props(1, kMiB);
  CUmemGenericAllocationHandle group = 0;
  ASSERT_EQ(cuMulticastCreate(&group, &props), CUDA_SUCCESS);
  ASSERT_EQ(cuMulticastAddDevice(group, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMulticastBindMem(group, 0, member, 0, kMiB, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x70000000, kMiB, 0, group, 0), CUDA_SUCCESS);
  int member_ticket = -1, group_ticket = -1;
  ASSERT_EQ(cuMemExportToShareableHandle(&member_ticket, member, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemExportToShareableHandle(&group_ticket, group, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);
  ASSERT_EQ(stats().cached_exports, 2u);

  struct cuinterpose_header identity = request_own_shim(CUINTERPOSE_IDENTIFY, nullptr);
  ASSERT_EQ(identity.status, 0) << identity.message;
  struct cuinterpose_header reply = request_own_shim(CUINTERPOSE_PREPARE_MULTICAST, identity.participant_id);
  EXPECT_EQ(reply.status, 0) << reply.message;
  EXPECT_EQ(stats().cached_exports, 1u) << "only the unicast descriptor is still cached; PREPARE closes that one";
  EXPECT_EQ(fakeMulticastObjects(), 0) << "the object itself was released";
  EXPECT_EQ(fakeMulticastBindings(0), 0);
  EXPECT_EQ(fakeMappedCount(), 1) << "the member's own mapping is untouched until PREPARE";
  EXPECT_EQ(stats().multicasts, 1u) << "the record waits for restore";
  close(member_ticket);
  close(group_ticket);
}

}  // namespace
