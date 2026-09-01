// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdbool.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <unistd.h>

#include "criu-plugin.h"

#define NVIDIA_DEVICE_MAJOR 195
#define METADATA_PREFIX "snapshot-nvidia-fd-"
#define log_error(fmt, ...) fprintf(stderr, "snapshot nvidia residual devices: " fmt, ##__VA_ARGS__)
#define log_info(fmt, ...) fprintf(stderr, "snapshot nvidia residual devices: " fmt, ##__VA_ARGS__)

// Snapshot drives CUDA lock/checkpoint/restore itself, so it must not load
// CRIU's full CUDA plugin. Preserve the one non-lifecycle behavior that plugin
// enables: CUDA process trees must be seized with ptrace interrupts rather
// than by freezing their cgroup.
extern void set_compel_interrupt_only_mode(void);

static int
snapshot_nvidia_init(int stage)
{
  if (stage == CR_PLUGIN_STAGE__DUMP) {
    set_compel_interrupt_only_mode();
    log_info("enabled CRIU compel interrupt-only mode\n");
  }
  return 0;
}

static bool
is_nvidia_device_name(const char* name)
{
  const char* suffix;

  if (strcmp(name, "nvidiactl") == 0)
    return true;
  if (strncmp(name, "nvidia", strlen("nvidia")) != 0)
    return false;

  suffix = name + strlen("nvidia");
  if (*suffix == '\0')
    return false;
  for (; *suffix != '\0'; suffix++) {
    if (!isdigit((unsigned char)*suffix))
      return false;
  }
  return true;
}

static int
device_name_from_fd(int fd, char* name, size_t name_size)
{
  char fd_path[64];
  char target[PATH_MAX];
  const char* basename;
  struct stat st;
  ssize_t length;

  if (fstat(fd, &st) != 0)
    return -errno;
  if (!S_ISCHR(st.st_mode) || major(st.st_rdev) != NVIDIA_DEVICE_MAJOR)
    return -ENOTSUP;

  if (snprintf(fd_path, sizeof(fd_path), "/proc/self/fd/%d", fd) >= (int)sizeof(fd_path))
    return -ENAMETOOLONG;
  length = readlink(fd_path, target, sizeof(target) - 1);
  if (length < 0)
    return -errno;
  target[length] = '\0';

  basename = strrchr(target, '/');
  basename = basename == NULL ? target : basename + 1;
  if (!is_nvidia_device_name(basename))
    return -ENOTSUP;
  if (snprintf(name, name_size, "%s", basename) >= (int)name_size)
    return -ENAMETOOLONG;
  return 0;
}

static int
metadata_name(int id, char* path, size_t path_size)
{
  if (snprintf(path, path_size, METADATA_PREFIX "%08x", (unsigned int)id) >= (int)path_size)
    return -ENAMETOOLONG;
  return 0;
}

static int
snapshot_dump_nvidia_fd(int fd, int id)
{
  char device_name[NAME_MAX + 1];
  char metadata[sizeof(METADATA_PREFIX) + 8];
  int image_dir;
  int metadata_fd;
  int ret;
  size_t length;
  ssize_t written;

  ret = device_name_from_fd(fd, device_name, sizeof(device_name));
  if (ret != 0)
    return ret;
  ret = metadata_name(id, metadata, sizeof(metadata));
  if (ret != 0)
    return ret;

  image_dir = criu_get_image_dir();
  metadata_fd = openat(image_dir, metadata, O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0600);
  if (metadata_fd < 0) {
    log_error("openat(%s) failed: %s\n", metadata, strerror(errno));
    return -errno;
  }

  length = strlen(device_name);
  written = write(metadata_fd, device_name, length);
  if (written != (ssize_t)length) {
    ret = written < 0 ? -errno : -EIO;
    log_error("write(%s) failed: %s\n", metadata, strerror(-ret));
    close(metadata_fd);
    unlinkat(image_dir, metadata, 0);
    return ret;
  }
  if (close(metadata_fd) != 0) {
    ret = -errno;
    unlinkat(image_dir, metadata, 0);
    return ret;
  }

  log_info("externalized /dev/%s as CRIU file id %#x\n", device_name, id);
  return 0;
}

static int
snapshot_restore_nvidia_fd(int id, bool* retry_needed)
{
  char device_name[NAME_MAX + 1];
  char device_path[sizeof("/dev/") + NAME_MAX];
  char metadata[sizeof(METADATA_PREFIX) + 8];
  int image_dir;
  int metadata_fd;
  int device_fd;
  int ret;
  ssize_t length;

  if (retry_needed == NULL)
    return -EINVAL;
  *retry_needed = false;

  ret = metadata_name(id, metadata, sizeof(metadata));
  if (ret != 0)
    return ret;
  image_dir = criu_get_image_dir();
  metadata_fd = openat(image_dir, metadata, O_RDONLY | O_CLOEXEC);
  if (metadata_fd < 0) {
    if (errno == ENOENT)
      return -ENOTSUP;
    return -errno;
  }

  length = read(metadata_fd, device_name, sizeof(device_name) - 1);
  ret = length < 0 ? -errno : 0;
  if (close(metadata_fd) != 0 && ret == 0)
    ret = -errno;
  if (ret != 0)
    return ret;
  if (length == 0 || length == (ssize_t)(sizeof(device_name) - 1))
    return -EINVAL;
  device_name[length] = '\0';
  if (!is_nvidia_device_name(device_name))
    return -EINVAL;

  if (snprintf(device_path, sizeof(device_path), "/dev/%s", device_name) >= (int)sizeof(device_path))
    return -ENAMETOOLONG;
  device_fd = open(device_path, O_RDWR | O_CLOEXEC);
  if (device_fd < 0) {
    log_error("open(%s) failed: %s\n", device_path, strerror(errno));
    return -errno;
  }

  log_info("reopened %s for CRIU file id %#x\n", device_path, id);
  return device_fd;
}

cr_plugin_desc_t CR_PLUGIN_DESC = {
    .name = "snapshot_nvidia_residual_devices",
    .init = snapshot_nvidia_init,
    .exit = cr_plugin_dummy_exit,
    .version = CRIU_PLUGIN_VERSION,
    .max_hooks = CR_PLUGIN_HOOK__MAX,
    .hooks =
        {
            [CR_PLUGIN_HOOK__DUMP_EXT_FILE] = snapshot_dump_nvidia_fd,
            [CR_PLUGIN_HOOK__RESTORE_EXT_FILE] = snapshot_restore_nvidia_fd,
        },
};
