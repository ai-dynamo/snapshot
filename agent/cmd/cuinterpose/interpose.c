/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * CUDA entry-point interposition is added by the forwarding layer.
 * Keep this translation unit so packaging and later behavior use one layout.
 */
static const char cuinterposer_skeleton[] __attribute__((used)) = "cuinterposer";
