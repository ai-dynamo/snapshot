// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Behavioral cases from the C coordinator suite, using typed v4 messages.
use cuinterpose_protocol::{
    AllocationId, BindingSource, BindingVersion, MemberRange, Operation, Participant,
    ParticipantId, Record, Reply, Request, Response, decode, receive, send,
};
use std::io::{Read, Write};
use std::os::fd::AsRawFd;
use std::os::unix::net::{UnixListener, UnixStream};
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

const ID: AllocationId = AllocationId([1; 16]);
const GROUP: AllocationId = AllocationId([2; 16]);

#[derive(Default)]
struct Model {
    records: Vec<Record>,
    raw: u64,
    unsupported: u64,
    fail: Option<Operation>,
    operations: Vec<String>,
    identity: u8,
}

#[test]
fn pipelined_load_readiness_and_failures() {
    for case in ["overlap", "unknown", "duplicate", "eof", "load-failure"] {
        let fixture = Fixture::new(2, (case == "overlap").then_some(Operation::LoadAllocations));
        assert!(fixture.run("--prepare").status.success());
        for model in &fixture.models {
            model.lock().unwrap().operations.clear();
        }
        if case == "load-failure" {
            fixture.models[0].lock().unwrap().fail = Some(Operation::LoadAllocations);
        }
        let (mut agent, coordinator) = UnixStream::pair().unwrap();
        agent
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        rustix::io::fcntl_setfd(&coordinator, rustix::io::FdFlags::empty()).unwrap();
        let sessions: Vec<_> = (0..2)
            .map(|_| std::fs::File::open("/dev/null").unwrap())
            .collect();
        let mut command = fixture.command("--restore");
        command.args([
            "--content-storage",
            "pagebroker",
            "--restore-ready-fd",
            &coordinator.as_raw_fd().to_string(),
        ]);
        for (index, session) in sessions.iter().enumerate() {
            rustix::io::fcntl_setfd(session, rustix::io::FdFlags::empty()).unwrap();
            command.args([
                "--allocation-session",
                &ParticipantId([index as u8 + 1; 16]).to_string(),
                &session.as_raw_fd().to_string(),
            ]);
        }
        command
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped());
        let child = command.spawn().unwrap();
        drop(coordinator);
        drop(sessions);
        let mut ack = [1; 4];
        agent.read_exact(&mut ack).unwrap();
        assert_eq!(ack, [0; 4]);
        if case == "unknown" {
            agent.write_all(&99i32.to_be_bytes()).unwrap();
        } else if case != "eof" {
            agent.write_all(&1i32.to_be_bytes()).unwrap();
            let deadline = std::time::Instant::now() + Duration::from_secs(5);
            while !fixture.models[0]
                .lock()
                .unwrap()
                .operations
                .contains(&"load_allocations".into())
            {
                assert!(std::time::Instant::now() < deadline);
                thread::sleep(Duration::from_millis(1));
            }
            // First LOAD starts while rank 2 has not finished native restore.
            assert!(
                !fixture.models[1]
                    .lock()
                    .unwrap()
                    .operations
                    .contains(&"load_allocations".into())
            );
            match case {
                "overlap" => agent.write_all(&2i32.to_be_bytes()).unwrap(),
                "duplicate" => agent.write_all(&1i32.to_be_bytes()).unwrap(),
                "load-failure" => assert_eq!(agent.read(&mut ack).unwrap(), 0),
                _ => unreachable!(),
            }
        }
        agent.shutdown(std::net::Shutdown::Write).unwrap();
        let output = child.wait_with_output().unwrap();
        assert_eq!(
            output.status.success(),
            case == "overlap",
            "{case}: {output:?}"
        );
        for model in &fixture.models {
            assert_eq!(
                model
                    .lock()
                    .unwrap()
                    .operations
                    .contains(&"restore_unicast".into()),
                case == "overlap"
            );
        }
    }
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
                    drop(fd);
                    let mut model = model.lock().unwrap();
                    let (operation, response) = match request {
                        Request::Handshake => (None, Reply::Handshake),
                        Request::Inspect { .. } => (
                            None,
                            Reply::Inspection {
                                records: model.records.clone(),
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
                        Reply::Handshake => "handshake".into(),
                        Reply::Inspection { .. } => "inspect".into(),
                        Reply::Completed { operation, .. } => serde_json::to_value(operation)
                            .unwrap()
                            .as_str()
                            .unwrap()
                            .into(),
                        _ => panic!("unexpected response"),
                    });
                    let response = Response {
                        participant: ParticipantId([model.identity; 16]),
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
                            Some(Operation::LoadAllocations),
                            Some(Operation::RestoreUnicast)
                        ) | (
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

    fn command(&self, mode: &str) -> Command {
        let mut command = Command::new(env!("CARGO_BIN_EXE_cuinterpose-coordinator"));
        command
            .args([mode, "--proc-root", "", "--checkpoint-dir"])
            .arg(&self.directory)
            .arg("--control-dir")
            .arg(&self.directory);
        for index in 1..=self.models.len() {
            command.args(["--process", &index.to_string(), &index.to_string()]);
        }
        command
    }

    fn run(&self, mode: &str) -> Output {
        let output = self.command(mode).output().unwrap();
        if output.status.success() {
            let reports: Vec<serde_json::Value> = String::from_utf8_lossy(&output.stdout)
                .lines()
                .map(|line| serde_json::from_str(line).unwrap())
                .filter(|r: &serde_json::Value| {
                    !r["phase"].as_str().unwrap().starts_with("rank_load_")
                })
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
                    "handshake",
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

fn allocation(creator: bool) -> Record {
    Record::Allocation {
        id: ID,
        creator,
        content: false,
        size: 4096,
        allocation_type: 1,
        handle_types: 1,
        location_type: 1,
        location_id: 0,
        handles: 1,
    }
}

fn mapping(size: u64, address: u64) -> Record {
    Record::Mapping {
        id: ID,
        creator: true,
        address,
        size,
        offset: 0,
        access: vec![],
    }
}

#[test]
fn preflight_refusals_do_not_mutate_or_publish_state() {
    for case in ["raw", "unsupported", "missing-creator", "mapping", "member"] {
        let fixture = Fixture::new(1, None);
        {
            let mut model = fixture.models[0].lock().unwrap();
            match case {
                "raw" => model.raw = 3,
                "unsupported" => model.unsupported = 2,
                "missing-creator" => model.records = vec![allocation(false)],
                "mapping" => model.records = vec![allocation(true), mapping(8192, 0x10000)],
                "member" => {
                    model.records = vec![
                        allocation(true),
                        Record::Multicast {
                            id: GROUP,
                            creator: ParticipantId([1; 16]),
                            owned: true,
                            size: 16384,
                            handles: 1,
                            handle_types: 1,
                            flags: 0,
                            devices: 1,
                        },
                        Record::MulticastDevice {
                            id: GROUP,
                            device: 0,
                        },
                        Record::MulticastBinding {
                            id: GROUP,
                            source: BindingSource::Memory(MemberRange {
                                allocation: ID,
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
            ["handshake", "inspect"],
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
            ["handshake", "inspect", "prepare_multicast"]
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
        fixture.models[0].lock().unwrap().records = vec![mapping(4096, 0x10000), allocation(true)];
        fixture.models[1].lock().unwrap().records = vec![allocation(false)];
        let output = fixture.run("--prepare");
        assert!(output.status.success(), "{output:?}");
        let state = std::fs::read(fixture.directory.join("cuinterpose.state")).unwrap();
        let participants: Vec<Participant> = decode(&state).unwrap();
        assert_eq!(participants.len(), 2);
        for participant in participants {
            let mut expected = fixture.models[participant.id.0[0] as usize - 1]
                .lock()
                .unwrap()
                .records
                .clone();
            expected.sort();
            assert_eq!(participant.records, expected);
        }
        // Input record order is immaterial to final topology comparison.
        fixture.models[0].lock().unwrap().records.reverse();
        let output = fixture.run("--restore");
        assert!(output.status.success(), "{output:?}");
        for model in &fixture.models {
            assert_eq!(
                model.lock().unwrap().operations,
                [
                    "handshake",
                    "inspect",
                    "prepare_multicast",
                    "save_allocations",
                    "prepare_unicast",
                    "handshake",
                    "load_allocations",
                    "restore_unicast",
                    "restore_multicast_creators",
                    "restore_multicast_importers",
                    "restore_multicast_devices",
                    "restore_multicast_bindings",
                    "handshake",
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
            fixture.models[0].lock().unwrap().records =
                vec![allocation(true), mapping(4096, 0x10000)];
            let output = fixture.run("--prepare");
            assert!(output.status.success(), "{output:?}");
        }
        {
            let mut model = fixture.models[0].lock().unwrap();
            model.operations.clear();
            match case {
                "corrupt" => std::fs::write(
                    fixture.directory.join("cuinterpose.state"),
                    b"cuinterpose-state-v2\n",
                )
                .unwrap(),
                "identity" => model.identity = 3,
                "topology" => model.records = vec![allocation(true), mapping(4096, 0x30000)],
                _ => {}
            }
        }
        let output = fixture.run("--restore");
        assert!(!output.status.success(), "{case}: {output:?}");
        let model = fixture.models[0].lock().unwrap();
        match case {
            "missing" | "corrupt" => assert!(model.operations.is_empty()),
            "identity" => assert_eq!(model.operations, ["handshake"]),
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
