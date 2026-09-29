/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cuda.h>

#ifdef __cplusplus
extern "C" {
#endif

/*
 * CUDA driver calls shared by the CLI and persistent GPU engine. This module
 * owns no process, storage, transfer buffers, or environment configuration.
 * Passing NULL arguments selects ordinary driver-managed checkpoint storage;
 * callers using optional driver features provide their own initialized args.
 * Keep this interface usable with the ordinary helper's CUDA 13.0 headers.
 */
CUresult snapshot_cuda_lock(int pid, unsigned int timeout_ms);
CUresult snapshot_cuda_checkpoint(int pid, CUcheckpointCheckpointArgs* args);
CUresult snapshot_cuda_restore(int pid, CUcheckpointRestoreArgs* args);
CUresult snapshot_cuda_unlock(int pid);
CUresult snapshot_cuda_get_state(int pid, CUprocessState* state_out);
CUresult snapshot_cuda_get_restore_thread_id(int pid, int* tid_out);

#ifdef __cplusplus
}
#endif
