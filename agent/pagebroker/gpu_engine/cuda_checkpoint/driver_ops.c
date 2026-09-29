/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "driver_ops.h"

#include <string.h>

CUresult
snapshot_cuda_lock(int pid, unsigned int timeout_ms)
{
  CUcheckpointLockArgs args;
  memset(&args, 0, sizeof(args));
  args.timeoutMs = timeout_ms;
  return cuCheckpointProcessLock(pid, &args);
}

CUresult
snapshot_cuda_checkpoint(int pid, CUcheckpointCheckpointArgs* args)
{
  CUcheckpointCheckpointArgs defaults;
  memset(&defaults, 0, sizeof(defaults));
  return cuCheckpointProcessCheckpoint(pid, args == NULL ? &defaults : args);
}

CUresult
snapshot_cuda_restore(int pid, CUcheckpointRestoreArgs* args)
{
  CUcheckpointRestoreArgs defaults;
  memset(&defaults, 0, sizeof(defaults));
  return cuCheckpointProcessRestore(pid, args == NULL ? &defaults : args);
}

CUresult
snapshot_cuda_unlock(int pid)
{
  CUcheckpointUnlockArgs args;
  memset(&args, 0, sizeof(args));
  return cuCheckpointProcessUnlock(pid, &args);
}

CUresult
snapshot_cuda_get_state(int pid, CUprocessState* state_out)
{
  return cuCheckpointProcessGetState(pid, state_out);
}

CUresult
snapshot_cuda_get_restore_thread_id(int pid, int* tid_out)
{
  return cuCheckpointProcessGetRestoreThreadId(pid, tid_out);
}
