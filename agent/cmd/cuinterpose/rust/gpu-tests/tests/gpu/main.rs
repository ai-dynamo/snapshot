// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Real-driver tests of the cuinterpose frontend, core, and coordinator. Each test runs
//! its scenarios in fresh processes, as `cuinterpose_gpu_tests::process` describes.

mod access_replay;
mod allocation_tracking;
mod context_teardown;
mod control_replies;
mod control_socket;
mod core_registration;
mod creator_released_backing;
mod foreign_imports;
mod host_numa_backing;
mod legacy_ipc;
mod malloc_peer_access;
mod multicast_bind_ordering;
mod multicast_restore;
mod multicast_sharing;
mod symbol_resolution;
