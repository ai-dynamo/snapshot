// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Real-driver tests of the cuinterpose frontend, core, and coordinator. Each test runs
//! its scenarios in fresh processes, as `cuinterpose_gpu_tests::process` describes.

mod control_replies;
mod control_socket;
mod core_registration;
mod symbol_resolution;
