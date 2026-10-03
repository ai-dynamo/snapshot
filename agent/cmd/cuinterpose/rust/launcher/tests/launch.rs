// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use std::ffi::OsStr;
use std::os::unix::ffi::OsStrExt;
use std::process::{Command, Stdio};

fn launcher() -> Command {
    let mut command = Command::new(env!("CARGO_BIN_EXE_cuinterpose-launch"));
    command.env_remove("LD_PRELOAD");
    command
}

#[test]
fn preserves_resolved_environment_arguments_and_pid() {
    for existing in [
        None,
        Some(&b""[..]),
        Some(b"a.so"),
        Some(b"a.so b.so:c.so"),
        Some(b"a-\xff.so"),
    ] {
        let arguments = [&b""[..], b"two words", b"$(exit 9)", b"arg-\xfe"];
        let mut command = launcher();
        if let Some(value) = existing {
            command.env("LD_PRELOAD", OsStr::from_bytes(value));
        }
        // The supplied shell reports its raw environment bytes and PID so an unintended
        // parent process cannot make the test pass.
        let child = command
            .args([
                "/bin/sh",
                "-c",
                r#"printf '%s\0' "$LD_PRELOAD" "$UNCHANGED" "$$" "$@""#,
                "workload",
            ])
            .args(arguments.map(OsStr::from_bytes))
            .env("UNCHANGED", OsStr::from_bytes(b"value-\xff"))
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("start launcher");
        let pid = child.id().to_string();
        let output = child.wait_with_output().expect("wait for workload");
        assert!(output.status.success(), "{:?}", output.stderr);
        let mut preload = b"/tmp/snapshot-cuda/libcuinterpose.so".to_vec();
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
    let output = launcher()
        .args(["/bin/sh", "-c", "exit 37"])
        .output()
        .unwrap();
    assert_eq!(output.status.code(), Some(37));
}

#[test]
fn reports_missing_command_and_exec_errors() {
    for (args, message) in [
        (vec![], "missing command"),
        (
            vec!["/cuinterpose-test-command-does-not-exist"],
            "cannot execute",
        ),
        (vec!["/"], "cannot execute"),
    ] {
        let output = launcher().args(args).output().unwrap();
        assert!(!output.status.success());
        assert!(String::from_utf8_lossy(&output.stderr).contains(message));
    }
}
