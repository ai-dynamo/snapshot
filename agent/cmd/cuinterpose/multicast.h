/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_MULTICAST_H
#define CUINTERPOSE_MULTICAST_H

/*
 * CUDA multicast objects (cuMulticast*) let several GPUs write one buffer
 * through NVLink: a team of devices is attached to an object, each rank binds
 * a slice of its own memory to it, and everyone maps the object into their
 * address space. NCCL's NVLS and PyTorch's symmetric memory build on this.
 *
 * This module tracks the objects created with the POSIX file-descriptor handle
 * type exactly as interpose.c tracks unicast allocations: the application
 * holds logical handles, one driver handle per object per process sits behind
 * them, sharing goes through tickets served from the export cache, and the
 * coordinator tears the objects down before the native checkpoint and rebuilds
 * them afterwards in four phases (create, import, add devices, bind and map)
 * with a barrier after each, because binding waits for the whole team to be
 * attached.
 *
 * Everything here runs under interpose.c's state_lock unless noted. The bind,
 * map, and add-device calls are team collectives that can block until the
 * other ranks arrive, so the lock is dropped around them and the object is
 * looked up again afterwards.
 */

#include <cuda.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "posix.h"
#include "protocol.h"

/* A tracked unicast allocation as seen from a multicast binding. */
struct cuinterpose_multicast_member {
  uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  CUmemGenericAllocationHandle handle; /* driver handle, for cuMulticastBindMem */
  CUdeviceptr address; /* for cuMulticastBindAddr */
  size_t allocation_offset;
  CUdevice device;
  bool temporary_handle; /* the caller must cuMemRelease `handle` when done */
};

/* What interpose.c provides: logical handle numbers, member lookups, the lock. */
struct cuinterpose_multicast_callbacks {
  int (*allocate_logical_handle)(CUmemGenericAllocationHandle* output);
  int (*member_from_handle)(CUmemGenericAllocationHandle logical, struct cuinterpose_multicast_member* member);
  int (*member_from_address)(CUdeviceptr address, size_t size, struct cuinterpose_multicast_member* member);
  int (*member_from_id)(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE], struct cuinterpose_multicast_member* member);
  void (*mark_member_shared)(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE]);
  void (*release_state_lock)(void);
  void (*acquire_state_lock)(void);
  bool (*state_is_active)(void); /* PHASE_ACTIVE; re-checked after every lock drop */
};

/* participant_id and endpoint point at interpose.c's buffers, filled in later. */
void cuinterpose_multicast_initialize(
    const struct cuinterpose_multicast_callbacks* callbacks, const char* participant_id, const char* endpoint);
/* The child of fork() owns none of the parent's objects. */
void cuinterpose_multicast_fork_child(void);
size_t cuinterpose_multicast_count(void);

/* Entry points interpose.c dispatches to when a handle or range is a multicast one. Lock held. */
bool cuinterpose_multicast_is_handle(CUmemGenericAllocationHandle logical);
bool cuinterpose_multicast_owns_range(CUdeviceptr address, size_t size);
CUresult cuinterpose_multicast_release(CUmemGenericAllocationHandle logical);
/* CUDA_ERROR_INVALID_VALUE when address is not inside a multicast mapping. */
CUresult cuinterpose_multicast_retain(CUmemGenericAllocationHandle* output, void* address);
/* Drops and reacquires the lock around the driver call. */
CUresult cuinterpose_multicast_map(
    CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle logical, unsigned long long flags);
CUresult cuinterpose_multicast_unmap(CUdeviceptr address, size_t size);
CUresult cuinterpose_multicast_set_access(
    CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count);
CUresult cuinterpose_multicast_export(
    void* shareable, CUmemGenericAllocationHandle logical, CUmemAllocationHandleType type, unsigned long long flags);
CUresult cuinterpose_multicast_get_properties(CUmemAllocationProp* properties, CUmemGenericAllocationHandle logical);
/* Lock NOT held: talks to the creator's listener and the driver first. */
CUresult cuinterpose_multicast_import(CUmemGenericAllocationHandle* output, const struct cuinterpose_posix_ticket* ticket);

/* INSPECT records for every live object: the object, its devices, its
 * bindings, and its mappings, in that order. Lock held. */
size_t cuinterpose_multicast_record_count(void);
int cuinterpose_multicast_write_records(struct cuinterpose_record* records, size_t count, const char** error);

/* Lifecycle, lock held. Errors are static strings (or written to message). */
int cuinterpose_multicast_prepare(const char** error);
int cuinterpose_multicast_restore_creators(const char** error);
int cuinterpose_multicast_restore_importers(char* message, size_t message_size);
int cuinterpose_multicast_restore_devices(const char** error);
int cuinterpose_multicast_restore_topology(const char** error);

#endif
