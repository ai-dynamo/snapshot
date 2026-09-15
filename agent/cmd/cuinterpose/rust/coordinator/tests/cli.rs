// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Exercise the shipped executable's CLI and phase reports over real UDS.
use cuinterpose_protocol::{Header, Operation, receive_header, send_header};
use std::os::unix::net::UnixListener;
use std::process::Command;

#[test]
fn empty_participant_capture_restore_contract() {
    let directory = std::env::temp_dir().join(format!("cui-cli-{}", std::process::id()));
    std::fs::create_dir(&directory).unwrap();
    let socket = directory.join("cuinterpose-1.sock");
    let listener = UnixListener::bind(&socket).unwrap();
    let mut identity = [0; 33];
    identity[..32].fill(b'a');
    let server = std::thread::spawn(move || {
        let mut operations = Vec::new();
        // Prepare: handshake, inspect, three destructive operations.
        // Restore: handshake, six reconstruction operations, handshake, inspect.
        for _ in 0..14 {
            let (stream, _) = listener.accept().unwrap();
            let (request, fd) = receive_header(&stream).unwrap();
            assert!(fd.is_none());
            operations.push(request.operation);
            send_header(&stream, &Header::new(request.operation, identity), None).unwrap();
        }
        operations
    });
    for (mode, phases) in [
        (
            "--prepare",
            &[
                "inspect",
                "validate",
                "prepare_multicast",
                "save_allocations",
                "prepare_unicast",
                "state_write",
            ][..],
        ),
        (
            "--restore",
            &[
                "handshake",
                "load_allocations",
                "restore_unicast",
                "restore_multicast",
                "validate",
            ][..],
        ),
    ] {
        let output = Command::new(env!("CARGO_BIN_EXE_cuinterpose-coordinator"))
            .args([mode, "--proc-root", "", "--checkpoint-dir"])
            .arg(&directory)
            .arg("--control-dir")
            .arg(&directory)
            .args(["--process", "1", "1"])
            .output()
            .unwrap();
        assert!(output.status.success(), "{:?}", output);
        let stdout = String::from_utf8(output.stdout).unwrap();
        let reports: Vec<serde_json::Value> = stdout
            .lines()
            .map(|line| serde_json::from_str(line).unwrap())
            .collect();
        assert_eq!(
            reports
                .iter()
                .map(|report| report["phase"].as_str().unwrap())
                .collect::<Vec<_>>(),
            phases,
            "{stdout}"
        );
        for report in reports {
            assert_eq!(report["status"], "ok");
            assert_eq!(report["participants"], 1);
            assert!(report["elapsed_ms"].is_number());
        }
    }
    assert!(directory.join("cuinterpose.state").is_file());
    assert_eq!(
        server.join().unwrap(),
        [
            Operation::Handshake,
            Operation::Inspect,
            Operation::PrepareMulticast,
            Operation::SaveAllocations,
            Operation::PrepareUnicast,
            Operation::Handshake,
            Operation::LoadAllocations,
            Operation::RestoreUnicast,
            Operation::RestoreMulticastCreators,
            Operation::RestoreMulticastImporters,
            Operation::RestoreMulticastDevices,
            Operation::RestoreMulticastBindings,
            Operation::Handshake,
            Operation::Inspect,
        ]
    );
    std::fs::remove_dir_all(directory).unwrap();
}
