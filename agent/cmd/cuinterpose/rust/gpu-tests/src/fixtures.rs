// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! C fixtures that must be separately linked objects. They compile with the system C
//! compiler into the test's work directory on first use, before the test starts the
//! processes that load them.

use crate::process::work_dir;
use std::env;
use std::fs;
use std::path::PathBuf;
use std::process::Command;

/// An executable linked directly against libcuda.
pub fn direct() -> PathBuf {
    build(
        "direct.c",
        include_str!("../fixtures/direct.c"),
        "direct",
        &["-l:libcuda.so.1"],
    )
}

/// A library whose constructor calls cuInit while the dynamic loader holds its lock.
pub fn cuinit_constructor() -> PathBuf {
    let source = include_str!("../fixtures/cuinit_constructor.c");
    build(
        "cuinit_constructor.c",
        source,
        "cuinit_constructor.so",
        &["-shared", "-fPIC"],
    )
}

/// A library preloaded after the frontend whose `fail_grant(n)` makes the shim's nth
/// following cuMemSetAccess grant fail after it applies.
pub fn fail_grant() -> PathBuf {
    let source = include_str!("../fixtures/fail_grant.c");
    build(
        "fail_grant.c",
        source,
        "fail_grant.so",
        &["-shared", "-fPIC", "-ldl"],
    )
}

/// A plugin that looks up symbols from its private dependency. Returns the plugin and
/// the dependency.
pub fn scope_plugin() -> (PathBuf, PathBuf) {
    let source = include_str!("../fixtures/scope.c");
    let dependency = build(
        "scope.c",
        source,
        "scope-dependency.so",
        &["-shared", "-fPIC", "-DSCOPE_DEPENDENCY"],
    );
    let directory = format!("-L{}", dependency.parent().unwrap().display());
    // A sibling call to dlsym would make glibc resolve the plugin's lookups in the
    // caller's scope.
    let flags = [
        "-shared",
        "-fPIC",
        "-fno-optimize-sibling-calls",
        &directory,
    ];
    let plugin = build(
        "scope.c",
        source,
        "scope.so",
        &[
            &flags[..],
            &["-l:scope-dependency.so", "-Wl,-rpath,$ORIGIN"],
        ]
        .concat(),
    );
    (plugin, dependency)
}

fn build(name: &str, source: &str, output: &str, flags: &[&str]) -> PathBuf {
    let directory = work_dir().join("fixtures");
    let path = directory.join(output);
    if path.exists() {
        return path;
    }
    fs::create_dir_all(&directory).unwrap();
    let source_path = directory.join(name);
    fs::write(&source_path, source).unwrap();
    let output = Command::new(env::var("CC").unwrap_or_else(|_| "cc".into()))
        .env_remove("LD_PRELOAD")
        .args(["-std=gnu11", "-O2", "-Wall", "-Wextra", "-Werror"])
        .arg(&source_path)
        .arg("-o")
        .arg(&path)
        .args(flags)
        .output()
        .expect("run the C compiler");
    assert!(
        output.status.success(),
        "cannot build {name}:\n{}",
        String::from_utf8_lossy(&output.stderr)
    );
    path
}
