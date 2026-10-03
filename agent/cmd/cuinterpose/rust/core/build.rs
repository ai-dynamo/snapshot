// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

fn main() {
    // Resolve libc PLT entries before installation so first use cannot enter the loader
    // while the process mutex is held.
    println!("cargo:rustc-link-arg=-Wl,-z,now");
}
