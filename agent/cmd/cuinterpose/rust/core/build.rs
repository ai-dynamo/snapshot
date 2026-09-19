// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

fn main() {
    // Installation holds a process-wide mutex. Its libc calls must not enter
    // the loader through first-use PLT resolution.
    println!("cargo:rustc-link-arg=-Wl,-z,now");
}
