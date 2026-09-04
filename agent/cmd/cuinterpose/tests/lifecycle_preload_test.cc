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


// Commands the parent sends the importer child over a pipe.
enum Command : int { kImport = 1, kCheckRestored = 2, kRelease = 3, kQuit = 4 };

class Lifecycle : public ::testing::Test {
 protected:
  static void SetUpTestSuite() { fakeEnableTrackedBehavior(); }
  void SetUp() override {
    ASSERT_NE(getenv("CUINTERPOSE_COORDINATOR"), nullptr) << "set CUINTERPOSE_COORDINATOR";
    fakeResetModel();
    char tmpl[] = "/tmp/cuinterpose-lifecycle-XXXXXX";
    ASSERT_NE(mkdtemp(tmpl), nullptr);
    checkpoint = tmpl;
  }
  void TearDown() override {
    // Not std::system(): a shell child would inherit the sanitized LD_PRELOAD,
    // and the base image's /bin/sh runs a profile script whose helpers then
    // report their own tiny leaks.
    std::error_code ignored;
    std::filesystem::remove_all(checkpoint, ignored);
  }
  std::string checkpoint;
};

// Creator (this process) and importer (child) share one 1 MiB allocation and
// a second, never-exported allocation that must still travel through the host
// carrier. The coordinator prepares both, then restores both; the fake driver
// counts bytes copied out and back in, and the child confirms its imported
// mapping came back.
TEST_F(Lifecycle, PrepareAndRestoreAcrossTwoProcesses) {
  CUmemAllocationProp prop = posix_props();
  CUmemGenericAllocationHandle shared = 0, private_alloc = 0;
  ASSERT_EQ(cuMemCreate(&shared, 1 << 20, &prop, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemCreate(&private_alloc, 1 << 19, &prop, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x10000000, 1 << 20, 0, shared, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x20000000, 1 << 19, 0, private_alloc, 0), CUDA_SUCCESS);
  CUmemAccessDesc access{};
  access.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
  ASSERT_EQ(cuMemSetAccess(0x10000000, 1 << 20, &access, 1), CUDA_SUCCESS);
  int ticket = -1;
  ASSERT_EQ(cuMemExportToShareableHandle(&ticket, shared, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);

  int to_child[2], from_child[2];
  ASSERT_EQ(pipe(to_child), 0);
  ASSERT_EQ(pipe(from_child), 0);
  pid_t child = fork();
  ASSERT_GE(child, 0);
  if (child == 0) {
    close(to_child[1]);
    close(from_child[0]);
    // A real fork child has no usable CUDA state; the fake model starts empty too.
    fakeResetModel();
    CUmemGenericAllocationHandle imported = 0;
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
          if (s.allocations != 1 || s.handles != 1 || s.mappings != 1) result |= 2;
          if (fakeMappedCount() != 1) result |= 4;  // the importer's mapping is back in the driver
          // The logical handle still works after restore.
          CUmemAllocationProp got{};
          if (cuMemGetAllocationPropertiesFromHandle(&got, imported) != CUDA_SUCCESS) result |= 8;
          break;
        }
        case kRelease:
          if (cuMemUnmap(0x30000000, 1 << 20) != CUDA_SUCCESS) result |= 1;
          if (cuMemRelease(imported) != CUDA_SUCCESS) result |= 2;
          if (stats().allocations != 0) result |= 4;
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
  EXPECT_NE(prepare.out.find("carrier_count=2 carrier_bytes=1572864"), std::string::npos) << prepare.out;
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
  EXPECT_EQ(stats().mappings, 2u);
  EXPECT_EQ(fakeMappedCount(), 2) << "both creator mappings are back";
  EXPECT_EQ(fakeExportCalls(), 2) << "the shared allocation was exported again for the export cache";
  EXPECT_EQ(stats().cached_exports, 1u);
  // The arena is unpinned after the reply, off the coordinator's critical path.
  for (int attempt = 0; attempt < 200 && fakeRegisteredHostRanges() != 0; attempt++) usleep(10000);
  EXPECT_EQ(fakeRegisteredHostRanges(), 0) << "the host carrier arena is released after the copy back";
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
  EXPECT_EQ(fakePrimaryContextsHeld(), 0);
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
