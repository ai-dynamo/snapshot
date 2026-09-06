/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 *
 * ns-bind-mount installs and removes the two mounts used by restore:
 *
 *   mount-bundle-fd <namespace-fd>
 *   mount-checkpoint-fd <namespace-fd> <checkpoint-path>
 *   unmount-bundle-fd <namespace-fd> [created]
 *   unmount-checkpoint-fd <namespace-fd> [created]
 *
 * The caller pins the target mount namespace and passes its descriptor through
 * ExtraFiles. Bundle and checkpoint policy is deliberately fixed here: callers
 * cannot select arbitrary host sources, container destinations, or attributes.
 */

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <sched.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

#ifndef __NR_open_tree
#define __NR_open_tree 428
#endif
#ifndef __NR_move_mount
#define __NR_move_mount 429
#endif
#ifndef __NR_mount_setattr
#define __NR_mount_setattr 442
#endif

#define OPEN_TREE_CLONE 1
#define MOVE_MOUNT_F_EMPTY_PATH 0x00000004

#ifndef MOUNT_ATTR_RDONLY
#define MOUNT_ATTR_RDONLY 0x00000001
#define MOUNT_ATTR_NOSUID 0x00000002
#define MOUNT_ATTR_NODEV 0x00000004
struct mount_attr {
  uint64_t attr_set;
  uint64_t attr_clr;
  uint64_t propagation;
  uint64_t userns_fd;
};
#endif
#ifndef MOUNT_ATTR_NOEXEC
#define MOUNT_ATTR_NOEXEC 0x00000008
#endif

#define BUNDLE_SOURCE "/snapshot-binaries"
#define BUNDLE_DESTINATION "/tmp/snapshot-binaries"
#define CHECKPOINT_ROOT "/checkpoints"
#define CHECKPOINT_DESTINATION "/tmp/checkpoint"
#define PAGEBROKER_RESTORE_ROOT "/pagebroker/staging/restore"
#define PAGEBROKER_DESTINATION "/tmp/pagebroker"

static int
parse_fd(const char* value)
{
  char* end;
  long fd = strtol(value, &end, 10);
  if (*end != '\0' || fd < 0 || fd > INT_MAX) {
    fprintf(stderr, "invalid namespace fd: %s\n", value);
    return -1;
  }
  return (int)fd;
}

static int
is_portable_path_char(char value)
{
  return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
         (value >= '0' && value <= '9') || value == '_' || value == '-' || value == '.';
}

/* The Go caller constructs this path from validated single path elements. This
 * second check keeps the privileged helper safe if that caller regresses. */
static int
check_storage_path(const char* value, const char* root)
{
  size_t root_len = strlen(root);
  if (strncmp(value, root, root_len) != 0 || value[root_len] != '/') {
    fprintf(stderr, "storage path must be below %s: %s\n", root, value);
    return -1;
  }

  const char* component = value + root_len + 1;
  for (const char* p = component;; p++) {
    if (*p != '/' && *p != '\0') {
      if (!is_portable_path_char(*p)) {
        fprintf(stderr, "storage path contains unsupported character: %s\n", value);
        return -1;
      }
      continue;
    }

    size_t len = (size_t)(p - component);
    if (len == 0 || (len == 1 && component[0] == '.') ||
        (len == 2 && component[0] == '.' && component[1] == '.')) {
      fprintf(stderr, "storage path contains an empty or traversing component: %s\n", value);
      return -1;
    }
    if (*p == '\0')
      return 0;
    component = p + 1;
  }
}

static int
check_checkpoint_path(const char* value)
{
  return check_storage_path(value, CHECKPOINT_ROOT);
}

static int
check_pagebroker_path(const char* value)
{
  return check_storage_path(value, PAGEBROKER_RESTORE_ROOT);
}

/* Returns 1 when this invocation created dst, 0 when a plain directory was
 * already present, and -1 on error. */
static int
ensure_destination(const char* dst)
{
  if (mkdir(dst, 0700) == 0)
    return 1;
  if (errno != EEXIST) {
    fprintf(stderr, "mkdir %s: %s\n", dst, strerror(errno));
    return -1;
  }

  struct stat st;
  if (lstat(dst, &st) < 0) {
    fprintf(stderr, "lstat %s: %s\n", dst, strerror(errno));
    return -1;
  }
  if (!S_ISDIR(st.st_mode)) {
    fprintf(stderr, "destination is not a plain directory: %s\n", dst);
    return -1;
  }
  return 0;
}

static int
install_mount(int ns_fd, const char* src, const char* dst, uint64_t attributes)
{
  int tree_fd = (int)syscall(
      __NR_open_tree,
      AT_FDCWD,
      src,
      OPEN_TREE_CLONE | O_CLOEXEC);
  if (tree_fd < 0) {
    fprintf(stderr, "open_tree %s: %s\n", src, strerror(errno));
    return 1;
  }

  struct mount_attr attr = {.attr_set = attributes};
  if (syscall(__NR_mount_setattr, tree_fd, "", AT_EMPTY_PATH, &attr, sizeof attr) < 0) {
    fprintf(stderr, "mount_setattr %s: %s\n", src, strerror(errno));
    close(tree_fd);
    return 1;
  }

  if (setns(ns_fd, CLONE_NEWNS) < 0) {
    fprintf(stderr, "setns fd %d: %s\n", ns_fd, strerror(errno));
    close(tree_fd);
    return 1;
  }

  int created = ensure_destination(dst);
  if (created < 0) {
    close(tree_fd);
    return 1;
  }

  if (syscall(
          __NR_move_mount,
          tree_fd,
          "",
          AT_FDCWD,
          dst,
          MOVE_MOUNT_F_EMPTY_PATH) < 0) {
    fprintf(stderr, "move_mount -> %s: %s\n", dst, strerror(errno));
    close(tree_fd);
    if (created)
      rmdir(dst);
    return 1;
  }
  close(tree_fd);
  printf("created_dst=%d\n", created);
  return 0;
}

static int
remove_mount(int ns_fd, const char* dst, int created)
{
  if (setns(ns_fd, CLONE_NEWNS) < 0) {
    fprintf(stderr, "setns fd %d: %s\n", ns_fd, strerror(errno));
    return 1;
  }
  if (umount2(dst, MNT_DETACH) < 0 && errno != ENOENT && errno != EINVAL) {
    fprintf(stderr, "umount2 %s: %s\n", dst, strerror(errno));
    return 1;
  }
  if (created)
    rmdir(dst);
  return 0;
}

static int
mount_bundle(int argc, char* argv[])
{
  if (argc != 3) {
    fprintf(stderr, "usage: ns-bind-mount mount-bundle-fd <namespace-fd>\n");
    return 1;
  }
  int ns_fd = parse_fd(argv[2]);
  if (ns_fd < 0)
    return 1;
  return install_mount(
      ns_fd,
      BUNDLE_SOURCE,
      BUNDLE_DESTINATION,
      MOUNT_ATTR_RDONLY | MOUNT_ATTR_NOSUID | MOUNT_ATTR_NODEV);
}

static int
mount_checkpoint(int argc, char* argv[])
{
  if (argc != 4) {
    fprintf(stderr, "usage: ns-bind-mount mount-checkpoint-fd <namespace-fd> <checkpoint-path>\n");
    return 1;
  }
  int ns_fd = parse_fd(argv[2]);
  if (ns_fd < 0 || check_checkpoint_path(argv[3]) < 0)
    return 1;
  return install_mount(
      ns_fd,
      argv[3],
      CHECKPOINT_DESTINATION,
      MOUNT_ATTR_RDONLY | MOUNT_ATTR_NOSUID | MOUNT_ATTR_NODEV | MOUNT_ATTR_NOEXEC);
}

static int
mount_pagebroker(int argc, char* argv[])
{
  if (argc != 4) {
    fprintf(stderr, "usage: ns-bind-mount mount-pagebroker-fd <namespace-fd> <staging-path>\n");
    return 1;
  }
  int ns_fd = parse_fd(argv[2]);
  if (ns_fd < 0 || check_pagebroker_path(argv[3]) < 0)
    return 1;
  return install_mount(
      ns_fd,
      argv[3],
      PAGEBROKER_DESTINATION,
      MOUNT_ATTR_RDONLY | MOUNT_ATTR_NOSUID | MOUNT_ATTR_NODEV | MOUNT_ATTR_NOEXEC);
}

static int
unmount_role(int argc, char* argv[], const char* dst, const char* usage)
{
  if (argc != 3 && argc != 4) {
    fprintf(stderr, "%s\n", usage);
    return 1;
  }
  int ns_fd = parse_fd(argv[2]);
  if (ns_fd < 0)
    return 1;
  int created = 0;
  if (argc == 4) {
    if (strcmp(argv[3], "created") != 0) {
      fprintf(stderr, "%s\n", usage);
      return 1;
    }
    created = 1;
  }
  return remove_mount(ns_fd, dst, created);
}

int
main(int argc, char* argv[])
{
  if (argc < 2) {
    fprintf(stderr, "missing role command\n");
    return 1;
  }
  if (strcmp(argv[1], "mount-bundle-fd") == 0)
    return mount_bundle(argc, argv);
  if (strcmp(argv[1], "mount-checkpoint-fd") == 0)
    return mount_checkpoint(argc, argv);
  if (strcmp(argv[1], "mount-pagebroker-fd") == 0)
    return mount_pagebroker(argc, argv);
  if (strcmp(argv[1], "unmount-bundle-fd") == 0)
    return unmount_role(
        argc,
        argv,
        BUNDLE_DESTINATION,
        "usage: ns-bind-mount unmount-bundle-fd <namespace-fd> [created]");
  if (strcmp(argv[1], "unmount-checkpoint-fd") == 0)
    return unmount_role(
        argc,
        argv,
        CHECKPOINT_DESTINATION,
        "usage: ns-bind-mount unmount-checkpoint-fd <namespace-fd> [created]");
  if (strcmp(argv[1], "unmount-pagebroker-fd") == 0)
    return unmount_role(
        argc,
        argv,
        PAGEBROKER_DESTINATION,
        "usage: ns-bind-mount unmount-pagebroker-fd <namespace-fd> [created]");

  fprintf(stderr, "unknown role command: %s\n", argv[1]);
  return 1;
}
