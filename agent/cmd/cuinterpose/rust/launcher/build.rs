// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use sha2::{Digest, Sha256};
use std::env;
use std::error::Error;
use std::fmt::Write;
use std::fs;
use std::path::Path;

fn main() -> Result<(), Box<dyn Error>> {
    let manifest =
        env::var_os("CARGO_MANIFEST_DIR").ok_or("Cargo did not set CARGO_MANIFEST_DIR")?;
    let build = Path::new(&manifest).join("../../build");
    let mut constants = String::new();
    for (library, name) in [
        ("libcuinterpose.so", "FRONTEND_SHA256"),
        ("libcuinterpose_core.so", "CORE_SHA256"),
    ] {
        let path = build.join(library);
        println!("cargo::rerun-if-changed={}", path.display());
        let bytes = fs::read(&path).map_err(|error| {
            format!(
                "read matched library {}: {error}; run make native from agent/cmd/cuinterpose before building the launcher",
                path.display()
            )
        })?;
        let digest: [u8; 32] = Sha256::digest(bytes).into();
        writeln!(constants, "const {name}: [u8; 32] = {digest:?};")?;
    }
    let output = env::var_os("OUT_DIR").ok_or("Cargo did not set OUT_DIR")?;
    fs::write(Path::new(&output).join("library_hashes.rs"), constants)?;
    Ok(())
}
