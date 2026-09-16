// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Build-time header generation; cbindgen is not linked into the shim.
fn main() {
    let directory = env!("CARGO_MANIFEST_DIR");
    let config = cbindgen::Config::from_file(concat!(env!("CARGO_MANIFEST_DIR"), "/cbindgen.toml"))
        .expect("read cbindgen configuration");
    cbindgen::Builder::new()
        .with_crate(directory)
        .with_config(config)
        .generate()
        .expect("generate the private C ABI")
        .write_to_file("../build/core_abi.h");
}
