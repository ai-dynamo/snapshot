// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Distinguish cuinterpose virtual allocation handles from handles returned by CUDA.

pub const VIRTUAL_ALLOCATION_HANDLE_TAG: u64 = 0xd94d_0000_0000_0000;
pub const VIRTUAL_ALLOCATION_HANDLE_MASK: u64 = 0xffff_0000_0000_0000;
