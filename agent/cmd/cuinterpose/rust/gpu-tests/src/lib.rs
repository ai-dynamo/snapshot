// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Harness for testing one packaged cuinterpose build against the installed CUDA driver.
//!
//! Every test needs two peer-capable GPUs and a CUDA 13.1+ driver. Missing hardware,
//! drivers, or artifacts fail the test instead of skipping it. Scenarios run in fresh
//! processes of the test binary with the frontend preloaded; see [`process`].

#![allow(
    clippy::macro_metavars_in_unsafe,
    reason = "CUDA entry points are unsafe, and cu! and tracked! keep scenarios close to the C API"
)]

pub mod core_abi;
pub mod cuda;
pub mod fixtures;
pub mod loader;
pub mod process;
pub mod shim;

pub use cudarc::driver::sys;
