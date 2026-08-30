/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSER_MULTICAST_H
#define CUINTERPOSER_MULTICAST_H

#include <cuda.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "posix.h"
#include "protocol.h"

struct cuinterposer_multicast_member {
  uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE];
  CUmemGenericAllocationHandle handle;
  CUdeviceptr address;
  size_t allocation_offset;
  CUdevice device;
  bool temporary_handle;
};

struct cuinterposer_multicast_callbacks {
  int (*allocate_logical_handle)(CUmemGenericAllocationHandle* output);
  int (*member_from_handle)(CUmemGenericAllocationHandle logical, struct cuinterposer_multicast_member* member);
  int (*member_from_address)(CUdeviceptr address, size_t size, struct cuinterposer_multicast_member* member);
  int (*member_from_id)(const uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE], struct cuinterposer_multicast_member* member);
  void (*mark_member_shared)(const uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE]);
  void (*release_state_lock)(void);
  void (*acquire_state_lock)(void);
  /*
   * True while the interposer is in PHASE_ACTIVE. Callers that drop
   * state_lock across a long driver call must revalidate with this on
   * reacquire: a checkpoint may have started in the window.
   */
  bool (*state_is_active)(void);
};

void cuinterposer_multicast_initialize(
    const struct cuinterposer_multicast_callbacks* callbacks, const char* participant_id, const char* endpoint);
void cuinterposer_multicast_reset(void);

bool cuinterposer_multicast_is_handle(CUmemGenericAllocationHandle logical);
bool cuinterposer_multicast_has_mapping(CUdeviceptr address, size_t size);
CUresult cuinterposer_multicast_release(CUmemGenericAllocationHandle logical);
CUresult cuinterposer_multicast_retain(CUmemGenericAllocationHandle* output, void* address);
CUresult cuinterposer_multicast_map(
    CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle logical, unsigned long long flags);
CUresult cuinterposer_multicast_unmap(CUdeviceptr address, size_t size);
CUresult cuinterposer_multicast_set_access(
    CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count);
CUresult cuinterposer_multicast_export(
    void* shareable, CUmemGenericAllocationHandle logical, CUmemAllocationHandleType type, unsigned long long flags);
CUresult cuinterposer_multicast_import(
    CUmemGenericAllocationHandle* output, const struct cuinterposer_posix_ticket* ticket, int raw_fd);
CUresult cuinterposer_multicast_get_properties(CUmemAllocationProp* properties, CUmemGenericAllocationHandle logical);

CUresult cuinterposer_multicast_create(CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties);
CUresult cuinterposer_multicast_add_device(CUmemGenericAllocationHandle logical, CUdevice device);
CUresult cuinterposer_multicast_bind_mem(
    CUmemGenericAllocationHandle multicast, CUdevice device, bool device_explicit, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size, unsigned long long flags);
CUresult cuinterposer_multicast_bind_address(
    CUmemGenericAllocationHandle multicast, CUdevice device, bool device_explicit, size_t multicast_offset,
    CUdeviceptr memory, size_t size, unsigned long long flags);
CUresult cuinterposer_multicast_unbind(CUmemGenericAllocationHandle multicast, CUdevice device, size_t offset, size_t size);

size_t cuinterposer_multicast_record_count(void);
int cuinterposer_multicast_write_records(struct cuinterposer_record* records, size_t count);
int cuinterposer_multicast_prepare(void);
int cuinterposer_multicast_restore_creators(void);
int cuinterposer_multicast_restore_importers(void);
int cuinterposer_multicast_restore_devices(void);
int cuinterposer_multicast_restore_topology(void);
int cuinterposer_multicast_validate_restored(void);
CUresult cuinterposer_multicast_export_raw(
    const uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE], const uint8_t authorization[CUINTERPOSER_TOKEN_SIZE],
    int* output);
const char* cuinterposer_multicast_error(void);

#endif
