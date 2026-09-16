// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cuinterpose_protocol::{Participant, ParticipantId, encode};
use std::process::Command;

#[test]
fn captured_inventory_reads_state_without_connecting_to_workload() {
    let directory = tempfile::tempdir().expect("temporary checkpoint directory");
    let identity = ParticipantId([7; 16]);
    std::fs::write(
        directory.path().join("cuinterpose.state"),
        encode(&vec![Participant {
            id: identity,
            records: Vec::new(),
        }])
        .expect("encode captured state"),
    )
    .expect("write captured state");
    let output = Command::new(env!("CARGO_BIN_EXE_cuinterpose-coordinator"))
        .args([
            "--state-participants",
            "--proc-root",
            "",
            "--control-dir",
            "/missing",
        ])
        .arg("--checkpoint-dir")
        .arg(directory.path())
        .args(["--process", "42", "42"])
        .output()
        .expect("run coordinator inventory");
    assert!(output.status.success(), "{output:?}");
    let ids: Vec<String> =
        serde_json::from_slice(&output.stdout).expect("JSON participant inventory");
    assert_eq!(ids, [identity.to_string()]);
}
