// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

fn main() {
    // Installation holds a process mutex. Resolve libc PLT entries before installation
    // to avoid entering the loader while holding that mutex.
    println!("cargo:rustc-link-arg=-Wl,-z,now");
}
