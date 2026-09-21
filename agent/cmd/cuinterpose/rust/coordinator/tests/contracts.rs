// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, BindingVersion, Manifest, MemberRange,
    Operation, Record, Reply, Request, Response, decode, receive, send,
};
use std::{
    os::unix::net::UnixListener,
    path::PathBuf,
    process::{Command, Output},
    sync::{
        Condvar, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    thread,
    time::Duration,
};

const ID: AllocationId = [1; 16];
const GROUP: AllocationId = [2; 16];
const ALLOCATION: AllocationReference = AllocationReference {
    id: ID,
    creator_pid: 1,
};
const MULTICAST: AllocationReference = AllocationReference {
    id: GROUP,
    creator_pid: 1,
};

#[derive(Default)]
struct Model {
    records: Vec<Record>,
    fail: Option<Operation>,
    disconnect: Option<Operation>,
    operations: Vec<String>,
    namespace_pid: u32,
}

struct Fixture {
    directory: PathBuf,
    _temporary: tempfile::TempDir,
    models: Vec<Model>,
    barrier: Option<Operation>,
}

impl Fixture {
    fn new(count: usize, barrier: Option<Operation>) -> Self {
        let temporary = tempfile::tempdir().unwrap();
        Self {
            directory: temporary.path().to_path_buf(),
            _temporary: temporary,
            models: (1..=count)
                .map(|pid| Model {
                    namespace_pid: pid as u32,
                    ..Model::default()
                })
                .collect(),
            barrier,
        }
    }

    fn run(&mut self, mode: &str) -> Output {
        let mut command = Command::new(env!("CARGO_BIN_EXE_cuinterpose-coordinator"));
        command
            .args([mode, "--checkpoint-dir"])
            .arg(&self.directory)
            .arg("--control-dir")
            .arg(&self.directory)
            .env("SNAPSHOT_CONTROL_TIMEOUT_SECONDS", "2");
        let stop = AtomicBool::new(false);
        let gate = (Mutex::new(0), Condvar::new());
        let released = AtomicBool::new(false);
        let count = self.models.len();
        let barrier = self.barrier;
        thread::scope(|scope| {
            for (index, model) in self.models.iter_mut().enumerate() {
                let pid = index as u32 + 1;
                command.args(["--process", &pid.to_string()]);
                let path = self.directory.join(format!("cuinterpose-{pid}.sock"));
                if path.exists() {
                    std::fs::remove_file(&path).unwrap();
                }
                let listener = UnixListener::bind(path).unwrap();
                listener.set_nonblocking(true).unwrap();
                let (stop, gate, released) = (&stop, &gate, &released);
                scope.spawn(move || {
                    while !stop.load(Ordering::Relaxed) {
                        let stream = match listener.accept() {
                            Ok((stream, _)) => stream,
                            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                                thread::sleep(Duration::from_millis(1));
                                continue;
                            }
                            Err(error) => panic!("{error}"),
                        };
                        stream
                            .set_read_timeout(Some(Duration::from_secs(5)))
                            .unwrap();
                        let (request, fd): (Request, _) = receive(&stream).unwrap();
                        assert!(fd.is_none());
                        let (namespace_pid, operation, reply, name) = match request {
                            Request::BeginCheckpoint { namespace_pid }
                            | Request::Inspect { namespace_pid } => (
                                namespace_pid,
                                None,
                                Reply::Inspection {
                                    records: model.records.clone(),
                                },
                                if matches!(request, Request::BeginCheckpoint { .. }) {
                                    "begin_checkpoint"
                                } else {
                                    "inspect"
                                }
                                .into(),
                            ),
                            Request::Execute {
                                namespace_pid,
                                operation,
                            } => (
                                namespace_pid,
                                Some(operation),
                                Reply::Completed {
                                    operation,
                                    bytes: 0,
                                },
                                serde_json::to_value(operation)
                                    .unwrap()
                                    .as_str()
                                    .unwrap()
                                    .to_owned(),
                            ),
                            Request::Export { .. } => {
                                panic!("coordinator must not request CUDA FDs")
                            }
                        };
                        assert_eq!(namespace_pid, pid);
                        model.operations.push(name);
                        if operation.is_some() && model.disconnect == operation {
                            continue;
                        }
                        if operation.is_some() && barrier == operation {
                            let mut arrived = gate.0.lock().unwrap();
                            *arrived += 1;
                            gate.1.notify_all();
                            let (arrived, _) = gate
                                .1
                                .wait_timeout_while(arrived, Duration::from_secs(2), |n| *n < count)
                                .unwrap();
                            assert_eq!(*arrived, count, "phase dispatched serially");
                            drop(arrived);
                            // Hold one reply to detect advancing another rank before the join.
                            if index == 0 {
                                thread::sleep(Duration::from_millis(100));
                                released.store(true, Ordering::SeqCst);
                            }
                        } else if matches!(
                            (barrier, operation),
                            (
                                Some(Operation::PrepareMulticast),
                                Some(Operation::SaveAllocations)
                            ) | (
                                Some(Operation::RestoreMulticastDevices),
                                Some(Operation::RestoreMulticastBindings)
                            )
                        ) {
                            assert!(
                                released.load(Ordering::SeqCst),
                                "advanced before the held reply"
                            );
                        }
                        send(
                            &stream,
                            &Response {
                                namespace_pid: model.namespace_pid,
                                result: if operation.is_some() && model.fail == operation {
                                    Err("injected participant failure".into())
                                } else {
                                    Ok(reply)
                                },
                            },
                            None,
                        )
                        .unwrap();
                    }
                });
            }
            let output = command.output();
            stop.store(true, Ordering::Relaxed);
            output.unwrap()
        })
    }
}

fn allocation(creator: u8) -> Record {
    Record::Allocation {
        allocation: AllocationReference {
            id: ID,
            creator_pid: creator.into(),
        },
        content: false,
        size: 4096,
        allocation_type: 1,
        handle_types: 1,
        location: (1, 0),
        virtual_allocation_handle_count: 1,
    }
}

fn mapping(size: u64, address: u64) -> Record {
    Record::Mapping {
        allocation: ALLOCATION,
        address,
        size,
        offset: 0,
        access: vec![],
    }
}

#[test]
fn preflight_refusals_do_not_mutate_or_publish_state() {
    for case in ["missing-creator", "mapping", "member"] {
        let mut fixture = Fixture::new(1, None);
        {
            let model = &mut fixture.models[0];
            match case {
                "missing-creator" => model.records = vec![allocation(2)],
                "mapping" => model.records = vec![allocation(1), mapping(8192, 0x10000)],
                "member" => {
                    model.records = vec![
                        allocation(1),
                        Record::Multicast {
                            allocation: MULTICAST,
                            devices: 1,
                            size: 16384,
                            handle_types: 1,
                            flags: 0,
                            virtual_multicast_handle_count: 1,
                        },
                        Record::MulticastDevice {
                            allocation: MULTICAST,
                            device: 0,
                        },
                        Record::MulticastBinding {
                            allocation: MULTICAST,
                            source: BindingSource::Memory(MemberRange {
                                allocation: ALLOCATION,
                                offset: 0,
                            }),
                            size: 8192,
                            offset: 0,
                            flags: 0,
                            version: BindingVersion::V1,
                            device: 0,
                        },
                    ]
                }
                _ => unreachable!(),
            }
        }
        let output = fixture.run("--prepare");
        assert!(!output.status.success(), "{case}: {output:?}");
        let error = String::from_utf8_lossy(&output.stderr);
        assert!(error.contains("participant 1:"), "{error}");
        assert!(error.contains("AllocationReference"), "{error}");
        assert_eq!(fixture.models[0].operations, ["begin_checkpoint"], "{case}");
        assert!(!fixture.directory.join("cuinterpose.state").exists());
    }
}

#[test]
fn failed_phase_stops_before_next_phase_and_state_publication() {
    let mut fixture = Fixture::new(2, None);
    fixture.models[1].fail = Some(Operation::PrepareMulticast);
    assert!(!fixture.run("--prepare").status.success());
    for model in &fixture.models {
        assert_eq!(model.operations, ["begin_checkpoint", "prepare_multicast"]);
    }
    assert!(!fixture.directory.join("cuinterpose.state").exists());
}

#[test]
fn wrong_transfer_size_stops_before_teardown_and_state_publication() {
    let mut fixture = Fixture::new(1, None);
    let mut shared = allocation(1);
    if let Record::Allocation { content, .. } = &mut shared {
        *content = true;
    }
    fixture.models[0].records = vec![shared];
    // The participant claims zero bytes instead of the creator's 4096 bytes.
    let output = fixture.run("--prepare");
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("transfer size"));
    assert_eq!(
        fixture.models[0].operations,
        ["begin_checkpoint", "prepare_multicast", "save_allocations"]
    );
    assert!(!fixture.directory.join("cuinterpose.state").exists());
}

#[test]
fn lost_reply_is_not_retried_or_followed_by_another_phase() {
    let mut fixture = Fixture::new(2, None);
    fixture.models[1].disconnect = Some(Operation::PrepareMulticast);
    assert!(!fixture.run("--prepare").status.success());
    for model in &fixture.models {
        assert_eq!(model.operations, ["begin_checkpoint", "prepare_multicast"]);
    }
    assert!(!fixture.directory.join("cuinterpose.state").exists());
}

#[test]
fn parallel_prepare_and_restore_barriers_preserve_canonical_state() {
    for barrier in [
        Operation::PrepareMulticast,
        Operation::RestoreMulticastDevices,
    ] {
        let mut fixture = Fixture::new(2, Some(barrier));
        fixture.models[0].records = vec![mapping(4096, 0x10000), allocation(1)];
        fixture.models[1].records = vec![allocation(1)];
        let output = fixture.run("--prepare");
        assert!(output.status.success(), "{output:?}");
        let state = std::fs::read(fixture.directory.join("cuinterpose.state")).unwrap();
        let participants: Manifest = decode(&state).unwrap();
        assert_eq!(participants.len(), 2);
        for (id, participant) in participants {
            let mut expected = fixture.models[id as usize - 1].records.clone();
            expected.sort();
            assert_eq!(participant, expected);
        }
        // Input record order is immaterial to final topology comparison.
        fixture.models[0].records.reverse();
        let output = fixture.run("--restore");
        assert!(output.status.success(), "{output:?}");
        for model in &fixture.models {
            assert_eq!(
                model.operations,
                [
                    "begin_checkpoint",
                    "prepare_multicast",
                    "save_allocations",
                    "prepare_unicast",
                    "inspect",
                    "load_allocations",
                    "restore_unicast",
                    "restore_multicast_creators",
                    "restore_multicast_importers",
                    "restore_multicast_devices",
                    "restore_multicast_bindings",
                    "inspect",
                ]
            );
        }
    }
}

#[test]
fn restore_rejects_missing_corrupt_or_changed_state() {
    for case in ["missing", "corrupt", "identity", "topology"] {
        let mut fixture = Fixture::new(1, None);
        if case != "missing" {
            fixture.models[0].records = vec![allocation(1), mapping(4096, 0x10000)];
            let output = fixture.run("--prepare");
            assert!(output.status.success(), "{output:?}");
        }
        {
            let model = &mut fixture.models[0];
            model.operations.clear();
            match case {
                "corrupt" => std::fs::write(
                    fixture.directory.join("cuinterpose.state"),
                    b"not-cuinterpose-state\n",
                )
                .unwrap(),
                "identity" => model.namespace_pid = 3,
                "topology" => model.records = vec![allocation(1), mapping(4096, 0x30000)],
                _ => {}
            }
        }
        let output = fixture.run("--restore");
        assert!(!output.status.success(), "{case}: {output:?}");
        let model = &fixture.models[0];
        match case {
            "missing" | "corrupt" => assert!(model.operations.is_empty()),
            "identity" => assert_eq!(model.operations, ["inspect"]),
            "topology" => assert_eq!(model.operations.last(), Some(&"inspect".to_string())),
            _ => unreachable!(),
        }
    }
}

#[test]
fn usage_errors() {
    for args in [
        "",
        "--prepare --checkpoint-dir /tmp",
        "--prepare --checkpoint-dir /tmp --control-dir relative --process 1",
        "--prepare --checkpoint-dir /tmp --control-dir /tmp --process 0",
        "--prepare --checkpoint-dir /tmp --control-dir /tmp --process 1 --process 1",
        "--prepare --checkpoint-dir /tmp --control-dir /tmp --process not-a-pid",
    ] {
        assert!(
            !Command::new(env!("CARGO_BIN_EXE_cuinterpose-coordinator"))
                .args(args.split_whitespace())
                .output()
                .unwrap()
                .status
                .success()
        );
    }
}

#[test]
fn later_participant_size_overflow_starts_no_save_workers() {
    let mut fixture = Fixture::new(2, None);
    let records = [u64::MAX, 1]
        .into_iter()
        .enumerate()
        .map(|(index, size)| {
            let mut record = allocation(2);
            if let Record::Allocation {
                allocation,
                content,
                size: length,
                ..
            } = &mut record
            {
                allocation.id = [index as u8; 16];
                *content = true;
                *length = size;
            }
            record
        })
        .collect();
    fixture.models[1].records = records;
    let output = fixture.run("--prepare");
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("allocation size overflow"));
    for model in &fixture.models {
        assert_eq!(model.operations, ["begin_checkpoint", "prepare_multicast"]);
    }
    assert!(!fixture.directory.join("cuinterpose.state").exists());
}

#[test]
fn multicast_creation_sizes_must_agree() {
    for sizes in [[4096, 8192], [8192, 4096]] {
        let mut fixture = Fixture::new(2, None);
        for (model, size) in fixture.models.iter_mut().zip(sizes) {
            model.records = vec![Record::Multicast {
                allocation: MULTICAST,
                devices: 2,
                size,
                handle_types: 1,
                flags: 0,
                virtual_multicast_handle_count: 1,
            }];
        }
        let output = fixture.run("--prepare");
        assert!(!output.status.success());
        assert!(
            String::from_utf8_lossy(&output.stderr).contains("inconsistent multicast properties")
        );
        for model in &fixture.models {
            assert_eq!(model.operations, ["begin_checkpoint"]);
        }
        assert!(!fixture.directory.join("cuinterpose.state").exists());
    }
}

#[test]
fn cuda_accepted_multicast_extents_do_not_change_creation_properties() {
    let mut fixture = Fixture::new(1, None);
    fixture.models[0].records = vec![
        allocation(1),
        Record::Multicast {
            allocation: MULTICAST,
            devices: 1,
            size: 4096,
            handle_types: 1,
            flags: 0,
            virtual_multicast_handle_count: 1,
        },
        Record::MulticastDevice {
            allocation: MULTICAST,
            device: 0,
        },
        Record::MulticastBinding {
            allocation: MULTICAST,
            source: BindingSource::Memory(MemberRange {
                allocation: ALLOCATION,
                offset: 0,
            }),
            size: 4096,
            offset: 4096,
            flags: 0,
            version: BindingVersion::V1,
            device: 0,
        },
        Record::MulticastMapping {
            allocation: MULTICAST,
            address: 0x10000,
            size: 8192,
            offset: 0,
            flags: 0,
            access: vec![],
        },
    ];
    let output = fixture.run("--prepare");
    assert!(output.status.success(), "{output:?}");
    let output = fixture.run("--restore");
    assert!(output.status.success(), "{output:?}");
}
