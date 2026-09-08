// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#ifndef CRIU_PROVIDER_H
#define CRIU_PROVIDER_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct criu_provider_plan criu_provider_plan;
typedef struct criu_provider_session criu_provider_session;
typedef struct criu_provider_dump_session criu_provider_dump_session;

enum criu_provider_restore_mode {
	CRIU_PROVIDER_RESTORE_LOCAL,
	CRIU_PROVIDER_RESTORE_READY_LOCAL,
	CRIU_PROVIDER_RESTORE_PROVIDER_FD,
	CRIU_PROVIDER_RESTORE_LOCAL_FALLBACK,
};

enum criu_provider_dump_mode {
	CRIU_PROVIDER_DUMP_LOCAL,
	CRIU_PROVIDER_DUMP_PROVIDER_OUTPUT,
	CRIU_PROVIDER_DUMP_LOCAL_FALLBACK,
};

struct criu_provider_requirements {
	uint64_t stored_bytes;
	uint64_t materialized_bytes;
	uint64_t logical_fd_bytes;
	uint64_t metadata_bytes;
	uint64_t workspace_bytes;
	uint32_t fd_count;
};

typedef int (*criu_provider_source_range_callback)(void *context,
		const char *logical_image, uint64_t source_offset, uint64_t length);

enum criu_provider_image_role {
	CRIU_PROVIDER_IMAGE_BOOTSTRAP_LOCAL,
	CRIU_PROVIDER_IMAGE_METADATA,
	CRIU_PROVIDER_IMAGE_PAGES,
};

typedef int (*criu_provider_image_callback)(void *context,
		const char *logical_image, uint64_t size,
		enum criu_provider_image_role role);

struct criu_provider_source_ops {
	int (*read_range)(void *context, const char *logical_image,
		uint64_t source_offset, void *buffer, size_t length);
	int (*open_ready_image)(void *context, const char *logical_image,
		int open_flags);
};

int criu_provider_plan_from_checkpoint(const char *checkpoint_dir,
		criu_provider_plan **out);
int criu_provider_plan_write(const criu_provider_plan *plan,
		const char *plan_path);
int criu_provider_plan_load(const char *plan_path, criu_provider_plan **out);
void criu_provider_plan_destroy(criu_provider_plan *plan);
int criu_provider_plan_requirements(const criu_provider_plan *plan,
		struct criu_provider_requirements *out);
int criu_provider_plan_enumerate_source_ranges(const criu_provider_plan *plan,
		criu_provider_source_range_callback callback, void *context);
int criu_provider_plan_enumerate_images(const criu_provider_plan *plan,
		criu_provider_image_callback callback, void *context);

int criu_provider_dump_plan_create(uint64_t page_size, criu_provider_plan **out);
int criu_provider_dump_plan_add_image(criu_provider_plan *plan,
		const char *logical_image, enum criu_provider_restore_mode restore_mode,
		enum criu_provider_dump_mode dump_mode);

int criu_provider_session_create(const criu_provider_plan *plan,
		const struct criu_provider_source_ops *source_ops, void *source_context,
		criu_provider_session **out);
int criu_provider_session_prepare(criu_provider_session *session);
int criu_provider_session_serve(criu_provider_session *session,
		int extmem_seqpacket_fd);
void criu_provider_session_destroy(criu_provider_session *session);

struct criu_provider_dump_ops {
	int (*open_output_image)(void *context, const char *logical_image,
		int open_flags);
	int (*commit)(void *context, const criu_provider_plan *plan);
	void (*abort)(void *context);
};

int criu_provider_dump_session_create(const criu_provider_plan *plan,
		const struct criu_provider_dump_ops *ops, void *context,
		criu_provider_dump_session **out);
int criu_provider_dump_session_serve(criu_provider_dump_session *session,
		int extmem_seqpacket_fd);
void criu_provider_dump_session_destroy(criu_provider_dump_session *session);

#ifdef __cplusplus
}
#endif

#endif
