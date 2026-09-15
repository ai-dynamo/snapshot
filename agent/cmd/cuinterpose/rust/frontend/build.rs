// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

fn main() {
    // Wrapper-address comparisons must identify this DSO's implementation, not
    // an earlier preload's same-named function. Bind only references to functions
    // defined here; CUDA/libc imports and explicit RTLD_NEXT lookup stay dynamic.
    // Scope this to the frontend cdylib, not the core or coordinator.
    println!("cargo:rustc-cdylib-link-arg=-Wl,-Bsymbolic-functions");
}
