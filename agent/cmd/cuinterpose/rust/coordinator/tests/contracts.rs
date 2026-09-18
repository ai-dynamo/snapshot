// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Behavioral cases from the C coordinator suite, using the typed protocol.
use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, BindingVersion, CUmemAllocationHandleType,
    CUmemAllocationType, CUmemLocation, CUmemLocationType, CUmulticastObjectProp, Manifest,
    MemberRange, Operation, Reply, Request, Response, StateEntry, decode, receive, send,
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
    creator: [1; 16],
};
const MULTICAST: AllocationReference = AllocationReference {
    id: GROUP,
    creator: [1; 16],
};

#[derive(Default)]
struct Model {
    entries: Vec<StateEntry>,
    raw: u64,
    unsupported: u64,
    fail: Option<Operation>,
    operations: Vec<String>,
    identity: u8,
}

struct Fixture {
    directory: PathBuf,
    _temporary: tempfile::TempDir,
    models: Vec<Arc<Mutex<Model>>>,
    stop: Arc<AtomicBool>,
    servers: Vec<JoinHandle<()>>,
}

impl Fixture {
    fn new(count: usize, rendezvous: Option<Operation>) -> Self {
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
                identity: index as u8 + 1,
                ..Model::default()
            }));
            models.push(model.clone());
            let (stop, gate, released) = (stop.clone(), gate.clone(), released.clone());
            let server_directory = directory.clone();
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
                    let (operation, response) = match request {
                        Request::Identify => (None, Reply::Identified),
                        Request::Rendezvous {
                            participant,
                            participants,
                        } => {
                            assert_eq!(participant, [model.identity; 16]);
                            assert_eq!(participants.len(), count);
                            assert_eq!(
                                participants[&participant],
                                PathBuf::from(format!(
                                    "{}/cuinterpose-{}.sock",
                                    server_directory.display(),
                                    index + 1
                                ))
                            );
                            (None, Reply::Ready)
                        }
                        Request::Inspect { .. } => (
                            None,
                            Reply::Inspection {
                                entries: model.entries.clone(),
                                live_raw_imports: model.raw,
                                unsupported_creations: model.unsupported,
                            },
                        ),
                        Request::Execute { operation, .. } => (
                            Some(operation),
                            Reply::Completed {
                                operation,
                                bytes: 0,
                                copy_us: 0,
                            },
                        ),
                        Request::Export { .. } => panic!("coordinator must not request CUDA FDs"),
                    };
                    model.operations.push(match &response {
                        Reply::Identified => "identify".into(),
                        Reply::Ready => "rendezvous".into(),
                        Reply::Inspection { .. } => "inspect".into(),
                        Reply::Completed { operation, .. } => serde_json::to_value(operation)
                            .unwrap()
                            .as_str()
                            .unwrap()
                            .into(),
                        _ => panic!("unexpected response"),
                    });
                    let response = Response {
                        participant: [model.identity; 16],
                        result: if model.fail.is_some() && model.fail == operation {
                            Err("injected participant failure".into())
                        } else {
                            Ok(response)
                        },
                    };
                    drop(model);
                    if rendezvous.is_some() && rendezvous == operation {
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
                        (rendezvous, operation),
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
            .args([mode, "--proc-root", "", "--checkpoint-dir"])
            .arg(&self.directory)
            .arg("--control-dir")
            .arg(&self.directory);
        for index in 1..=self.models.len() {
            command.args(["--process", &index.to_string(), &index.to_string()]);
        }
        let output = command.output().unwrap();
        if output.status.success() {
            let reports: Vec<serde_json::Value> = String::from_utf8_lossy(&output.stdout)
                .lines()
                .map(|line| serde_json::from_str(line).unwrap())
                .collect();
            let phases: &[&str] = if mode == "--prepare" {
                &[
                    "rendezvous",
                    "inspect",
                    "validate",
                    "prepare_multicast",
                    "save_allocations",
                    "prepare_unicast",
                    "state_write",
                ]
            } else {
                &[
                    "rendezvous",
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

fn allocation(creator: u8) -> StateEntry {
    StateEntry::Allocation {
        allocation: AllocationReference {
            id: ID,
            creator: [creator; 16],
        },
        content: false,
        size: 4096,
        allocation_type: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
        handle_types: CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
        location: CUmemLocation {
            type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
            id: 0,
        },
        logical_handle_count: 1,
    }
}

fn mapping(size: u64, address: u64) -> StateEntry {
    StateEntry::Mapping {
        allocation: ALLOCATION,
        address,
        size,
        offset: 0,
        access: vec![],
    }
}

#[test]
fn preflight_refusals_do_not_mutate_or_publish_state() {
    for case in [
        "raw",
        "unsupported",
        "records",
        "missing-creator",
        "mapping",
        "member",
    ] {
        let fixture = Fixture::new(1, None);
        {
            let mut model = fixture.models[0].lock().unwrap();
            match case {
                "raw" => model.raw = 3,
                "unsupported" => model.unsupported = 2,
                "records" => {
                    model.entries = vec![allocation(1); cuinterpose_protocol::MAX_ENTRIES + 1]
                }
                "missing-creator" => model.entries = vec![allocation(2)],
                "mapping" => model.entries = vec![allocation(1), mapping(8192, 0x10000)],
                "member" => {
                    model.entries = vec![
                        allocation(1),
                        StateEntry::Multicast {
                            allocation: MULTICAST,
                            properties: CUmulticastObjectProp {
                                numDevices: 1,
                                size: 16384,
                                handleTypes: 1,
                                flags: 0,
                            },
                            logical_handle_count: 1,
                        },
                        StateEntry::MulticastDevice {
                            allocation: MULTICAST,
                            device: 0,
                        },
                        StateEntry::MulticastBinding {
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
            ["identify", "rendezvous", "inspect"],
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
            ["identify", "rendezvous", "inspect", "prepare_multicast"]
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
        fixture.models[0].lock().unwrap().entries = vec![mapping(4096, 0x10000), allocation(1)];
        fixture.models[1].lock().unwrap().entries = vec![allocation(1)];
        let output = fixture.run("--prepare");
        assert!(output.status.success(), "{output:?}");
        let state = std::fs::read(fixture.directory.join("cuinterpose.state")).unwrap();
        let participants: Manifest = decode(&state).unwrap();
        assert_eq!(participants.len(), 2);
        for (id, participant) in participants {
            let mut expected = fixture.models[id[0] as usize - 1]
                .lock()
                .unwrap()
                .entries
                .clone();
            expected.sort();
            assert_eq!(participant.entries, expected);
            assert_eq!(
                participant.socket_path,
                fixture
                    .directory
                    .join(format!("cuinterpose-{}.sock", id[0]))
            );
        }
        // Input record order is immaterial to final topology comparison.
        fixture.models[0].lock().unwrap().entries.reverse();
        let output = fixture.run("--restore");
        assert!(output.status.success(), "{output:?}");
        for model in &fixture.models {
            assert_eq!(
                model.lock().unwrap().operations,
                [
                    "identify",
                    "rendezvous",
                    "inspect",
                    "prepare_multicast",
                    "save_allocations",
                    "prepare_unicast",
                    "identify",
                    "rendezvous",
                    "load_allocations",
                    "restore_unicast",
                    "restore_multicast_creators",
                    "restore_multicast_importers",
                    "restore_multicast_devices",
                    "restore_multicast_bindings",
                    "identify",
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
            fixture.models[0].lock().unwrap().entries = vec![allocation(1), mapping(4096, 0x10000)];
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
                "identity" => model.identity = 3,
                "topology" => model.entries = vec![allocation(1), mapping(4096, 0x30000)],
                _ => {}
            }
        }
        let output = fixture.run("--restore");
        assert!(!output.status.success(), "{case}: {output:?}");
        let model = fixture.models[0].lock().unwrap();
        match case {
            "missing" | "corrupt" => assert!(model.operations.is_empty()),
            "identity" => assert_eq!(model.operations, ["identify"]),
            "topology" => assert_eq!(model.operations.last(), Some(&"inspect".to_string())),
            _ => unreachable!(),
        }
    }
}

#[test]
fn usage_errors() {
    for args in [
        vec![],
        vec!["--prepare", "--proc-root", "", "--checkpoint-dir", "/tmp"],
        vec![
            "--prepare",
            "--proc-root",
            "",
            "--checkpoint-dir",
            "/tmp",
            "--control-dir",
            "relative",
            "--process",
            "1",
            "1",
        ],
        vec![
            "--prepare",
            "--proc-root",
            "",
            "--checkpoint-dir",
            "/tmp",
            "--control-dir",
            "/tmp",
            "--process",
            "1",
        ],
        vec![
            "--prepare",
            "--proc-root",
            "",
            "--checkpoint-dir",
            "/tmp",
            "--control-dir",
            "/tmp",
            "--process",
            "0",
            "1",
        ],
        vec![
            "--prepare",
            "--proc-root",
            "",
            "--checkpoint-dir",
            "/tmp",
            "--control-dir",
            "/tmp",
            "--process",
            "not-a-pid",
            "1",
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
