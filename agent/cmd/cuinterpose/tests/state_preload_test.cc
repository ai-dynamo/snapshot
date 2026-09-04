// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Allocation tracking, tickets, and the export cache, exercised through the
// public CUDA entry points with the shim LD_PRELOADed over the fake driver in
// tracked mode. SNAPSHOT_CONTROL_DIR must point at a writable directory.

#include <cuda.h>
#include <dirent.h>
#include <dlfcn.h>
#include <fcntl.h>
#include <gtest/gtest.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#include <atomic>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

#include "../export.h"
#include "../protocol.h"
#include "fake_cuda.h"

namespace {

const uint64_t kLogicalTag = 0xd94d000000000000ULL;
const uint64_t kLogicalMask = 0xffff000000000000ULL;

bool is_logical(CUmemGenericAllocationHandle handle) { return (handle & kLogicalMask) == kLogicalTag; }

struct cuinterpose_debug_stats stats() {
  using fn = void (*)(struct cuinterpose_debug_stats*);
  static fn get = reinterpret_cast<fn>(dlsym(RTLD_DEFAULT, "cuinterpose_debug_stats"));
  struct cuinterpose_debug_stats s{};
  if (get != nullptr) get(&s);
  return s;
}

int open_fds() {
  int count = 0;
  DIR* dir = opendir("/proc/self/fd");
  while (readdir(dir) != nullptr) count++;
  closedir(dir);
  return count - 3;
}

CUmemAllocationProp posix_props() {
  CUmemAllocationProp prop{};
  prop.type = CU_MEM_ALLOCATION_TYPE_PINNED;
  prop.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  prop.location.id = 0;
  prop.requestedHandleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  return prop;
}

class Tracking : public ::testing::Test {
 protected:
  static void SetUpTestSuite() { fakeEnableTrackedBehavior(); }
  void SetUp() override {
    fakeResetModel();
    fakeReset();
    struct cuinterpose_debug_stats s = stats();
    ASSERT_EQ(s.allocations, 0u) << "a previous test leaked tracking state";
    ASSERT_EQ(s.handles, 0u);
    ASSERT_EQ(s.mappings, 0u);
    ASSERT_EQ(s.cached_exports, 0u);
    fds_before = open_fds();
  }
  void TearDown() override {
    struct cuinterpose_debug_stats s = stats();
    EXPECT_EQ(s.allocations, 0u) << "test left tracked allocations behind";
    EXPECT_EQ(s.handles, 0u);
    EXPECT_EQ(s.mappings, 0u);
    EXPECT_EQ(s.cached_exports, 0u);
    EXPECT_EQ(open_fds(), fds_before) << "descriptor leak";
  }
  CUmemGenericAllocationHandle create(size_t size = 1 << 20) {
    CUmemAllocationProp prop = posix_props();
    CUmemGenericAllocationHandle handle = 0;
    EXPECT_EQ(cuMemCreate(&handle, size, &prop, 0), CUDA_SUCCESS);
    EXPECT_TRUE(is_logical(handle));
    return handle;
  }
  int fds_before = 0;
};

TEST_F(Tracking, CreateMapUnmapReleaseLeavesNothingBehind) {
  CUmemGenericAllocationHandle handle = create();
  EXPECT_EQ(stats().allocations, 1u);
  EXPECT_EQ(stats().handles, 1u);
  EXPECT_EQ(cuMemMap(0x10000, 1 << 20, 0, handle, 0), CUDA_SUCCESS);
  EXPECT_EQ(stats().mappings, 1u);
  EXPECT_EQ(fakeMappedCount(), 1);
  EXPECT_EQ(fakeLastCall()->handle, 0x1000u) << "the driver saw its own handle, not the logical one";
  // Releasing the handle while mapped keeps the record (the mapping is a reference).
  EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
  EXPECT_EQ(stats().handles, 0u);
  EXPECT_EQ(stats().allocations, 1u);
  EXPECT_EQ(fakeLiveAllocations(), 1) << "the mapping keeps the driver allocation alive";
  EXPECT_EQ(cuMemUnmap(0x10000, 1 << 20), CUDA_SUCCESS);
  EXPECT_EQ(stats().allocations, 0u);
  EXPECT_EQ(fakeLiveAllocations(), 0);
  // A released logical handle is refused, not passed to the driver.
  EXPECT_EQ(cuMemRelease(handle), CUDA_ERROR_INVALID_HANDLE);
}

TEST_F(Tracking, NonPosixHandleTypesPassThroughUntracked) {
  for (unsigned type : {0u, static_cast<unsigned>(CU_MEM_HANDLE_TYPE_FABRIC),
                        static_cast<unsigned>(CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR | CU_MEM_HANDLE_TYPE_FABRIC)}) {
    CUmemAllocationProp prop = posix_props();
    prop.requestedHandleTypes = static_cast<CUmemAllocationHandleType>(type);
    CUmemGenericAllocationHandle handle = 0;
    ASSERT_EQ(cuMemCreate(&handle, 4096, &prop, 0), CUDA_SUCCESS) << type;
    EXPECT_FALSE(is_logical(handle)) << "untracked allocations keep the driver's handle";
    EXPECT_EQ(stats().allocations, 0u);
    EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
  }
  EXPECT_EQ(stats().passthrough_creations, 2u) << "handle type 0 is ordinary and not counted";
}

TEST_F(Tracking, RetainAliasesOneDriverHandle) {
  CUmemGenericAllocationHandle handle = create();
  ASSERT_EQ(cuMemMap(0x10000, 1 << 20, 0, handle, 0), CUDA_SUCCESS);
  CUmemGenericAllocationHandle retained = 0;
  ASSERT_EQ(cuMemRetainAllocationHandle(&retained, reinterpret_cast<void*>(0x10800)), CUDA_SUCCESS);
  EXPECT_TRUE(is_logical(retained));
  EXPECT_NE(retained, handle);
  EXPECT_EQ(stats().handles, 2u);
  // Driver-side: create (1) + map (1) + retain (1) - alias release (1) = 2.
  EXPECT_EQ(fakeAllocationRefs(0x1000), 2) << "the redundant driver reference was released immediately";
  EXPECT_EQ(cuMemRelease(retained), CUDA_SUCCESS);
  EXPECT_EQ(fakeAllocationRefs(0x1000), 2) << "releasing an alias keeps the shared backing reference";
  EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
  EXPECT_EQ(fakeAllocationRefs(0x1000), -1) << "the last logical handle drops the driver handle";
  EXPECT_EQ(fakeLiveAllocations(), 1) << "the mapping alone keeps the memory alive";
  EXPECT_EQ(cuMemUnmap(0x10000, 1 << 20), CUDA_SUCCESS);
  EXPECT_EQ(fakeLiveAllocations(), 0);
  // Retain on an untracked address is a plain pass-through.
  CUmemGenericAllocationHandle untracked = 0;
  EXPECT_EQ(cuMemRetainAllocationHandle(&untracked, reinterpret_cast<void*>(0x999000)), CUDA_ERROR_INVALID_VALUE);
}

TEST_F(Tracking, MapRefusesOverlapAndUnmapIsRangeBased) {
  CUmemGenericAllocationHandle a = create(1 << 20);
  CUmemGenericAllocationHandle b = create(1 << 20);
  ASSERT_EQ(cuMemMap(0x100000, 0x100000, 0, a, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x200000, 0x100000, 0, b, 0), CUDA_SUCCESS);
  EXPECT_EQ(cuMemMap(0x180000, 0x100000, 0, a, 0), CUDA_ERROR_INVALID_VALUE) << "overlapping a tracked mapping";
  EXPECT_EQ(stats().mappings, 2u);
  EXPECT_EQ(fakeMappedCount(), 2) << "the refused map never reached the driver";
  EXPECT_EQ(cuMemUnmap(0x100000, 0x180000), CUDA_ERROR_INVALID_VALUE) << "cutting through a mapping";
  EXPECT_EQ(cuMemUnmap(0x100000, 0x200000), CUDA_SUCCESS) << "one call covering two whole mappings";
  EXPECT_EQ(stats().mappings, 0u);
  EXPECT_EQ(fakeMappedCount(), 0);
  // An unmap that touches nothing tracked passes straight through.
  EXPECT_EQ(cuMemUnmap(0x900000, 0x1000), CUDA_ERROR_INVALID_VALUE) << "the fake driver rejects unknown ranges";
  EXPECT_EQ(cuMemRelease(a), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(b), CUDA_SUCCESS);
}

TEST_F(Tracking, AccessIsMergedPerLocation) {
  CUmemGenericAllocationHandle handle = create(1 << 21);
  // PyTorch maps segments individually and sets access over the whole span.
  ASSERT_EQ(cuMemMap(0x100000, 1 << 20, 0, handle, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemMap(0x200000, 1 << 20, 1 << 20, handle, 0), CUDA_SUCCESS);
  CUmemAccessDesc gpu0{};
  gpu0.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
  gpu0.location.id = 0;
  gpu0.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
  CUmemAccessDesc gpu1 = gpu0;
  gpu1.location.id = 1;
  EXPECT_EQ(cuMemSetAccess(0x100000, 1 << 21, &gpu0, 1), CUDA_SUCCESS);
  EXPECT_EQ(cuMemSetAccess(0x100000, 1 << 21, &gpu1, 1), CUDA_SUCCESS);
  EXPECT_EQ(fakeAccessCalls(), 2);
  // Partial overlap is refused before the driver sees it.
  EXPECT_EQ(cuMemSetAccess(0x180000, 0x100000, &gpu0, 1), CUDA_ERROR_INVALID_VALUE);
  EXPECT_EQ(fakeAccessCalls(), 2);
  // Too many distinct locations for one record: refused, driver untouched.
  std::vector<CUmemAccessDesc> many(CUINTERPOSE_MAX_ACCESS + 1, gpu0);
  for (size_t i = 0; i < many.size(); i++) many[i].location.id = static_cast<int>(i + 10);
  EXPECT_EQ(cuMemSetAccess(0x100000, 1 << 20, many.data(), many.size()), CUDA_ERROR_NOT_SUPPORTED);
  EXPECT_EQ(fakeAccessCalls(), 2);
  // Exactly the capacity, replacing gpu0/gpu1 through NONE first, is fine.
  CUmemAccessDesc none0 = gpu0;
  none0.flags = CU_MEM_ACCESS_FLAGS_PROT_NONE;
  CUmemAccessDesc none1 = gpu1;
  none1.flags = CU_MEM_ACCESS_FLAGS_PROT_NONE;
  CUmemAccessDesc revoke[2] = {none0, none1};
  EXPECT_EQ(cuMemSetAccess(0x100000, 1 << 21, revoke, 2), CUDA_SUCCESS);
  many.pop_back();
  EXPECT_EQ(cuMemSetAccess(0x100000, 1 << 21, many.data(), many.size()), CUDA_SUCCESS);
  // Untracked ranges pass through.
  EXPECT_EQ(cuMemSetAccess(0x900000, 4096, &gpu0, 1), CUDA_SUCCESS);
  EXPECT_EQ(fakeAccessCalls(), 5);
  EXPECT_EQ(cuMemUnmap(0x100000, 1 << 21), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
}

TEST_F(Tracking, ExportMintsTicketsFromOneDriverExport) {
  CUmemGenericAllocationHandle handle = create();
  int ticket_a = -1, ticket_b = -1;
  EXPECT_EQ(cuMemExportToShareableHandle(nullptr, handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0),
            CUDA_ERROR_INVALID_VALUE);
  EXPECT_EQ(cuMemExportToShareableHandle(&ticket_a, handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 1),
            CUDA_ERROR_INVALID_VALUE) << "non-zero flags, as the driver would";
  EXPECT_EQ(cuMemExportToShareableHandle(&ticket_a, handle, CU_MEM_HANDLE_TYPE_FABRIC, 0), CUDA_ERROR_INVALID_VALUE);
  ASSERT_EQ(cuMemExportToShareableHandle(&ticket_a, handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);
  ASSERT_EQ(cuMemExportToShareableHandle(&ticket_b, handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);
  EXPECT_EQ(fakeExportCalls(), 1) << "the driver is asked once; later exports reuse the cached descriptor";
  EXPECT_EQ(stats().cached_exports, 1u);
  EXPECT_NE(ticket_a, ticket_b);
  // A ticket is a sealed memfd, not a driver descriptor.
  EXPECT_EQ(fcntl(ticket_a, F_GET_SEALS) & F_SEAL_WRITE, F_SEAL_WRITE);
  close(ticket_a);
  close(ticket_b);
  EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
  EXPECT_EQ(stats().cached_exports, 0u) << "the cached descriptor goes with the last local reference";
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

TEST_F(Tracking, ImportOfATicketInTheSameProcessAliasesTheAllocation) {
  CUmemGenericAllocationHandle handle = create();
  int ticket = -1;
  ASSERT_EQ(cuMemExportToShareableHandle(&ticket, handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);
  CUmemGenericAllocationHandle imported = 0;
  ASSERT_EQ(cuMemImportFromShareableHandle(&imported, reinterpret_cast<void*>(static_cast<intptr_t>(ticket)),
                                           CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR),
            CUDA_SUCCESS);
  close(ticket);
  EXPECT_TRUE(is_logical(imported));
  EXPECT_NE(imported, handle);
  EXPECT_EQ(stats().allocations, 1u) << "same allocation, second logical handle";
  EXPECT_EQ(stats().handles, 2u);
  EXPECT_EQ(fakeAllocationRefs(0x1000), 1) << "the import's driver reference was collapsed into the creator's";
  CUmemAllocationProp props{};
  EXPECT_EQ(cuMemGetAllocationPropertiesFromHandle(&props, imported), CUDA_SUCCESS);
  EXPECT_EQ(props.requestedHandleTypes, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR);
  EXPECT_EQ(cuMemRelease(imported), CUDA_SUCCESS);
  EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
  EXPECT_EQ(fakeLiveAllocations(), 0);
}

TEST_F(Tracking, RawImportsAreCountedUntilReleased) {
  int foreign = memfd_create("foreign", MFD_CLOEXEC);
  ASSERT_EQ(write(foreign, "x", 1), 1);
  CUmemGenericAllocationHandle imported = 0;
  ASSERT_EQ(cuMemImportFromShareableHandle(&imported, reinterpret_cast<void*>(static_cast<intptr_t>(foreign)),
                                           CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR),
            CUDA_SUCCESS);
  close(foreign);
  EXPECT_FALSE(is_logical(imported)) << "a raw import stays a driver handle";
  EXPECT_EQ(stats().live_raw_imports, 1u);
  EXPECT_EQ(stats().allocations, 0u);
  EXPECT_EQ(cuMemRelease(imported), CUDA_SUCCESS);
  EXPECT_EQ(stats().live_raw_imports, 0u);
}

// Two threads export the same allocation for the first time at once: the
// driver must see exactly one export and both must get valid tickets.
TEST_F(Tracking, ConcurrentFirstExportsCallTheDriverOnce) {
  CUmemGenericAllocationHandle handle = create();
  std::vector<std::thread> threads;
  std::vector<int> tickets(8, -1);
  std::atomic<int> failures{0};
  for (int i = 0; i < 8; i++) {
    threads.emplace_back([&, i] {
      if (cuMemExportToShareableHandle(&tickets[i], handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0) != CUDA_SUCCESS)
        failures++;
    });
  }
  for (auto& t : threads) t.join();
  EXPECT_EQ(failures.load(), 0);
  EXPECT_EQ(fakeExportCalls(), 1);
  for (int fd : tickets) {
    EXPECT_GE(fd, 0);
    close(fd);
  }
  EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
}

// Peers keep importing through the listener while the creator releases and
// recreates allocations. The sanitizers flag any use of a freed entry or a
// closed descriptor; the assertions check that nobody ever got a wrong buffer.
TEST_F(Tracking, ListenerServesWhileAllocationsChurn) {
  std::atomic<bool> stop{false};
  std::atomic<int> imports{0};
  std::atomic<int> refusals{0};
  CUmemGenericAllocationHandle handle = create();
  int ticket = -1;
  ASSERT_EQ(cuMemExportToShareableHandle(&ticket, handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);

  std::thread importer([&] {
    while (!stop) {
      CUmemGenericAllocationHandle imported = 0;
      CUresult r = cuMemImportFromShareableHandle(&imported, reinterpret_cast<void*>(static_cast<intptr_t>(ticket)),
                                                  CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR);
      if (r == CUDA_SUCCESS) {
        imports++;
        cuMemRelease(imported);
      } else {
        refusals++;
      }
    }
  });
  usleep(20000);
  // Drop the creator's last reference while imports are in flight: the ticket
  // becomes dead, imports start failing cleanly, nothing crashes.
  EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
  usleep(20000);
  stop = true;
  importer.join();
  close(ticket);
  EXPECT_GT(imports.load(), 0);
  EXPECT_GT(refusals.load(), 0) << "imports after the creator released must be refused";
}

// A child created by fork() gets its own identity and socket on first CUDA
// activity, imports the parent's ticket through the parent's listener, and
// leaves the parent's tracking untouched.
TEST_F(Tracking, ForkedChildImportsThroughTheParentsListener) {
  CUmemGenericAllocationHandle handle = create();
  int ticket = -1;
  ASSERT_EQ(cuMemExportToShareableHandle(&ticket, handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0), CUDA_SUCCESS);
  int pipefd[2];
  ASSERT_EQ(pipe(pipefd), 0);
  pid_t child = fork();
  ASSERT_GE(child, 0);
  if (child == 0) {
    close(pipefd[0]);
    int code = 0;
    struct cuinterpose_debug_stats s = stats();
    if (s.allocations != 0 || s.handles != 0) code |= 1;  // inherited records were dropped
    CUmemGenericAllocationHandle imported = 0;
    if (cuMemImportFromShareableHandle(&imported, reinterpret_cast<void*>(static_cast<intptr_t>(ticket)),
                                       CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) != CUDA_SUCCESS)
      code |= 2;
    if (!is_logical(imported)) code |= 4;
    s = stats();
    if (s.allocations != 1 || s.handles != 1) code |= 8;
    if (cuMemRelease(imported) != CUDA_SUCCESS) code |= 16;
    if (write(pipefd[1], &code, sizeof(code)) != static_cast<ssize_t>(sizeof(code))) _exit(99);
    _exit(0);
  }
  close(pipefd[1]);
  int code = -1;
  EXPECT_EQ(read(pipefd[0], &code, sizeof(code)), static_cast<ssize_t>(sizeof(code)));
  close(pipefd[0]);
  int status = 0;
  waitpid(child, &status, 0);
  EXPECT_TRUE(WIFEXITED(status) && WEXITSTATUS(status) == 0);
  EXPECT_EQ(code, 0) << "child failure bits: " << code;
  // The parent is unaffected by the child's activity.
  EXPECT_EQ(stats().allocations, 1u);
  EXPECT_EQ(stats().handles, 1u);
  close(ticket);
  EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
}

TEST_F(Tracking, ManyAllocationsComeAndGoWithoutGrowth) {
  for (int round = 0; round < 10000; round++) {
    CUmemGenericAllocationHandle handle = create(4096);
    ASSERT_EQ(cuMemMap(0x10000000 + static_cast<CUdeviceptr>(round % 64) * 0x10000, 4096, 0, handle, 0), CUDA_SUCCESS);
    ASSERT_EQ(cuMemUnmap(0x10000000 + static_cast<CUdeviceptr>(round % 64) * 0x10000, 4096), CUDA_SUCCESS);
    ASSERT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
  }
  EXPECT_EQ(fakeLiveAllocations(), 0);
  // TearDown asserts the tables are empty.
}

TEST_F(Tracking, DescriptorExhaustionFailsCleanly) {
  CUmemGenericAllocationHandle handle = create();
  rlimit before{}, low{};
  ASSERT_EQ(getrlimit(RLIMIT_NOFILE, &before), 0);
  low = before;
  low.rlim_cur = static_cast<rlim_t>(open_fds() + 3 + 4);  // room for the export and one ticket, not much more
  ASSERT_EQ(setrlimit(RLIMIT_NOFILE, &low), 0);
  std::vector<int> tickets;
  CUresult last = CUDA_SUCCESS;
  for (int i = 0; i < 16 && last == CUDA_SUCCESS; i++) {
    int ticket = -1;
    last = cuMemExportToShareableHandle(&ticket, handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0);
    if (last == CUDA_SUCCESS) tickets.push_back(ticket);
  }
  EXPECT_NE(last, CUDA_SUCCESS) << "running out of descriptors must surface as an error";
  EXPECT_FALSE(tickets.empty()) << "at least the first export succeeded";
  ASSERT_EQ(setrlimit(RLIMIT_NOFILE, &before), 0);
  for (int fd : tickets) close(fd);
  EXPECT_EQ(cuMemRelease(handle), CUDA_SUCCESS);
}

}  // namespace
