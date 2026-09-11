/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_UTIL_ARRAY_H
#define CUINTERPOSE_UTIL_ARRAY_H

#include <stddef.h>
#include <stdlib.h>

/* Grows a plain array by one element and returns it; NULL on allocation
 * failure. reallocarray rejects the size overflow internally. The array owns
 * no constructor: the caller initializes the returned element. */
static inline void*
array_push(void** array, size_t* count, size_t element_size)
{
  void* grown = reallocarray(*array, *count + 1, element_size);

  if (grown == NULL)
    return NULL;
  *array = grown;
  (*count)++;
  return (char*)grown + (*count - 1) * element_size;
}

#endif
