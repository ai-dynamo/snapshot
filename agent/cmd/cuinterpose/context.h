/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_CONTEXT_H
#define CUINTERPOSE_CONTEXT_H

#include <cuda.h>
#include <stdbool.h>

/*
 * Driver calls that act on an allocation or a multicast object must run in the
 * CUDA context it belongs to. The shim remembers that context when it can
 * (create, import, first map or export) and switches to it for the duration
 * of a scope during checkpoint and restore, switching back afterwards.
 */

#define CUINTERPOSE_NO_DEVICE ((CUdevice)-1)

struct cuinterpose_context_scope {
  CUcontext previous;
  bool changed;
  bool retained_primary; /* the device's primary context was retained for this scope */
  CUdevice primary_device;
};

/* Remembers the current context, or NULL when there is none. */
void cuinterpose_capture_context(CUcontext* context);

/*
 * Makes `context` current. When it is NULL (the object never had a context)
 * and fallback_device is a real device, that device's primary context is
 * retained for the scope and released on leave. Returns -1 on failure.
 */
int cuinterpose_enter_context(
    CUcontext context, CUdevice fallback_device, struct cuinterpose_context_scope* scope);
int cuinterpose_leave_context(struct cuinterpose_context_scope* scope);

#endif
