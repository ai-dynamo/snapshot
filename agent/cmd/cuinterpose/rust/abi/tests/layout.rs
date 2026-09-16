// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Check the generated C layout against the actual Rust layout, without CUDA.
use cuinterpose_abi::{Core, DebugStats, Host};
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
    layout!(Host, version, size, resolve, origin_pid);
    layout!(
        DebugStats,
        allocations,
        handles,
        mappings,
        multicasts,
        cached_exports,
        live_raw_imports,
        unsupported_exportable_creations,
        phase
    );
    layout!(
        Core,
        version,
        size,
        debug_stats,
        fork_prepare,
        fork_parent,
        fork_child,
        ensure_ready,
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
        cuMulticastGetGranularity,
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
