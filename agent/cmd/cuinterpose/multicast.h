/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_MULTICAST_H
#define CUINTERPOSE_MULTICAST_H

/*
 * CUDA multicast objects (cuMulticast*) let several GPUs write one buffer
 * through NVLink. This layer forwards the entry points, translating tracked
 * unicast handles for the bind calls; tracking and checkpoint of multicast
 * groups is added in a later change.
 */

#endif
