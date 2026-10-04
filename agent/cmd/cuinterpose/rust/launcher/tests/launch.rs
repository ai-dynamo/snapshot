// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use std::ffi::OsStr;
use std::fs;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::process::{Command, Stdio};
use tempfile::TempDir;

fn bundle() -> TempDir {
    let directory = tempfile::tempdir().unwrap();
    let build = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../build");
    for library in ["libcuinterpose.so", "libcuinterpose_core.so"] {
        fs::copy(build.join(library), directory.path().join(library))
            .expect("build the real frontend and core before testing the launcher");
    }
    directory
}

fn launcher(library: &Path) -> Command {
    let mut command = Command::new(env!("CARGO_BIN_EXE_cuinterpose-launch"));
    command.env_remove("LD_PRELOAD");
    command.arg("--library").arg(library).arg("--");
    command
}

#[test]
fn preserves_resolved_environment_arguments_and_pid() {
    let bundle = bundle();
    let library = bundle.path().join("libcuinterpose.so");
    for existing in [
        None,
        Some(&b""[..]),
        Some(b"a.so"),
        Some(b"a.so b.so:c.so"),
        Some(b"a-\xff.so"),
    ] {
        let arguments = [&b""[..], b"two words", b"$(exit 9)", b"arg-\xfe"];
        let mut command = launcher(&library);
        if let Some(value) = existing {
            command.env("LD_PRELOAD", OsStr::from_bytes(value));
        }
        // The supplied shell reports its raw environment bytes and PID so an unintended
        // parent process cannot make the test pass.
        let child = command
            .args([
                "/bin/sh",
                "-c",
                r#"grep -F -- "$TEST_LIBRARY" /proc/$$/maps >/dev/null || exit 92
printf '%s\0' "$LD_PRELOAD" "$UNCHANGED" "$$" "$@""#,
                "workload",
            ])
            .args(arguments.map(OsStr::from_bytes))
            .env("UNCHANGED", OsStr::from_bytes(b"value-\xff"))
            .env("TEST_LIBRARY", &library)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("start launcher");
        let pid = child.id().to_string();
        let output = child.wait_with_output().expect("wait for workload");
        assert!(output.status.success(), "{:?}", output.stderr);
        let mut preload = library.as_os_str().as_bytes().to_vec();
        if let Some(existing) = existing.filter(|value| !value.is_empty()) {
            preload.push(b':');
            preload.extend_from_slice(existing);
        }
        let expected: Vec<_> = [preload.as_slice(), b"value-\xff", pid.as_bytes()]
            .into_iter()
            .chain(arguments)
            .flat_map(|value| value.iter().copied().chain([0]))
            .collect();
        assert_eq!(output.stdout, expected);
    }
}

#[test]
fn preserves_workload_exit_status() {
    let bundle = bundle();
    let output = launcher(&bundle.path().join("libcuinterpose.so"))
        .args(["/bin/sh", "-c", "exit 37"])
        .output()
        .unwrap();
    assert_eq!(output.status.code(), Some(37));
}

#[test]
fn reports_usage_and_exec_status() {
    let bundle = bundle();
    let library = bundle.path().join("libcuinterpose.so");
    let denied = bundle.path().join("not-executable");
    fs::write(&denied, "#!/bin/sh\nexit 0\n").unwrap();
    fs::set_permissions(&denied, fs::Permissions::from_mode(0o644)).unwrap();
    for (args, code, message) in [
        (vec![], 2, "usage:"),
        (
            vec!["/cuinterpose-test-command-does-not-exist"],
            127,
            "cannot execute",
        ),
        (vec!["/"], 126, "cannot execute"),
        (vec![denied.to_str().unwrap()], 126, "cannot execute"),
    ] {
        let output = launcher(&library).args(args).output().unwrap();
        assert_eq!(output.status.code(), Some(code));
        assert!(String::from_utf8_lossy(&output.stderr).contains(message));
    }
}

#[test]
fn missing_or_invalid_delivery_never_starts_the_workload() {
    for missing in ["libcuinterpose.so", "libcuinterpose_core.so"] {
        for directory in [false, true] {
            let bundle = bundle();
            let path = bundle.path().join(missing);
            fs::remove_file(&path).unwrap();
            if directory {
                fs::create_dir(&path).unwrap();
            }
            let marker = bundle.path().join("started");
            let output = launcher(&bundle.path().join("libcuinterpose.so"))
                .args(["/bin/sh", "-c", "touch \"$1\"", "workload"])
                .arg(&marker)
                .output()
                .unwrap();
            assert_eq!(output.status.code(), Some(1));
            assert!(String::from_utf8_lossy(&output.stderr).contains(missing));
            assert!(!marker.exists());
        }
    }
}

#[test]
fn requires_an_unambiguous_explicit_library_path() {
    for library in [
        "relative.so",
        "/tmp/two words/libcuinterpose.so",
        "/tmp/a:b.so",
    ] {
        let output = launcher(Path::new(library))
            .arg("/bin/true")
            .output()
            .unwrap();
        assert_eq!(output.status.code(), Some(2));
    }
    let output = Command::new(env!("CARGO_BIN_EXE_cuinterpose-launch"))
        .arg("/bin/true")
        .output()
        .unwrap();
    assert_eq!(output.status.code(), Some(2));
}
