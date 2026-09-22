// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Check the generated C layout against the actual Rust layout.
use cuinterpose_abi::{BackendAbi, FrontendAbi};
use std::fmt::Write as _;
use std::io::Write as _;
use std::process::{Command, Stdio};

#[test]
fn generated_c_layout_matches_rust() {
    let directory = tempfile::tempdir().expect("temporary header directory");
    cbindgen::Builder::new()
        .with_crate(env!("CARGO_MANIFEST_DIR"))
        .with_config(
            cbindgen::Config::from_file(concat!(env!("CARGO_MANIFEST_DIR"), "/cbindgen.toml"))
                .expect("read cbindgen configuration"),
        )
        .generate()
        .expect("generate the private C ABI")
        .write_to_file(directory.path().join("core_abi.h"));
    let mut source = String::from("#include \"core_abi.h\"\n");
    macro_rules! layout {
        ($ty:ident, $($field:ident),+ $(,)?) => {
            writeln!(source, "_Static_assert(sizeof(struct {}) == {}, \"size\");",
                stringify!($ty), size_of::<$ty>()).expect("write to string");
            writeln!(source, "_Static_assert(_Alignof(struct {}) == {}, \"alignment\");",
                stringify!($ty), align_of::<$ty>()).expect("write to string");
            $(writeln!(source, "_Static_assert(offsetof(struct {}, {}) == {}, \"{}.{}\");",
                stringify!($ty), stringify!($field), std::mem::offset_of!($ty, $field),
                stringify!($ty), stringify!($field)).expect("write to string");)+
        };
    }
    layout!(FrontendAbi, version, size, resolve);
    layout!(
        BackendAbi,
        version,
        size,
        ensure_cuinterpose_initialized,
        cuMemAlloc_v2,
        cuMemFree_v2,
        cuMemGetAddressRange_v2,
        cuIpcGetMemHandle,
        cuIpcOpenMemHandle,
        cuIpcOpenMemHandle_v2,
        cuIpcCloseMemHandle,
        cuMemCreate,
        cuMemRelease,
        cuMemRetainAllocationHandle,
        cuMemMap,
        cuMemUnmap,
        cuMemSetAccess,
        cuMemExportToShareableHandle,
        cuMemImportFromShareableHandle,
        cuMemGetAllocationPropertiesFromHandle,
        cuMulticastCreate,
        cuMulticastAddDevice,
        cuMulticastBindMem,
        cuMulticastBindMem_v2,
        cuMulticastBindAddr,
        cuMulticastBindAddr_v2,
        cuMulticastUnbind
    );
    let mut compiler = Command::new("/usr/bin/gcc")
        .args([
            "-std=gnu11",
            "-Wall",
            "-Wextra",
            "-Werror",
            "-fsyntax-only",
            "-x",
            "c",
            "-",
        ])
        .arg("-I")
        .arg(directory.path())
        .arg("-I")
        .arg("/opt/cuda/include")
        .stdin(Stdio::piped())
        .spawn()
        .expect("start C compiler");
    compiler
        .stdin
        .take()
        .expect("compiler stdin")
        .write_all(source.as_bytes())
        .expect("write ABI assertions");
    assert!(compiler.wait().expect("wait for C compiler").success());
}
