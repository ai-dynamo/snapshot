// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Behavioral cases from the C coordinator suite, using the typed protocol.
use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, BindingVersion, Manifest, MemberRange,
    Operation, Record, Reply, Request, Response, decode, receive, send,
};
use std::os::unix::net::UnixListener;
use std::{
    path::PathBuf,
    process::{Command, Output},
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    thread::{self, JoinHandle},
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
    models: Vec<Arc<Mutex<Model>>>,
    stop: Arc<AtomicBool>,
    servers: Vec<JoinHandle<()>>,
}

impl Fixture {
    fn new(count: usize, barrier: Option<Operation>) -> Self {
        let temporary = tempfile::tempdir().unwrap();
        let directory = temporary.path().to_path_buf();
        let stop = Arc::new(AtomicBool::new(false));
        let gate = Arc::new((Mutex::new(0), Condvar::new()));
        let released = Arc::new(AtomicBool::new(false));
        let mut models = Vec::new();
        let mut servers = Vec::new();
        for index in 0..count {
            let listener =
                UnixListener::bind(directory.join(format!("cuinterpose-{}.sock", index + 1)))
                    .unwrap();
            listener.set_nonblocking(true).unwrap();
            let model = Arc::new(Mutex::new(Model {
                namespace_pid: index as u32 + 1,
                ..Model::default()
            }));
            models.push(model.clone());
            let (stop, gate, released) = (stop.clone(), gate.clone(), released.clone());
            servers.push(thread::spawn(move || {
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
                    let mut model = model.lock().unwrap();
                    let beginning = matches!(request, Request::BeginCheckpoint { .. });
                    let (operation, response) = match request {
                        Request::BeginCheckpoint { namespace_pid }
                        | Request::Inspect { namespace_pid } => {
                            assert_eq!(namespace_pid, index as u32 + 1);
                            (
                                None,
                                Reply::Inspection {
                                    records: model.records.clone(),
                                },
                            )
                        }
                        Request::Execute {
                            namespace_pid,
                            operation,
                        } => {
                            assert_eq!(namespace_pid, index as u32 + 1);
                            (
                                Some(operation),
                                Reply::Completed {
                                    operation,
                                    bytes: 0,
                                    copy_us: 0,
                                },
                            )
                        }
                        Request::Export { .. } => panic!("coordinator must not request CUDA FDs"),
                    };
                    model.operations.push(match &response {
                        Reply::Inspection { .. } => if beginning {
                            "begin_checkpoint"
                        } else {
                            "inspect"
                        }
                        .into(),
                        Reply::Completed { operation, .. } => serde_json::to_value(operation)
                            .unwrap()
                            .as_str()
                            .unwrap()
                            .into(),
                        _ => panic!("unexpected response"),
                    });
                    if model.disconnect.is_some() && model.disconnect == operation {
                        // The command may have completed, but its reply was lost.
                        // Reconnecting and repeating it would replay mutation.
                        continue;
                    }
                    let response = Response {
                        namespace_pid: model.namespace_pid,
                        result: if model.fail.is_some() && model.fail == operation {
                            Err("injected participant failure".into())
                        } else {
                            Ok(response)
                        },
                    };
                    drop(model);
                    if barrier.is_some() && barrier == operation {
                        let mut arrived = gate.0.lock().unwrap();
                        *arrived += 1;
                        gate.1.notify_all();
                        let (arrived, _) = gate
                            .1
                            .wait_timeout_while(arrived, Duration::from_secs(2), |n| *n < count)
                            .unwrap();
                        assert_eq!(*arrived, count, "phase dispatched serially");
                        drop(arrived);
                        // Hold one reply after every rank has arrived. The
                        // coordinator must not advance any rank past this join.
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
                        assert_eq!(
                            *gate.0.lock().unwrap(),
                            count,
                            "advanced before all ranks arrived"
                        );
                        assert!(
                            released.load(Ordering::SeqCst),
                            "advanced before the held reply"
                        );
                    }
                    send(&stream, &response, None).unwrap();
                }
            }));
        }
        Self {
            directory,
            _temporary: temporary,
            models,
            stop,
            servers,
        }
    }

    fn run(&self, mode: &str) -> Output {
        let mut command = Command::new(env!("CARGO_BIN_EXE_cuinterpose-coordinator"));
        command
            .args([mode, "--checkpoint-dir"])
            .arg(&self.directory)
            .arg("--control-dir")
            .arg(&self.directory);
        for index in 1..=self.models.len() {
            command.args(["--process", &index.to_string()]);
        }
        let output = command.output().unwrap();
        if output.status.success() {
            let reports: Vec<serde_json::Value> = String::from_utf8_lossy(&output.stdout)
                .lines()
                .map(|line| serde_json::from_str(line).unwrap())
                .collect();
            let phases: &[&str] = if mode == "--prepare" {
                &[
                    "inspect",
                    "validate",
                    "prepare_multicast",
                    "save_allocations",
                    "prepare_unicast",
                    "state_write",
                ]
            } else {
                &[
                    "inspect",
                    "load_allocations",
                    "restore_unicast",
                    "restore_multicast",
                    "validate",
                ]
            };
            assert_eq!(
                reports
                    .iter()
                    .map(|r| r["phase"].as_str().unwrap())
                    .collect::<Vec<_>>(),
                phases
            );
            for report in reports {
                assert_eq!(report["status"], "ok");
                assert_eq!(report["participants"], self.models.len());
                assert!(report["elapsed_ms"].is_number());
            }
        }
        output
    }
}

impl Drop for Fixture {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        for server in self.servers.drain(..) {
            server.join().unwrap();
        }
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
        let fixture = Fixture::new(1, None);
        {
            let mut model = fixture.models[0].lock().unwrap();
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
        assert_eq!(
            fixture.models[0].lock().unwrap().operations,
            ["begin_checkpoint"],
            "{case}"
        );
        assert!(!fixture.directory.join("cuinterpose.state").exists());
    }
}

#[test]
fn failed_phase_stops_before_next_phase_and_state_publication() {
    let fixture = Fixture::new(2, None);
    fixture.models[1].lock().unwrap().fail = Some(Operation::PrepareMulticast);
    assert!(!fixture.run("--prepare").status.success());
    for model in &fixture.models {
        assert_eq!(
            model.lock().unwrap().operations,
            ["begin_checkpoint", "prepare_multicast"]
        );
    }
    assert!(!fixture.directory.join("cuinterpose.state").exists());
}

#[test]
fn lost_reply_is_not_retried_or_followed_by_another_phase() {
    let fixture = Fixture::new(2, None);
    fixture.models[1].lock().unwrap().disconnect = Some(Operation::PrepareMulticast);
    assert!(!fixture.run("--prepare").status.success());
    for model in &fixture.models {
        assert_eq!(
            model.lock().unwrap().operations,
            ["begin_checkpoint", "prepare_multicast"]
        );
    }
    assert!(!fixture.directory.join("cuinterpose.state").exists());
}

#[test]
fn parallel_prepare_and_restore_barriers_preserve_canonical_state() {
    for barrier in [
        Operation::PrepareMulticast,
        Operation::RestoreMulticastDevices,
    ] {
        let fixture = Fixture::new(2, Some(barrier));
        fixture.models[0].lock().unwrap().records = vec![mapping(4096, 0x10000), allocation(1)];
        fixture.models[1].lock().unwrap().records = vec![allocation(1)];
        let output = fixture.run("--prepare");
        assert!(output.status.success(), "{output:?}");
        let state = std::fs::read(fixture.directory.join("cuinterpose.state")).unwrap();
        let participants: Manifest = decode(&state).unwrap();
        assert_eq!(participants.len(), 2);
        for (id, participant) in participants {
            let mut expected = fixture.models[id as usize - 1]
                .lock()
                .unwrap()
                .records
                .clone();
            expected.sort();
            assert_eq!(participant, expected);
        }
        // Input record order is immaterial to final topology comparison.
        fixture.models[0].lock().unwrap().records.reverse();
        let output = fixture.run("--restore");
        assert!(output.status.success(), "{output:?}");
        for model in &fixture.models {
            assert_eq!(
                model.lock().unwrap().operations,
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
        let fixture = Fixture::new(1, None);
        if case != "missing" {
            fixture.models[0].lock().unwrap().records = vec![allocation(1), mapping(4096, 0x10000)];
            let output = fixture.run("--prepare");
            assert!(output.status.success(), "{output:?}");
        }
        {
            let mut model = fixture.models[0].lock().unwrap();
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
        let model = fixture.models[0].lock().unwrap();
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
        vec![],
        vec!["--prepare", "--checkpoint-dir", "/tmp"],
        vec![
            "--prepare",
            "--checkpoint-dir",
            "/tmp",
            "--control-dir",
            "relative",
            "--process",
            "1",
        ],
        vec![
            "--prepare",
            "--checkpoint-dir",
            "/tmp",
            "--control-dir",
            "/tmp",
            "--process",
            "0",
        ],
        vec![
            "--prepare",
            "--checkpoint-dir",
            "/tmp",
            "--control-dir",
            "/tmp",
            "--process",
            "1",
            "--process",
            "1",
        ],
        vec![
            "--prepare",
            "--checkpoint-dir",
            "/tmp",
            "--control-dir",
            "/tmp",
            "--process",
            "not-a-pid",
        ],
    ] {
        assert!(
            !Command::new(env!("CARGO_BIN_EXE_cuinterpose-coordinator"))
                .args(args)
                .output()
                .unwrap()
                .status
                .success()
        );
    }
}
