/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 *
 * ns-bind-mount: bind-mount or unmount a directory in another process's mount namespace.
 *
 * Mount:      ns-bind-mount <pid> <src> <dst> [ro] [noexec]
 * Mount-fd:   ns-bind-mount mount-fd <ns_fd> <src> <dst> [ro] [noexec]
 * Unmount:    ns-bind-mount umount <pid> <dst>
 * Unmount-fd: ns-bind-mount umount-fd <ns_fd> <dst> [created]
 *
 * mount-fd is the preferred form: the caller (Go) opens /proc/<pid>/ns/mnt
 * before launching the helper and passes the fd through ExtraFiles, so the
 * namespace is pinned at open time rather than re-resolved from the PID inside
 * the helper.  Both mount paths apply mount_setattr(MOUNT_ATTR_RDONLY) to the
 * cloned tree *before* attaching so the mount is never visible as writable
 * inside the target namespace.  Unmount enters the namespace the same way and
 * calls umount2(MNT_DETACH). Optional noexec policy is applied to checkpoint
 * artifacts while the binary bundle remains executable. Both subcommands run as single-threaded C
 * processes so setns(CLONE_NEWNS) is allowed (prohibited in multithreaded Go
 * programs).
 *
 * Requires Linux 5.12+ (mount_setattr; open_tree/move_mount need only 5.2).
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

/* Destinations mirror the Go constants in internal/nsmount/injector.go. */
#define ALLOWED_BUNDLE_DST "/tmp/snapshot-binaries"
#define ALLOWED_CHECKPOINT_DST "/tmp/checkpoint"
#define ALLOWED_BUNDLE_SRC "/snapshot-binaries"
#define ALLOWED_CHECKPOINT_SRC "/checkpoints"

static int
is_portable_path_char(char value)
{
  return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
         (value >= '0' && value <= '9') || value == '_' || value == '-' || value == '.';
}

static int
check_absolute_path(const char* path, const char* label)
{
  if (path[0] != '/' || path[1] == '\0') {
    fprintf(stderr, "%s must be a non-root absolute path: %s\n", label, path);
    return -1;
  }

  const char* component = path + 1;
  for (const char* p = component;; p++) {
    if (*p != '/' && *p != '\0') {
      if (!is_portable_path_char(*p)) {
        fprintf(stderr, "%s contains unsupported character: %s\n", label, path);
        return -1;
      }
      continue;
    }

    size_t len = (size_t)(p - component);
    if (len == 0 || (len == 1 && component[0] == '.') ||
        (len == 2 && component[0] == '.' && component[1] == '.')) {
      fprintf(stderr, "%s contains an empty or traversing component: %s\n", label, path);
      return -1;
    }
    if (*p == '\0')
      break;
    component = p + 1;
  }
  return 0;
}

static int
check_source(const char* path, const char* dst)
{
  if (check_absolute_path(path, "src") < 0)
    return -1;

  const char* allowed_root;
  if (strcmp(dst, ALLOWED_BUNDLE_DST) == 0) {
    allowed_root = ALLOWED_BUNDLE_SRC;
  } else if (strcmp(dst, ALLOWED_CHECKPOINT_DST) == 0) {
    allowed_root = ALLOWED_CHECKPOINT_SRC;
  } else {
    fprintf(stderr, "unsupported destination for source validation: %s\n", dst);
    return -1;
  }
  size_t root_len = strlen(allowed_root);
  if (strncmp(path, allowed_root, root_len) != 0 || (path[root_len] != '\0' && path[root_len] != '/')) {
    fprintf(stderr, "src must be within allowed source root %s: %s\n", allowed_root, path);
    return -1;
  }
  return 0;
}

static int
check_destination(const char* path)
{
  if (strcmp(path, ALLOWED_BUNDLE_DST) != 0 && strcmp(path, ALLOWED_CHECKPOINT_DST) != 0) {
    fprintf(stderr, "dst must be %s or %s: %s\n", ALLOWED_BUNDLE_DST, ALLOWED_CHECKPOINT_DST, path);
    return -1;
  }
  return 0;
}

static int
sys_open_tree(int dfd, const char* path, unsigned flags)
{
  return (int)syscall(__NR_open_tree, dfd, path, flags);
}

static int
sys_move_mount(int from_dfd, const char* from_path, int to_dfd, const char* to_path, unsigned flags)
{
  return (int)syscall(__NR_move_mount, from_dfd, from_path, to_dfd, to_path, flags);
}

/* Enter the mount namespace of the given pid.  Returns 0 on success. */
static int
enter_mnt_ns(int pid)
{
  char ns_path[256];
  snprintf(ns_path, sizeof(ns_path), "/proc/%d/ns/mnt", pid);
  int ns_fd = open(ns_path, O_RDONLY | O_CLOEXEC);
  if (ns_fd < 0) {
    fprintf(stderr, "open %s: %s\n", ns_path, strerror(errno));
    return -1;
  }
  if (setns(ns_fd, CLONE_NEWNS) < 0) {
    fprintf(stderr, "setns %s: %s\n", ns_path, strerror(errno));
    close(ns_fd);
    return -1;
  }
  close(ns_fd);
  return 0;
}

/* Parse a positive pid from str.  Returns the pid on success, -1 on error. */
static int
parse_pid(const char* str)
{
  char* end;
  long val = strtol(str, &end, 10);
  if (*end != '\0' || val <= 0 || val > INT_MAX) {
    fprintf(stderr, "invalid pid: %s\n", str);
    return -1;
  }
  return (int)val;
}

struct mount_options {
  int readonly;
  int noexec;
};

static int
parse_mount_options(int argc, char* argv[], int first, struct mount_options* options)
{
  memset(options, 0, sizeof(*options));
  for (int i = first; i < argc; i++) {
    if (strcmp(argv[i], "ro") == 0) {
      options->readonly = 1;
    } else if (strcmp(argv[i], "noexec") == 0) {
      options->noexec = 1;
    } else {
      fprintf(stderr, "unknown mount option: %s\n", argv[i]);
      return -1;
    }
  }
  return 0;
}

/* Apply mount attributes to tree_fd before attaching it so the mount is never
 * visible with weaker policy inside the target namespace. */
static int
apply_mount_attrs(int tree_fd, const struct mount_options* options)
{
  struct mount_attr attr = {
      .attr_set = 0,
  };
  if (options->readonly)
    attr.attr_set |= MOUNT_ATTR_RDONLY | MOUNT_ATTR_NOSUID | MOUNT_ATTR_NODEV;
  if (options->noexec)
    attr.attr_set |= MOUNT_ATTR_NOEXEC;
  if (attr.attr_set == 0)
    return 0;
  if (syscall(__NR_mount_setattr, tree_fd, "", AT_EMPTY_PATH, &attr, sizeof attr) < 0) {
    fprintf(stderr, "mount_setattr: %s\n", strerror(errno));
    return -1;
  }
  return 0;
}

/* Create or verify the target directory.  Returns 1 if this call created it,
 * 0 if it already existed as a plain directory, -1 on error. */
static int
ensure_dst_dir(const char* dst)
{
  if (mkdir(dst, 0700) == 0)
    return 1;
  if (errno != EEXIST) {
    fprintf(stderr, "mkdir %s: %s\n", dst, strerror(errno));
    return -1;
  }
  /* dst already existed — verify it is a plain directory, not a symlink,
   * so a process inside the namespace cannot redirect the mount. */
  struct stat st;
  if (lstat(dst, &st) < 0) {
    fprintf(stderr, "lstat %s: %s\n", dst, strerror(errno));
    return -1;
  }
  if (!S_ISDIR(st.st_mode)) {
    fprintf(stderr, "dst %s exists but is not a plain directory\n", dst);
    return -1;
  }
  return 0;
}

static int
do_umount(int argc, char* argv[])
{
  if (argc < 4) {
    fprintf(stderr, "usage: ns-bind-mount umount <pid> <dst>\n");
    return 1;
  }
  int pid = parse_pid(argv[2]);
  if (pid < 0)
    return 1;
  const char* dst = argv[3];

  if (check_destination(dst) < 0)
    return 1;

  if (enter_mnt_ns(pid) < 0)
    return 1;

  /* MNT_DETACH: lazy unmount — succeeds even if the path is busy. */
  if (umount2(dst, MNT_DETACH) < 0) {
    if (errno != ENOENT && errno != EINVAL) {
      fprintf(stderr, "umount2 %s: %s\n", dst, strerror(errno));
      return 1;
    }
    /* Already gone (CRIU removed it during namespace restore).
     * Fall through to rmdir so we don't leave the directory behind. */
  }

  /* Remove the directory we created at mount time. Ignore errors — the
   * directory may be non-empty or already gone, neither is fatal. */
  rmdir(dst);
  return 0;
}

/* Unmount via an open namespace fd rather than a pid.  The caller (Go) passes
 * an already-open /proc/<pid>/ns/mnt fd inherited through ExtraFiles; using
 * the fd avoids the PID-reuse window between mount time and cleanup.
 * The optional "created" argument instructs the helper to remove dst — only
 * set when the mount subcommand reported that it created the directory. */
static int
do_umount_fd(int argc, char* argv[])
{
  if (argc < 4) {
    fprintf(stderr, "usage: ns-bind-mount umount-fd <ns_fd> <dst> [created]\n");
    return 1;
  }
  char* end;
  long fd_val = strtol(argv[2], &end, 10);
  if (*end != '\0' || fd_val < 0 || fd_val > INT_MAX) {
    fprintf(stderr, "invalid fd: %s\n", argv[2]);
    return 1;
  }
  int ns_fd = (int)fd_val;
  const char* dst = argv[3];
  int created_dst = (argc >= 5 && strcmp(argv[4], "created") == 0);

  if (check_destination(dst) < 0)
    return 1;

  if (setns(ns_fd, CLONE_NEWNS) < 0) {
    fprintf(stderr, "setns fd %d: %s\n", ns_fd, strerror(errno));
    return 1;
  }

  if (umount2(dst, MNT_DETACH) < 0) {
    if (errno != ENOENT && errno != EINVAL) {
      fprintf(stderr, "umount2 %s: %s\n", dst, strerror(errno));
      return 1;
    }
    /* Already gone (CRIU removed it during namespace restore).
     * Fall through so we clean up the directory if we created it. */
  }

  /* Only remove the directory if the mount subcommand created it. */
  if (created_dst)
    rmdir(dst);
  return 0;
}

/* Mount via an already-open namespace fd.  The caller (Go) opens
 * /proc/<pid>/ns/mnt before launching the helper and passes the fd through
 * ExtraFiles, so the namespace is pinned at Go-side open time rather than
 * re-resolved from the PID — eliminating the PID-reuse window. */
static int
do_mount_fd(int argc, char* argv[])
{
  if (argc < 5) {
    fprintf(stderr, "usage: ns-bind-mount mount-fd <ns_fd> <src> <dst> [ro] [noexec]\n");
    return 1;
  }
  char* end;
  long fd_val = strtol(argv[2], &end, 10);
  if (*end != '\0' || fd_val < 0 || fd_val > INT_MAX) {
    fprintf(stderr, "invalid fd: %s\n", argv[2]);
    return 1;
  }
  int ns_fd = (int)fd_val;
  const char* src = argv[3];
  const char* dst = argv[4];
  struct mount_options options;
  if (parse_mount_options(argc, argv, 5, &options) < 0)
    return 1;

  if (check_destination(dst) < 0)
    return 1;
  if (check_source(src, dst) < 0)
    return 1;

  /* Clone the source mount tree before entering the target namespace. */
  int tree_fd = sys_open_tree(AT_FDCWD, src, OPEN_TREE_CLONE | O_CLOEXEC);
  if (tree_fd < 0) {
    fprintf(stderr, "open_tree %s: %s\n", src, strerror(errno));
    return 1;
  }

  if (apply_mount_attrs(tree_fd, &options) < 0) {
    close(tree_fd);
    return 1;
  }

  /* Enter the target namespace via the inherited fd. */
  if (setns(ns_fd, CLONE_NEWNS) < 0) {
    fprintf(stderr, "setns fd %d: %s\n", ns_fd, strerror(errno));
    close(tree_fd);
    return 1;
  }

  int created_dst = ensure_dst_dir(dst);
  if (created_dst < 0) {
    close(tree_fd);
    return 1;
  }

  /* Move the cloned mount into the target namespace at dst. */
  if (sys_move_mount(tree_fd, "", AT_FDCWD, dst, MOVE_MOUNT_F_EMPTY_PATH) < 0) {
    fprintf(stderr, "move_mount -> %s: %s\n", dst, strerror(errno));
    close(tree_fd);
    if (created_dst)
      rmdir(dst);
    return 1;
  }
  close(tree_fd);

  printf("created_dst=%d\n", created_dst);
  return 0;
}

int
main(int argc, char* argv[])
{
  if (argc >= 2 && strcmp(argv[1], "mount-fd") == 0)
    return do_mount_fd(argc, argv);
  if (argc >= 2 && strcmp(argv[1], "umount-fd") == 0)
    return do_umount_fd(argc, argv);
  if (argc >= 2 && strcmp(argv[1], "umount") == 0)
    return do_umount(argc, argv);

  if (argc < 4) {
    fprintf(
        stderr,
        "usage: ns-bind-mount <pid> <src> <dst> [ro] [noexec]\n"
        "       ns-bind-mount mount-fd <ns_fd> <src> <dst> [ro] [noexec]\n"
        "       ns-bind-mount umount <pid> <dst>\n"
        "       ns-bind-mount umount-fd <ns_fd> <dst> [created]\n");
    return 1;
  }

  int pid = parse_pid(argv[1]);
  if (pid < 0)
    return 1;
  const char* src = argv[2];
  const char* dst = argv[3];
  struct mount_options options;
  if (parse_mount_options(argc, argv, 4, &options) < 0)
    return 1;

  if (check_destination(dst) < 0)
    return 1;
  if (check_source(src, dst) < 0)
    return 1;

  /* Clone the source mount tree before entering the target namespace. */
  int tree_fd = sys_open_tree(AT_FDCWD, src, OPEN_TREE_CLONE | O_CLOEXEC);
  if (tree_fd < 0) {
    fprintf(stderr, "open_tree %s: %s\n", src, strerror(errno));
    return 1;
  }

  if (apply_mount_attrs(tree_fd, &options) < 0) {
    close(tree_fd);
    return 1;
  }

  /* Enter the target process's mount namespace. */
  if (enter_mnt_ns(pid) < 0) {
    close(tree_fd);
    return 1;
  }

  int created_dst = ensure_dst_dir(dst);
  if (created_dst < 0) {
    close(tree_fd);
    return 1;
  }

  /* Move the cloned mount into the target namespace at dst. */
  if (sys_move_mount(tree_fd, "", AT_FDCWD, dst, MOVE_MOUNT_F_EMPTY_PATH) < 0) {
    fprintf(stderr, "move_mount -> %s: %s\n", dst, strerror(errno));
    close(tree_fd);
    if (created_dst)
      rmdir(dst);
    return 1;
  }
  close(tree_fd);

  printf("created_dst=%d\n", created_dst);
  return 0;
}
