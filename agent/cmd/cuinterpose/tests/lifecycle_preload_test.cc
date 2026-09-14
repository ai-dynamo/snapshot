// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// End-to-end checkpoint and restore of shared allocations with the shim
// LD_PRELOADed over the fake driver: this process creates and shares memory, a
// forked child imports it, and the real cuinterpose-coordinator binary drives
// both through prepare and restore exactly as the agent would. Requires
// SNAPSHOT_CONTROL_DIR (writable) and CUINTERPOSE_COORDINATOR (binary path).

#include <cuda.h>
#include <dlfcn.h>
#include <fcntl.h>
#include <gtest/gtest.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#include <cstdio>
#include <cstring>
#include <filesystem>
#include <string>
#include <vector>

#include "../export.h"
#include "../protocol.h"
#include "fake_cuda.h"
#include "coordinator_driver.h"

namespace {

struct cuinterpose_debug_stats stats() {
  using fn = void (*)(struct cuinterpose_debug_stats*);
  static fn get = reinterpret_cast<fn>(dlsym(RTLD_DEFAULT, "cuinterpose_debug_stats"));
  struct cuinterpose_debug_stats s{};
  get(&s);
  return s;
}

CUmemAllocationProp posix_props() {
  CUmemAllocationProp prop{};
  prop.type = CU_MEM_ALLOCATION_TYPE_PINNED;
  prop.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  prop.requestedHandleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  return prop;
}

bool send_fd(int socket, int fd) {
  char byte = 0;
  struct iovec iov {.iov_base = &byte, .iov_len = sizeof(byte)};
  char control[CMSG_SPACE(sizeof(fd))] = {0};
  struct msghdr message {};
  message.msg_iov = &iov;
  message.msg_iovlen = 1;
  message.msg_control = control;
  message.msg_controllen = sizeof(control);
  struct cmsghdr* header = CMSG_FIRSTHDR(&message);
  header->cmsg_level = SOL_SOCKET;
  header->cmsg_type = SCM_RIGHTS;
  header->cmsg_len = CMSG_LEN(sizeof(fd));
  std::memcpy(CMSG_DATA(header), &fd, sizeof(fd));
  return sendmsg(socket, &message, MSG_NOSIGNAL) == 1;
}

int receive_fd(int socket) {
  char byte = 0;
  struct iovec iov {.iov_base = &byte, .iov_len = sizeof(byte)};
  char control[CMSG_SPACE(sizeof(int))] = {0};
  struct msghdr message {};
  message.msg_iov = &iov;
  message.msg_iovlen = 1;
  message.msg_control = control;
  message.msg_controllen = sizeof(control);
  if (recvmsg(socket, &message, 0) != 1)
    return -1;
  const struct cmsghdr* header = CMSG_FIRSTHDR(&message);
  if (header == nullptr || header->cmsg_level != SOL_SOCKET ||
      header->cmsg_type != SCM_RIGHTS || header->cmsg_len != CMSG_LEN(sizeof(int)))
    return -1;
  int fd = -1;
  std::memcpy(&fd, CMSG_DATA(header), sizeof(fd));
  return fd;
}


// Commands the parent sends the importer child over a pipe.
enum Command : int { kImport = 1, kCheckRestored = 2, kRelease = 3, kQuit = 4 };

class Lifecycle : public ::testing::Test {
 protected:
  static void SetUpTestSuite() { fakeEnableTrackedBehavior(); }
  void SetUp() override {
    ASSERT_NE(getenv("CUINTERPOSE_COORDINATOR"), nullptr) << "set CUINTERPOSE_COORDINATOR";
    fakeResetModel();
    ASSERT_EQ(cuCtxSetCurrent(reinterpret_cast<CUcontext>(static_cast<uintptr_t>(1))), CUDA_SUCCESS);
    char tmpl[] = "/tmp/cuinterpose-lifecycle-XXXXXX";
    ASSERT_NE(mkdtemp(tmpl), nullptr);
    checkpoint = tmpl;
  }
  void TearDown() override {
    for (int attempt = 0;
         attempt < 200 && (fakeRegisteredHostRanges() != 0 || fakePrimaryContextsHeld() != 0); attempt++)
      usleep(10000);
    EXPECT_EQ(fakeRegisteredHostRanges(), 0) << "deferred carrier cleanup did not unregister its host arena";
    EXPECT_EQ(fakePrimaryContextsHeld(), 0) << "deferred carrier cleanup did not release its primary context";
    // Not std::system(): a shell child would inherit the sanitized LD_PRELOAD,
    // and the base image's /bin/sh runs a profile script whose helpers then
    // report their own tiny leaks.
    std::error_code ignored;
    std::filesystem::remove_all(checkpoint, ignored);
  }
  std::string checkpoint;
};

// Both processes create and import memory from the other. This all-to-all
// shape covers the restore-time export cycle used by tensor-parallel runtimes:
// every RESTORE_IMPORTERS handler holds its state lock while each peer's
// listener must serve exports without that lock. A second, never-exported
// parent allocation still proves private memory travels through the carrier.
TEST_F(Lifecycle, PrepareAndRestoreAcrossTwoProcesses) {
  CUmemAllocationProp prop = posix_props();
  CUmemGenericAllocationHandle shared = 0, private_alloc = 0;
  ASSERT_EQ(cuMemCreate(&shared, 1 << 20, &prop, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&private_alloc, 1 << 19, &prop, 0), CUDA_SUCCESS);
  const std::string own_socket =
      std::string(getenv("SNAPSHOT_CONTROL_DIR")) + "/" +
      CUINTERPOSE_SOCKET_PREFIX + std::to_string(getpid()) + ".sock";
  struct stat socket_metadata {};
  ASSERT_EQ(stat(own_socket.c_str(), &socket_metadata), 0);
  EXPECT_EQ(socket_metadata.st_mode & 0777, 0666)
      << "PageBroker must connect without CAP_DAC_OVERRIDE; SO_PEERCRED authenticates the peer";
  ASSERT_EQ(cuMemMap(0x10000000, 1 << 20, 0, shared, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x20000000, 1 << 19, 0, private_alloc, 0), CUDA_SUCCESS);
  CUmemAccessDesc access{};
  access.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
  ASSERT_EQ(cuMemSetAccess(0x10000000, 1 << 20, &access, 1), CUDA_SUCCESS);
  int ticket = -1;
  ASSERT_EQ(cuMemExportToShareableHandle(&ticket, shared, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);

  int to_child[2], from_child[2], child_ticket_socket[2];
  ASSERT_EQ(pipe(to_child), 0);
  ASSERT_EQ(pipe(from_child), 0);
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_DGRAM | SOCK_CLOEXEC, 0, child_ticket_socket), 0);
  pid_t child = fork();
  ASSERT_GE(child, 0);
  if (child == 0) {
    close(to_child[1]);
    close(from_child[0]);
    close(child_ticket_socket[0]);
    // A real fork child has no usable CUDA state; the fake model starts empty too.
    fakeResetModel();
    if (cuCtxSetCurrent(reinterpret_cast<CUcontext>(static_cast<uintptr_t>(1))) != CUDA_SUCCESS) _exit(4);
    CUmemGenericAllocationHandle imported = 0;
    CUmemGenericAllocationHandle child_shared = 0;
    int child_ticket = -1;
    if (cuMemCreate(&child_shared, 1 << 18, &prop, 0) != CUDA_SUCCESS ||
        cuMemMap(0x40000000, 1 << 18, 0, child_shared, 0) != CUDA_SUCCESS ||
        cuMemSetAccess(0x40000000, 1 << 18, &access, 1) != CUDA_SUCCESS ||
        cuMemExportToShareableHandle(
            &child_ticket, child_shared,
            CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0) != CUDA_SUCCESS ||
        !send_fd(child_ticket_socket[1], child_ticket))
      _exit(5);
    close(child_ticket);
    close(child_ticket_socket[1]);
    for (;;) {
      int command = 0;
      if (read(to_child[0], &command, sizeof(command)) != static_cast<ssize_t>(sizeof(command))) _exit(2);
      int result = 0;
      switch (command) {
        case kImport:
          if (cuMemImportFromShareableHandle(&imported, reinterpret_cast<void*>(static_cast<intptr_t>(ticket)),
                                             CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) != CUDA_SUCCESS)
            result |= 1;
          if (cuMemMap(0x30000000, 1 << 20, 0, imported, 0) != CUDA_SUCCESS) result |= 2;
          if (cuMemSetAccess(0x30000000, 1 << 20, &access, 1) != CUDA_SUCCESS) result |= 4;
          break;
        case kCheckRestored: {
          struct cuinterpose_debug_stats s = stats();
          if (s.phase != CUINTERPOSE_PHASE_ACTIVE) result |= 1;
          if (s.allocations != 2 || s.handles != 2 || s.mappings != 2) result |= 2;
          if (fakeMappedCount() != 2) result |= 4;  // creator and importer mappings are back
          // The logical handle still works after restore.
          CUmemAllocationProp got{};
          if (cuMemGetAllocationPropertiesFromHandle(&got, imported) != CUDA_SUCCESS) result |= 8;
          break;
        }
        case kRelease:
          if (cuMemUnmap(0x30000000, 1 << 20) != CUDA_SUCCESS) result |= 1;
          if (cuMemRelease(imported) != CUDA_SUCCESS) result |= 2;
          if (cuMemUnmap(0x40000000, 1 << 18) != CUDA_SUCCESS) result |= 4;
          if (cuMemRelease(child_shared) != CUDA_SUCCESS) result |= 8;
          if (stats().allocations != 0) result |= 16;
          break;
        case kQuit:
          _exit(0);
      }
      if (write(from_child[1], &result, sizeof(result)) != static_cast<ssize_t>(sizeof(result))) _exit(3);
    }
  }
  close(to_child[0]);
  close(from_child[1]);
  close(child_ticket_socket[1]);
  int child_ticket = receive_fd(child_ticket_socket[0]);
  close(child_ticket_socket[0]);
  ASSERT_GE(child_ticket, 0) << "child did not publish its shareable ticket";
  CUmemGenericAllocationHandle peer_import = 0;
  ASSERT_EQ(cuMemImportFromShareableHandle(
                &peer_import,
                reinterpret_cast<void*>(static_cast<intptr_t>(child_ticket)),
                CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR),
            CUDA_SUCCESS);
  close(child_ticket);
  ASSERT_EQ(cuMemMap(0x40000000, 1 << 18, 0, peer_import, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemSetAccess(0x40000000, 1 << 18, &access, 1), CUDA_SUCCESS);
  auto tell = [&](int command) {
    int result = -1;
    EXPECT_EQ(write(to_child[1], &command, sizeof(command)), static_cast<ssize_t>(sizeof(command)));
    EXPECT_EQ(read(from_child[0], &result, sizeof(result)), static_cast<ssize_t>(sizeof(result)));
    return result;
  };

  EXPECT_EQ(tell(kImport), 0) << "child import failed";
  EXPECT_EQ(fakeExportCalls(), 1);

  // Checkpoint.
  Outcome prepare = coordinate("--prepare", checkpoint, {getpid(), child});
  EXPECT_EQ(prepare.status, 0) << prepare.err << prepare.out;
  EXPECT_EQ(fakeRegisteredHostRanges(), 1) << "one pinned arena holds every host carrier while checkpointed";
  struct stat st{};
  EXPECT_EQ(stat((checkpoint + "/" + CUINTERPOSE_STATE_FILENAME).c_str(), &st), 0);
  EXPECT_EQ(fakeCopiedToHost(), static_cast<uint64_t>((1 << 20) + (1 << 19)))
      << "both creator allocations, the never-exported one included, were copied to the host";
  EXPECT_NE(prepare.out.find("carrier_count=3 carrier_bytes=1835008"), std::string::npos) << prepare.out;
  EXPECT_EQ(fakeMappedCount(), 0) << "nothing shared stays mapped in the parent for the native checkpoint";
  EXPECT_EQ(stats().phase, static_cast<uint32_t>(CUINTERPOSE_PHASE_PREPARED));
  EXPECT_EQ(stats().cached_exports, 0u) << "the export cache is closed while checkpointed";
  // While prepared, the application's handles answer "not ready".
  EXPECT_EQ(cuMemRelease(shared), CUDA_ERROR_NOT_READY);
  EXPECT_EQ(cuMemMap(0x40000000, 4096, 0, shared, 0), CUDA_ERROR_NOT_READY);

  // Simulate what a native restore leaves behind: the host pages lost their
  // registration in one process, so the shim must pin them again.
  fakeForgetHostRegistrations();
  EXPECT_EQ(fakeRegisteredHostRanges(), 0);

  // Restore.
  Outcome restore = coordinate("--restore", checkpoint, {getpid(), child});
  EXPECT_EQ(restore.status, 0) << restore.err << restore.out;
  EXPECT_EQ(fakeCopiedToDevice(), static_cast<uint64_t>((1 << 20) + (1 << 19)));
  EXPECT_NE(restore.out.find("phase=restore_host_carrier status=ok"), std::string::npos) << restore.out;
  EXPECT_EQ(stats().phase, static_cast<uint32_t>(CUINTERPOSE_PHASE_ACTIVE));
  EXPECT_EQ(stats().mappings, 3u);
  EXPECT_EQ(fakeMappedCount(), 3) << "both creator mappings and the peer import are back";
  EXPECT_EQ(fakeExportCalls(), 2) << "the shared allocation was exported again for the export cache";
  EXPECT_EQ(stats().cached_exports, 1u);
  // The arena is unpinned after the reply, off the coordinator's critical
  // path. TearDown drains and verifies that deferred cleanup.
  EXPECT_EQ(tell(kCheckRestored), 0) << "child did not see its mapping restored";

  // Normal operation resumes: a fresh import of the old ticket still works.
  CUmemGenericAllocationHandle again = 0;
  EXPECT_EQ(cuMemImportFromShareableHandle(&again, reinterpret_cast<void*>(static_cast<intptr_t>(ticket)),
                                           CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR),
            CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(again), CUDA_SUCCESS);

  EXPECT_EQ(tell(kRelease), 0);
  int quit = kQuit;  // no reply: the child exits
  EXPECT_EQ(write(to_child[1], &quit, sizeof(quit)), static_cast<ssize_t>(sizeof(quit)));
  int status = 0;
  waitpid(child, &status, 0);
  EXPECT_TRUE(WIFEXITED(status) && WEXITSTATUS(status) == 0)
      << "child status " << status << (WIFSIGNALED(status) ? " signal " + std::to_string(WTERMSIG(status)) : "");

  close(ticket);
  EXPECT_EQ(cuMemUnmap(0x40000000, 1 << 18), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(peer_import), CUDA_SUCCESS);
  EXPECT_EQ(cuMemUnmap(0x10000000, 1 << 20), CUDA_SUCCESS);
  EXPECT_EQ(cuMemUnmap(0x20000000, 1 << 19), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(shared), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(private_alloc), CUDA_SUCCESS);
  EXPECT_EQ(stats().allocations, 0u);
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

TEST_F(Lifecycle, PrepareIsRefusedWhileARawImportIsAlive) {
  int foreign = memfd_create("foreign", MFD_CLOEXEC);
  ASSERT_EQ(write(foreign, "x", 1), 1);
  CUmemGenericAllocationHandle raw = 0;
  ASSERT_EQ(cuMemImportFromShareableHandle(&raw, reinterpret_cast<void*>(static_cast<intptr_t>(foreign)),
                                           CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR),
            CUDA_SUCCESS);
  close(foreign);
  Outcome prepare = coordinate("--prepare", checkpoint, {getpid()});
  EXPECT_NE(prepare.status, 0);
  EXPECT_NE(prepare.err.find("live raw imports"), std::string::npos) << prepare.err;
  EXPECT_EQ(stats().phase, static_cast<uint32_t>(CUINTERPOSE_PHASE_ACTIVE)) << "nothing was torn down";
  EXPECT_EQ(cuMemRelease(raw), CUDA_SUCCESS);
  // With the raw import gone, prepare succeeds (on an empty topology).
  Outcome again = coordinate("--prepare", checkpoint, {getpid()});
  EXPECT_EQ(again.status, 0) << again.err;
  // Bring the shim back to ACTIVE for the next test.
  Outcome restore = coordinate("--restore", checkpoint, {getpid()});
  EXPECT_EQ(restore.status, 0) << restore.err;
  EXPECT_EQ(stats().phase, static_cast<uint32_t>(CUINTERPOSE_PHASE_ACTIVE));
}

// The driver needs no current context for cuMemCreate, and some workloads
// allocate before they initialize one. Such an allocation adopts the context
// current at its first map or export; one that never gets a context is still
// carried through checkpoint and restore in its device's primary context.
TEST_F(Lifecycle, AllocationsCreatedWithoutAContextAreCarried) {
  CUmemAllocationProp prop = posix_props();
  CUmemGenericAllocationHandle mapped_later = 0, never_mapped = 0;
  ASSERT_EQ(cuCtxSetCurrent(nullptr), CUDA_SUCCESS);  // not interposed: goes to the fake
  ASSERT_EQ(cuMemCreate(&mapped_later, 1 << 20, &prop, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&never_mapped, 1 << 19, &prop, 0), CUDA_SUCCESS);
  EXPECT_EQ(stats().allocations, 2u) << "allocations made without a context are tracked";

  CUcontext application = reinterpret_cast<CUcontext>(static_cast<uintptr_t>(7));
  ASSERT_EQ(cuCtxSetCurrent(application), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x50000000, 1 << 20, 0, mapped_later, 0), CUDA_SUCCESS);

  Outcome prepare = coordinate("--prepare", checkpoint, {getpid()});
  EXPECT_EQ(prepare.status, 0) << prepare.err << prepare.out;
  EXPECT_EQ(fakeCopiedToHost(), static_cast<uint64_t>((1 << 20) + (1 << 19)))
      << "both allocations were copied out, the never-mapped one in its primary context";
  EXPECT_GT(fakePrimaryContextRetainCalls(), 0) << "the primary context was used for the context-less allocation";
  EXPECT_EQ(fakePrimaryContextsHeld(), 0) << "every retained primary context was released";
  EXPECT_EQ(fakeCurrentContext(), application) << "the application's context is current again";

  Outcome restore = coordinate("--restore", checkpoint, {getpid()});
  EXPECT_EQ(restore.status, 0) << restore.err << restore.out;
  EXPECT_EQ(fakeCopiedToDevice(), static_cast<uint64_t>((1 << 20) + (1 << 19)));
  EXPECT_EQ(fakeCurrentContext(), application);
  EXPECT_EQ(stats().phase, static_cast<uint32_t>(CUINTERPOSE_PHASE_ACTIVE));

  // The allocation that never had a context is still usable afterwards.
  EXPECT_EQ(cuMemMap(0x60000000, 1 << 19, 0, never_mapped, 0), CUDA_SUCCESS);
  EXPECT_EQ(fakeMappedCount(), 2);

  EXPECT_EQ(cuMemUnmap(0x50000000, 1 << 20), CUDA_SUCCESS);
  EXPECT_EQ(cuMemUnmap(0x60000000, 1 << 19), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(mapped_later), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(never_mapped), CUDA_SUCCESS);
  EXPECT_EQ(stats().allocations, 0u);
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

// A null recorded context means "use this allocation's device primary
// context", not one context shared by every context-less allocation. In
// particular, allocations on different devices must use separate carrier
// batches.
TEST_F(Lifecycle, NullContextsUseSeparateFallbackDeviceBatches) {
  CUmemAllocationProp device_zero = posix_props();
  CUmemAllocationProp device_one = posix_props();
  CUmemGenericAllocationHandle first = 0, second = 0;
  device_zero.location.id = 0;
  device_one.location.id = 1;

  ASSERT_EQ(cuCtxSetCurrent(nullptr), CUDA_SUCCESS);  // allocations retain a null recorded context
  ASSERT_EQ(cuMemCreate(&first, 1 << 20, &device_zero, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&second, 1 << 19, &device_one, 0), CUDA_SUCCESS);

  Outcome prepare = coordinate("--prepare", checkpoint, {getpid()});
  EXPECT_EQ(prepare.status, 0) << prepare.err << prepare.out;
  EXPECT_EQ(fakeCopiedToHost(), static_cast<uint64_t>((1 << 20) + (1 << 19)));
  EXPECT_EQ(fakeAddressReserveCallsForPrimaryDevice(0), 1)
      << "carrier save must reserve a staging range in device 0's primary context";
  EXPECT_EQ(fakeAddressReserveCallsForPrimaryDevice(1), 1)
      << "carrier save must reserve a staging range in device 1's primary context";
  EXPECT_EQ(fakePrimaryContextsHeld(), 0);

  Outcome restore = coordinate("--restore", checkpoint, {getpid()});
  EXPECT_EQ(restore.status, 0) << restore.err << restore.out;
  EXPECT_EQ(fakeCopiedToDevice(), static_cast<uint64_t>((1 << 20) + (1 << 19)));
  EXPECT_EQ(fakeAddressReserveCallsForPrimaryDevice(0), 2)
      << "carrier restore must reserve a staging range in device 0's primary context";
  EXPECT_EQ(fakeAddressReserveCallsForPrimaryDevice(1), 2)
      << "carrier restore must reserve a staging range in device 1's primary context";
  EXPECT_EQ(cuMemRelease(first), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(second), CUDA_SUCCESS);
  EXPECT_EQ(stats().allocations, 0u);
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

// The address index and allocation ownership index serve different queries.
// Exercise mappings whose address order alternates between allocations so the
// lifecycle cannot accidentally rely on the global range ordering to find an
// allocation's mappings. Releasing one application handle also forces
// PREPARE_MULTICAST to retain its carrier through that allocation's list.
TEST_F(Lifecycle, InterleavedMappingsSurvivePrepareRestoreAndBulkUnmap) {
  constexpr size_t kMiB = 1 << 20;
  CUmemAllocationProp prop = posix_props();
  CUmemGenericAllocationHandle first = 0, second = 0;
  ASSERT_EQ(cuMemCreate(&first, 2 * kMiB, &prop, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&second, 2 * kMiB, &prop, 0), CUDA_SUCCESS);

  ASSERT_EQ(cuMemMap(0x10000000, kMiB, 0, first, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x10100000, kMiB, 0, second, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x10200000, kMiB, kMiB, first, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x10300000, kMiB, kMiB, second, 0), CUDA_SUCCESS);
  CUmemAccessDesc access{};
  access.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
  for (CUdeviceptr address : {0x10000000ULL, 0x10100000ULL, 0x10200000ULL, 0x10300000ULL})
    ASSERT_EQ(cuMemSetAccess(address, kMiB, &access, 1), CUDA_SUCCESS);
  EXPECT_EQ(fakeAccessCalls(), 4);

  ASSERT_EQ(cuMemRelease(first), CUDA_SUCCESS);
  EXPECT_EQ(stats().allocations, 2u);
  EXPECT_EQ(stats().handles, 1u);
  EXPECT_EQ(stats().mappings, 4u);

  Outcome prepare = coordinate("--prepare", checkpoint, {getpid()});
  ASSERT_EQ(prepare.status, 0) << prepare.err << prepare.out;
  EXPECT_EQ(fakeMappedCount(), 0);
  EXPECT_EQ(stats().phase, static_cast<uint32_t>(CUINTERPOSE_PHASE_PREPARED));
  const uint64_t access_calls_after_prepare = fakeAccessCalls();

  Outcome restore = coordinate("--restore", checkpoint, {getpid()});
  ASSERT_EQ(restore.status, 0) << restore.err << restore.out;
  EXPECT_EQ(fakeMappedCount(), 4);
  EXPECT_EQ(fakeAccessCalls(), access_calls_after_prepare + 6)
      << "restore sets access on two carrier staging mappings and replays all four application mappings";
  EXPECT_EQ(stats().mappings, 4u);

  EXPECT_EQ(cuMemUnmap(0x10000000, 4 * kMiB), CUDA_SUCCESS)
      << "one application unmap may span mappings owned by different allocations";
  EXPECT_EQ(stats().mappings, 0u);
  EXPECT_EQ(stats().allocations, 1u) << "the released first allocation settles with its last mapping";
  EXPECT_EQ(cuMemRelease(second), CUDA_SUCCESS);
  EXPECT_EQ(stats().allocations, 0u);
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

TEST_F(Lifecycle, FailedHostCopyLeavesTheWorkloadIntactAndFailsClosed) {
  CUmemAllocationProp prop = posix_props();
  CUmemGenericAllocationHandle handle = 0;
  ASSERT_EQ(cuMemCreate(&handle, 1 << 20, &prop, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x10000000, 1 << 20, 0, handle, 0), CUDA_SUCCESS);
  fakeFailNext("cuMemcpyDtoHAsync_v2");
  Outcome prepare = coordinate("--prepare", checkpoint, {getpid()});
  EXPECT_NE(prepare.status, 0);
  EXPECT_NE(prepare.err.find("host carrier save"), std::string::npos) << prepare.err;
  struct stat st{};
  EXPECT_NE(stat((checkpoint + "/" + CUINTERPOSE_STATE_FILENAME).c_str(), &st), 0) << "no state file on failure";
  EXPECT_EQ(fakeRegisteredHostRanges(), 0) << "partial host carriers were released";
  EXPECT_EQ(fakeMappedCount(), 1) << "the mapping was never touched";
  // The shim is in the failed phase: IDENTIFY reports it, and the application
  // cannot continue with VMM calls. This is the documented fail-stop behavior.
  EXPECT_EQ(stats().phase, static_cast<uint32_t>(CUINTERPOSE_PHASE_FAILED));
  EXPECT_EQ(cuMemUnmap(0x10000000, 1 << 20), CUDA_ERROR_NOT_READY);
  // The process is unusable for the rest of this test binary, so run this
  // test last (gtest runs tests in definition order within a binary).
}

}  // namespace
