// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the shim's containers and the export cache. Linked into
// proto_test; no CUDA, no LD_PRELOAD.

#include <fcntl.h>
#include <gtest/gtest.h>
#include <sys/mman.h>
#include <unistd.h>

#include <atomic>
#include <cstring>
#include <set>
#include <thread>
#include <vector>

extern "C" {
#include "../export_cache.h"
#include "../table.h"
}

namespace {

TEST(Table, PutGetRemoveAndShrink) {
  cuinterpose_table table{};
  int values[1000];
  for (int i = 0; i < 1000; i++) {
    ASSERT_EQ(cuinterpose_table_put(&table, cuinterpose_key_u64(i * 7919), &values[i]), 0);
  }
  EXPECT_EQ(table.count, 1000u);
  for (int i = 0; i < 1000; i++) {
    EXPECT_EQ(cuinterpose_table_get(&table, cuinterpose_key_u64(i * 7919)), &values[i]);
  }
  EXPECT_EQ(cuinterpose_table_get(&table, cuinterpose_key_u64(1)), nullptr);
  // Replace keeps the count.
  ASSERT_EQ(cuinterpose_table_put(&table, cuinterpose_key_u64(0), &values[5]), 0);
  EXPECT_EQ(table.count, 1000u);
  EXPECT_EQ(cuinterpose_table_get(&table, cuinterpose_key_u64(0)), &values[5]);
  // Remove everything; the table must give its memory back.
  for (int i = 0; i < 1000; i++) {
    EXPECT_EQ(cuinterpose_table_remove(&table, cuinterpose_key_u64(i * 7919)), i == 0 ? &values[5] : &values[i]);
  }
  EXPECT_EQ(table.count, 0u);
  EXPECT_EQ(table.capacity, 0u);
  EXPECT_EQ(table.slots, nullptr);
  EXPECT_EQ(cuinterpose_table_remove(&table, cuinterpose_key_u64(3)), nullptr);
}

// Keys chosen to collide in a small table exercise probing, deletion from the
// middle of a chain, and reuse of tombstones.
TEST(Table, CollisionChainsAndTombstones) {
  cuinterpose_table table{};
  int marker[64];
  // Only the low bits index the table; these differ only in the high word.
  for (uint64_t i = 0; i < 6; i++) {
    cuinterpose_key key{12345, i};
    ASSERT_EQ(cuinterpose_table_put(&table, key, &marker[i]), 0);
  }
  cuinterpose_key middle{12345, 2};
  EXPECT_EQ(cuinterpose_table_remove(&table, middle), &marker[2]);
  for (uint64_t i = 0; i < 6; i++) {
    cuinterpose_key key{12345, i};
    EXPECT_EQ(cuinterpose_table_get(&table, key), i == 2 ? nullptr : &marker[i]) << i;
  }
  // Insert after a tombstone, then grow past it repeatedly.
  for (uint64_t i = 6; i < 60; i++) {
    cuinterpose_key key{12345, i};
    ASSERT_EQ(cuinterpose_table_put(&table, key, &marker[i]), 0);
  }
  for (uint64_t i = 0; i < 60; i++) {
    cuinterpose_key key{12345, i};
    EXPECT_EQ(cuinterpose_table_get(&table, key), i == 2 ? nullptr : &marker[i]) << i;
  }
  // Delete and re-add many times: the tombstone accounting must not fill the table.
  for (int round = 0; round < 2000; round++) {
    cuinterpose_key key{12345, static_cast<uint64_t>(100 + (round % 3))};
    ASSERT_EQ(cuinterpose_table_put(&table, key, &marker[0]), 0);
    EXPECT_EQ(cuinterpose_table_remove(&table, key), &marker[0]);
  }
  EXPECT_EQ(table.count, 59u);
  cuinterpose_table_clear(&table);
  EXPECT_EQ(table.capacity, 0u);
}

TEST(Table, ByteKeys) {
  cuinterpose_table table{};
  uint8_t a[16], b[16];
  std::memset(a, 0x11, sizeof(a));
  std::memset(b, 0x11, sizeof(b));
  b[15] = 0x12;
  int va = 1, vb = 2;
  ASSERT_EQ(cuinterpose_table_put(&table, cuinterpose_key_bytes(a), &va), 0);
  ASSERT_EQ(cuinterpose_table_put(&table, cuinterpose_key_bytes(b), &vb), 0);
  EXPECT_EQ(cuinterpose_table_get(&table, cuinterpose_key_bytes(a)), &va);
  EXPECT_EQ(cuinterpose_table_get(&table, cuinterpose_key_bytes(b)), &vb);
  int visited = 0;
  cuinterpose_table_each(&table, [](cuinterpose_key, void*, void* arg) { (*static_cast<int*>(arg))++; return 0; }, &visited);
  EXPECT_EQ(visited, 2);
  cuinterpose_table_clear(&table);
}

TEST(Ranges, InsertLookupCoverRemove) {
  cuinterpose_ranges ranges{};
  int v[8];
  ASSERT_EQ(cuinterpose_ranges_insert(&ranges, 0x1000, 0x2000, &v[0]), 0);
  ASSERT_EQ(cuinterpose_ranges_insert(&ranges, 0x3000, 0x4000, &v[1]), 0);
  ASSERT_EQ(cuinterpose_ranges_insert(&ranges, 0x2000, 0x3000, &v[2]), 0) << "adjacent is fine";
  EXPECT_EQ(cuinterpose_ranges_insert(&ranges, 0x1800, 0x1900, &v[3]), 1) << "inside an existing range";
  EXPECT_EQ(cuinterpose_ranges_insert(&ranges, 0x0800, 0x1800, &v[3]), 1) << "straddling";
  EXPECT_EQ(cuinterpose_ranges_insert(&ranges, 0x5000, 0x5000, &v[3]), 1) << "empty";
  EXPECT_EQ(ranges.count, 3u);
  EXPECT_EQ(ranges.items[0].value, &v[0]);
  EXPECT_EQ(ranges.items[1].value, &v[2]);
  EXPECT_EQ(ranges.items[2].value, &v[1]);

  EXPECT_EQ(cuinterpose_ranges_at(&ranges, 0x1000)->value, &v[0]);
  EXPECT_EQ(cuinterpose_ranges_at(&ranges, 0x1fff)->value, &v[0]);
  EXPECT_EQ(cuinterpose_ranges_at(&ranges, 0x2000)->value, &v[2]);
  EXPECT_EQ(cuinterpose_ranges_at(&ranges, 0x0fff), nullptr);
  EXPECT_EQ(cuinterpose_ranges_at(&ranges, 0x4000), nullptr);

  size_t first, last;
  EXPECT_EQ(cuinterpose_ranges_cover(&ranges, 0x1000, 0x3000, &first, &last), 0);
  EXPECT_EQ(first, 0u);
  EXPECT_EQ(last, 2u);
  EXPECT_EQ(cuinterpose_ranges_cover(&ranges, 0x1000, 0x2800, &first, &last), 1) << "cuts the second range";
  EXPECT_EQ(cuinterpose_ranges_cover(&ranges, 0x5000, 0x6000, &first, &last), 0);
  EXPECT_EQ(first, last) << "nothing intersects";
  EXPECT_EQ(cuinterpose_ranges_cover(&ranges, 0x0000, 0x9000, &first, &last), 0);
  EXPECT_EQ(last - first, 3u);

  cuinterpose_ranges_remove_at(&ranges, 1);
  EXPECT_EQ(ranges.count, 2u);
  EXPECT_EQ(cuinterpose_ranges_at(&ranges, 0x2800), nullptr);
  cuinterpose_ranges_remove_at(&ranges, 0);
  cuinterpose_ranges_remove_at(&ranges, 0);
  EXPECT_EQ(ranges.count, 0u);
  EXPECT_EQ(ranges.items, nullptr);
}

class ExportCache : public ::testing::Test {
 protected:
  void SetUp() override {
    std::memset(id, 0x42, sizeof(id));
    std::memset(auth, 0x99, sizeof(auth));
    cuinterpose_export_cache_resume();
  }
  void TearDown() override {
    cuinterpose_export_cache_quiesce();
    cuinterpose_export_cache_resume();
  }
  int memfd(const char* text) {
    int fd = memfd_create("t", MFD_CLOEXEC);
    if (write(fd, text, std::strlen(text)) != static_cast<ssize_t>(std::strlen(text))) abort();
    return fd;
  }
  uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  uint8_t auth[CUINTERPOSE_TOKEN_SIZE];
};

TEST_F(ExportCache, ServesDuplicatesAndChecksAuthorization) {
  ASSERT_EQ(cuinterpose_export_cache_put(id, auth, memfd("gpu")), 0);
  EXPECT_TRUE(cuinterpose_export_cache_has(id));
  EXPECT_EQ(cuinterpose_export_cache_count(), 1u);
  int dup = -1;
  const char* reason = nullptr;
  ASSERT_EQ(cuinterpose_export_cache_begin(id, auth, &dup, &reason), 0);
  ASSERT_GE(dup, 0);
  char buffer[4] = {};
  EXPECT_EQ(pread(dup, buffer, 3, 0), 3);
  EXPECT_STREQ(buffer, "gpu");
  EXPECT_NE(fcntl(dup, F_GETFD) & FD_CLOEXEC, 0);
  close(dup);
  cuinterpose_export_cache_end(id);

  uint8_t wrong[CUINTERPOSE_TOKEN_SIZE];
  std::memset(wrong, 0, sizeof(wrong));
  EXPECT_EQ(cuinterpose_export_cache_begin(id, wrong, &dup, &reason), -1);
  EXPECT_EQ(dup, -1);
  EXPECT_STREQ(reason, "creator export authorization mismatch");
  uint8_t other[CUINTERPOSE_ALLOCATION_ID_SIZE];
  std::memset(other, 0x43, sizeof(other));
  EXPECT_EQ(cuinterpose_export_cache_begin(other, auth, &dup, &reason), -1);
  EXPECT_STREQ(reason, "creator resource is unavailable");
}

TEST_F(ExportCache, DropWaitsForInFlightRequests) {
  ASSERT_EQ(cuinterpose_export_cache_put(id, auth, memfd("gpu")), 0);
  int dup = -1;
  const char* reason = nullptr;
  ASSERT_EQ(cuinterpose_export_cache_begin(id, auth, &dup, &reason), 0);
  std::atomic<bool> dropped{false};
  std::thread dropper([&] {
    cuinterpose_export_cache_drop(id);
    dropped = true;
  });
  usleep(20000);
  EXPECT_FALSE(dropped) << "drop must wait while a request is being served";
  // The descriptor we duplicated is still valid while the request is in flight.
  char buffer[4] = {};
  EXPECT_EQ(pread(dup, buffer, 3, 0), 3);
  close(dup);
  cuinterpose_export_cache_end(id);
  dropper.join();
  EXPECT_TRUE(dropped);
  EXPECT_FALSE(cuinterpose_export_cache_has(id));
  EXPECT_EQ(cuinterpose_export_cache_begin(id, auth, &dup, &reason), -1);
}

TEST_F(ExportCache, QuiesceStopsServingAndResumeAllowsRefill) {
  ASSERT_EQ(cuinterpose_export_cache_put(id, auth, memfd("gpu")), 0);
  cuinterpose_export_cache_quiesce();
  EXPECT_EQ(cuinterpose_export_cache_count(), 0u);
  int dup = -1;
  const char* reason = nullptr;
  EXPECT_EQ(cuinterpose_export_cache_begin(id, auth, &dup, &reason), -1);
  // Refilling while quiesced stores the descriptor but does not serve it.
  ASSERT_EQ(cuinterpose_export_cache_put(id, auth, memfd("again")), 0);
  EXPECT_EQ(cuinterpose_export_cache_begin(id, auth, &dup, &reason), -1) << "not accepting yet";
  cuinterpose_export_cache_resume();
  ASSERT_EQ(cuinterpose_export_cache_begin(id, auth, &dup, &reason), 0);
  close(dup);
  cuinterpose_export_cache_end(id);
}

TEST_F(ExportCache, ReplacingAnEntryClosesTheOldDescriptorAfterDrain) {
  int old = memfd("old");
  ASSERT_EQ(cuinterpose_export_cache_put(id, auth, old), 0);
  ASSERT_EQ(cuinterpose_export_cache_put(id, auth, memfd("new")), 0);
  EXPECT_EQ(cuinterpose_export_cache_count(), 1u);
  EXPECT_EQ(fcntl(old, F_GETFD), -1) << "the replaced descriptor must have been closed";
  int dup = -1;
  const char* reason = nullptr;
  ASSERT_EQ(cuinterpose_export_cache_begin(id, auth, &dup, &reason), 0);
  char buffer[4] = {};
  EXPECT_EQ(pread(dup, buffer, 3, 0), 3);
  EXPECT_STREQ(buffer, "new");
  close(dup);
  cuinterpose_export_cache_end(id);
}

// Many threads serving while another drops and re-adds the entry: the
// sanitizers catch any use of a closed descriptor or freed entry.
TEST_F(ExportCache, ConcurrentServeAndDropIsSafe) {
  ASSERT_EQ(cuinterpose_export_cache_put(id, auth, memfd("gpu")), 0);
  std::atomic<bool> stop{false};
  std::atomic<int> served{0};
  std::vector<std::thread> workers;
  for (int i = 0; i < 4; i++) {
    workers.emplace_back([&] {
      while (!stop) {
        int dup = -1;
        const char* reason = nullptr;
        if (cuinterpose_export_cache_begin(id, auth, &dup, &reason) == 0) {
          char buffer[4] = {};
          EXPECT_EQ(pread(dup, buffer, 3, 0), 3);
          EXPECT_STREQ(buffer, "gpu");
          close(dup);
          cuinterpose_export_cache_end(id);
          served++;
        }
      }
    });
  }
  for (int round = 0; round < 200; round++) {
    cuinterpose_export_cache_drop(id);
    ASSERT_EQ(cuinterpose_export_cache_put(id, auth, memfd("gpu")), 0);
  }
  stop = true;
  for (auto& worker : workers) worker.join();
  EXPECT_GT(served.load(), 0);
}

}  // namespace
