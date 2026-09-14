/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include <fcntl.h>
#include <stdint.h>
#include <string.h>
#include <unistd.h>

#include <gtest/gtest.h>

extern "C" {
#include "util/cleanup.h"
#include "util/id.h"
#include "util/time.h"
}

namespace {

TEST(util, identity_compare_and_copy)
{
  char id[CUINTERPOSE_ID_SIZE] = "0123456789abcdef0123456789abcdef";
  char same[CUINTERPOSE_ID_SIZE] = "0123456789abcdef0123456789abcdef";
  char different[CUINTERPOSE_ID_SIZE] = "ffffffffffffffffffffffffffffffff";
  char copied[CUINTERPOSE_ID_SIZE] = {0};
  uint8_t allocation[CUINTERPOSE_ALLOCATION_ID_SIZE];
  uint8_t other[CUINTERPOSE_ALLOCATION_ID_SIZE];

  EXPECT_TRUE(id_eq(id, same));
  EXPECT_FALSE(id_eq(id, different));
  id_copy(copied, id);
  EXPECT_TRUE(id_eq(copied, id));

  memset(allocation, 0x5a, sizeof(allocation));
  memset(other, 0x5a, sizeof(other));
  EXPECT_TRUE(allocation_id_eq(allocation, other));
  other[CUINTERPOSE_ALLOCATION_ID_SIZE - 1] = 0;
  EXPECT_FALSE(allocation_id_eq(allocation, other));
  allocation_id_copy(other, allocation);
  EXPECT_TRUE(allocation_id_eq(allocation, other));
}

TEST(util, hex_digit)
{
  EXPECT_EQ(hex_digit('0'), 0);
  EXPECT_EQ(hex_digit('9'), 9);
  EXPECT_EQ(hex_digit('a'), 10);
  EXPECT_EQ(hex_digit('f'), 15);
  EXPECT_EQ(hex_digit('g'), -1);
  EXPECT_EQ(hex_digit('A'), -1);
  EXPECT_EQ(hex_digit(EOF), -1);
}

TEST(util, elapsed_time_handles_nanosecond_rollover)
{
  const timespec start = {.tv_sec = 10, .tv_nsec = 900000000};
  const timespec end = {.tv_sec = 12, .tv_nsec = 100000000};

  EXPECT_DOUBLE_EQ(elapsed_milliseconds(&start, &end), 1200.0);
}

TEST(util, cleanup_closes_fd_and_take_releases_it)
{
  int taken;
  int closed;

  taken = open("/dev/null", O_RDONLY);
  ASSERT_GE(taken, 0);
  {
    CUINTERPOSE_CLEANUP(close_fd) int fd = open("/dev/null", O_RDONLY);
    ASSERT_GE(fd, 0);
    closed = fd;
  }
  EXPECT_EQ(fcntl(closed, F_GETFD), -1);
  {
    CUINTERPOSE_CLEANUP(close_fd) int fd = open("/dev/null", O_RDONLY);
    ASSERT_GE(fd, 0);
    closed = take_fd(&fd);
  }
  EXPECT_GE(fcntl(closed, F_GETFD), 0);
  close(closed);
  close(taken);
}

TEST(util, cleanup_closes_file)
{
  FILE* file = tmpfile();
  int raw;
  ASSERT_NE(file, nullptr);
  raw = fileno(file);
  ASSERT_GE(raw, 0);
  {
    CUINTERPOSE_CLEANUP(close_file) FILE* scoped = file;
    (void)scoped;
  }
  EXPECT_EQ(fcntl(raw, F_GETFD), -1);
  {
    CUINTERPOSE_CLEANUP(close_file) FILE* nothing = nullptr;
    (void)nothing;
  }
}

TEST(util, unlink_guard_removes_unless_disarmed)
{
  char path[] = "/tmp/cuinterpose-util-test-XXXXXX";
  int fd = mkstemp(path);
  ASSERT_GE(fd, 0);
  close(fd);
  {
    CUINTERPOSE_CLEANUP(unlink_guard_release) struct unlink_guard guard = {path, true};
  }
  EXPECT_EQ(access(path, F_OK), -1);

  {
    char kept[] = "/tmp/cuinterpose-util-test-XXXXXX";
    fd = mkstemp(kept);
    ASSERT_GE(fd, 0);
    close(fd);
    {
      CUINTERPOSE_CLEANUP(unlink_guard_release) struct unlink_guard guard = {kept, true};
      guard.armed = false;
    }
    EXPECT_EQ(access(kept, F_OK), 0);
    EXPECT_EQ(unlink(kept), 0);
  }
}

} // namespace
