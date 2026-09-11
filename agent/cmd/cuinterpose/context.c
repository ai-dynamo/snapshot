/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "context.h"

#include <string.h>

#include "symbols.h"

typedef CUresult(CUDAAPI* context_get_fn)(CUcontext*);
typedef CUresult(CUDAAPI* context_set_fn)(CUcontext);
typedef CUresult(CUDAAPI* primary_retain_fn)(CUcontext*, CUdevice);
typedef CUresult(CUDAAPI* primary_release_fn)(CUdevice);

void
cuinterpose_capture_context(CUcontext* context)
{
  context_get_fn get_current = (context_get_fn)cuinterpose_lookup_real_symbol("cuCtxGetCurrent");
  CUcontext current = NULL;

  *context = get_current != NULL && get_current(&current) == CUDA_SUCCESS ? current : NULL;
}

static void
release_primary(struct cuinterpose_context_scope* scope)
{
  primary_release_fn release = (primary_release_fn)cuinterpose_lookup_real_symbol("cuDevicePrimaryCtxRelease_v2");

  if (scope->retained_primary && release != NULL)
    (void)release(scope->primary_device);
  scope->retained_primary = false;
}

int
cuinterpose_enter_context(CUcontext context, CUdevice fallback_device, struct cuinterpose_context_scope* scope)
{
  context_get_fn get_current = (context_get_fn)cuinterpose_lookup_real_symbol("cuCtxGetCurrent");
  context_set_fn set_current = (context_set_fn)cuinterpose_lookup_real_symbol("cuCtxSetCurrent");

  memset(scope, 0, sizeof(*scope));
  if (get_current == NULL || set_current == NULL)
    return -1;
  if (context == NULL) {
    primary_retain_fn retain = (primary_retain_fn)cuinterpose_lookup_real_symbol("cuDevicePrimaryCtxRetain");

    if (retain == NULL || fallback_device == CUINTERPOSE_NO_DEVICE || retain(&context, fallback_device) != CUDA_SUCCESS ||
        context == NULL)
      return -1;
    scope->retained_primary = true;
    scope->primary_device = fallback_device;
  }
  if (get_current(&scope->previous) != CUDA_SUCCESS) {
    release_primary(scope);
    return -1;
  }
  if (scope->previous != context) {
    if (set_current(context) != CUDA_SUCCESS) {
      release_primary(scope);
      return -1;
    }
    scope->changed = true;
  }
  return 0;
}

int
cuinterpose_leave_context(struct cuinterpose_context_scope* scope)
{
  context_set_fn set_current = (context_set_fn)cuinterpose_lookup_real_symbol("cuCtxSetCurrent");
  int result = !scope->changed || (set_current != NULL && set_current(scope->previous) == CUDA_SUCCESS) ? 0 : -1;

  release_primary(scope);
  return result;
}
