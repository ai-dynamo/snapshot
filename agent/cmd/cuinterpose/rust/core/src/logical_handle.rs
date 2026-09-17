// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Distinguish cuinterpose logical handles from handles returned by CUDA.

pub const LOGICAL_HANDLE_TAG: u64 = 0xd94d_0000_0000_0000;
pub const LOGICAL_HANDLE_MASK: u64 = 0xffff_0000_0000_0000;
