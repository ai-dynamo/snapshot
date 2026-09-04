/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_PROTOCOL_H
#define CUINTERPOSE_PROTOCOL_H

#include <stdint.h>

/* The headers are shared with C++ tests. */
#ifdef __cplusplus
#define CUINTERPOSE_STATIC_ASSERT static_assert
#else
#define CUINTERPOSE_STATIC_ASSERT _Static_assert
#endif

/*
 * Wire contract between the shim's control socket and the coordinator, and
 * the fixed names the Go agent relies on. The Go side pins these strings with a
 * test (agent/internal/cuda), so renaming one here breaks that test rather
 * than silently disabling interposition.
 */

/* Directory (inside the workload container) holding per-process control sockets. */
#define CUINTERPOSE_CONTROL_DIR "/snapshot-control"
/* Environment variable overriding CUINTERPOSE_CONTROL_DIR; must be absolute. */
#define CUINTERPOSE_CONTROL_DIR_ENV "SNAPSHOT_CONTROL_DIR"
/* Socket name: <prefix><namespace-pid>.sock */
#define CUINTERPOSE_SOCKET_PREFIX "cuinterpose-"
/* Topology sidecar the coordinator writes into the checkpoint directory. */
#define CUINTERPOSE_STATE_FILENAME "cuinterpose.state"
/* Optional stable participant identity: 32 lowercase hex characters. */
#define CUINTERPOSE_PARTICIPANT_ID_ENV "SNAPSHOT_PARTICIPANT_ID"
/* Whole-second send/receive timeout for every control socket, both sides. */
#define CUINTERPOSE_CONTROL_TIMEOUT_ENV "SNAPSHOT_CONTROL_TIMEOUT_SECONDS"
#define CUINTERPOSE_CONTROL_TIMEOUT_SECONDS_DEFAULT 10U
/* Longer timeout for the two operations that copy allocation contents between
 * device and host; they scale with the amount of tracked memory. */
#define CUINTERPOSE_CARRIER_TIMEOUT_ENV "SNAPSHOT_CARRIER_TIMEOUT_SECONDS"
#define CUINTERPOSE_CARRIER_TIMEOUT_SECONDS_DEFAULT 3600U
/* First line of the state file; bump when the record layout changes. */
#define CUINTERPOSE_STATE_HEADER "cuinterpose-state-v2"

#define CUINTERPOSE_MAGIC 0x44564d4dU
#define CUINTERPOSE_VERSION 2U
#define CUINTERPOSE_ID_SIZE 33U
#define CUINTERPOSE_ALLOCATION_ID_SIZE 16U
#define CUINTERPOSE_TOKEN_SIZE 16U
/* Access descriptors kept per mapping. The shim merges access calls per
 * location, so this bounds the number of distinct GPUs (plus host NUMA nodes)
 * that may be granted access to one mapping. */
#define CUINTERPOSE_MAX_ACCESS 32U
#define CUINTERPOSE_MAX_RECORDS 4096U
#define CUINTERPOSE_POSIX_HANDLE_TYPE 1U

enum cuinterpose_operation {
  CUINTERPOSE_IDENTIFY = 1,
  CUINTERPOSE_INSPECT = 2,
  CUINTERPOSE_PREPARE_MULTICAST = 3,
  CUINTERPOSE_PREPARE = 4,
  CUINTERPOSE_RESTORE_CREATORS = 5,
  CUINTERPOSE_RESTORE_IMPORTERS = 6,
  CUINTERPOSE_EXPORT = 7,
  CUINTERPOSE_RESTORE_MULTICAST_CREATORS = 8,
  CUINTERPOSE_RESTORE_MULTICAST_IMPORTERS = 9,
  CUINTERPOSE_RESTORE_MULTICAST_DEVICES = 10,
  CUINTERPOSE_RESTORE_MULTICAST = 11,
  CUINTERPOSE_SAVE_HOST_CARRIER = 12,
  CUINTERPOSE_RESTORE_HOST_CARRIER = 13,
};

enum cuinterpose_record_kind {
  CUINTERPOSE_ALLOCATION = 1,
  CUINTERPOSE_MAPPING = 2,
  CUINTERPOSE_MULTICAST = 3,
  CUINTERPOSE_MULTICAST_DEVICE = 4,
  CUINTERPOSE_MULTICAST_BINDING = 5,
  CUINTERPOSE_MULTICAST_MAPPING = 6,
};

enum cuinterpose_record_flags {
  CUINTERPOSE_CREATOR = 1U << 0,
  CUINTERPOSE_APPLICATION_HANDLE_LIVE = 1U << 1,
  CUINTERPOSE_HOST_CARRIER = 1U << 2,
  /* A creator allocation that was never exported. It is reported only so its
   * contents can travel through the host carrier; no importer refers to it. */
  CUINTERPOSE_CARRIER_ONLY = 1U << 3,
};

/* Where a participant is in the checkpoint/restore lifecycle; reported in
 * IDENTIFY replies for diagnostics. */
enum cuinterpose_phase {
  CUINTERPOSE_PHASE_ACTIVE = 1,
  CUINTERPOSE_PHASE_PREPARING = 2,
  CUINTERPOSE_PHASE_PREPARED = 3,
  CUINTERPOSE_PHASE_RESTORING = 4,
  CUINTERPOSE_PHASE_FAILED = 5,
};

enum cuinterpose_resource_kind {
  CUINTERPOSE_RESOURCE_UNICAST = 1,
  CUINTERPOSE_RESOURCE_MULTICAST = 2,
};

enum cuinterpose_multicast_binding_kind {
  CUINTERPOSE_MULTICAST_BIND_MEM = 1,
  CUINTERPOSE_MULTICAST_BIND_ADDR = 2,
};

/*
 * Every control exchange is one fixed-size header each way, optionally
 * followed by `count` records (`payload_size` bytes) and optionally carrying
 * one file descriptor as SCM_RIGHTS ancillary data.
 */
struct cuinterpose_header {
  uint32_t magic;
  uint16_t version;
  uint16_t operation;
  int32_t status; /* 0 on success, -1 with `message` on failure */
  uint32_t count;
  uint64_t payload_size;
  char participant_id[CUINTERPOSE_ID_SIZE];
  char message[96];
  uint8_t authorization[CUINTERPOSE_TOKEN_SIZE];
  uint8_t allocation_id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  uint32_t resource_kind;
  /* Set by the shim in IDENTIFY and INSPECT replies. */
  uint32_t live_raw_imports; /* untracked imports still held; prepare refuses if non-zero */
  uint32_t passthrough_creations; /* allocations created with non-POSIX handle types, informational */
  uint8_t phase; /* enum cuinterpose_phase, 0 when unknown */
  uint8_t reserved_align[3];
  uint32_t copy_us; /* carrier replies: how long the copies themselves took, in microseconds */
  uint8_t reserved[48];
};

struct cuinterpose_access {
  int32_t location_type;
  int32_t location_id;
  uint64_t flags;
};

struct cuinterpose_record {
  uint32_t kind;
  uint32_t flags;
  uint8_t allocation_id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  uint64_t address;
  uint64_t size;
  uint64_t offset;
  uint64_t allocation_size;
  int32_t allocation_type;
  uint32_t requested_handle_types;
  int32_t allocation_location_type;
  int32_t allocation_location_id;
  uint32_t access_count;
  uint32_t application_handle_count;
  struct cuinterpose_access access[CUINTERPOSE_MAX_ACCESS];
  uint8_t member_id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  char creator_participant[CUINTERPOSE_ID_SIZE];
  uint8_t binding_kind;
  uint8_t api_version;
  uint8_t reserved[5];
  uint64_t member_offset;
  uint64_t operation_flags;
  uint64_t handle_types;
  uint64_t object_flags;
  uint32_t num_devices;
  int32_t device;
};

CUINTERPOSE_STATIC_ASSERT(sizeof(struct cuinterpose_header) == 256, "cuinterpose header layout changed");
CUINTERPOSE_STATIC_ASSERT(sizeof(struct cuinterpose_access) == 16, "cuinterpose access layout changed");
CUINTERPOSE_STATIC_ASSERT(sizeof(struct cuinterpose_record) == 688, "cuinterpose record layout changed");
CUINTERPOSE_STATIC_ASSERT(
    CUINTERPOSE_ALLOCATION < CUINTERPOSE_MULTICAST_BINDING, "unicast members must sort before multicast bindings");
CUINTERPOSE_STATIC_ASSERT(
    CUINTERPOSE_MULTICAST < CUINTERPOSE_MULTICAST_DEVICE, "multicast objects must sort before their devices");
CUINTERPOSE_STATIC_ASSERT(
    CUINTERPOSE_MULTICAST_DEVICE < CUINTERPOSE_MULTICAST_BINDING,
    "multicast devices must sort before their bindings");
CUINTERPOSE_STATIC_ASSERT(
    CUINTERPOSE_MULTICAST < CUINTERPOSE_MULTICAST_MAPPING, "multicast objects must sort before their mappings");

#endif
