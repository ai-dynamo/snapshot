// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Run the coordinator against scripted participants on real Unix sockets.

#[path = "../src/fake_agent.rs"]
mod fake_agent;
#[path = "../src/test_records.rs"]
mod test_records;

use cudarc::driver::sys::CUmemAllocationHandleType;
use cuinterpose_protocol::{
    self as protocol, AllocationOwner, AllocationReference, FABRIC_HANDLE_BYTES, FabricHandle,
    FabricReplacement, Manifest, NamespacePid, Operation, Record, Reply, Request, Response,
};
use rustix::event::{PollFd, PollFlags, Timespec, poll};
use rustix::io::FdFlags;
use std::collections::BTreeMap;
use std::fs::File;
use std::io::Read;
use std::os::fd::{FromRawFd, OwnedFd, RawFd};
use std::os::unix::net::{UnixListener, UnixStream};
use std::os::unix::process::CommandExt;
use std::path::PathBuf;
use std::process::{Child, Command, ExitStatus, Stdio};
use std::time::{Duration, Instant};
use test_records::{A, A2, allocation, mapping, shared, with};

const BINARY: &str = env!("CARGO_BIN_EXE_cuinterpose-coordinator");
/// The steps of each mode, without arguments: these participants hold no carrier
/// allocations and no FABRIC handles unless a test says otherwise.
const PREPARE: [Operation; 3] = [
    Operation::PrepareMulticast,
    Operation::SaveAllocations { owners: Vec::new() },
    Operation::PrepareUnicast,
];
const RESTORE: [Operation; 6] = [
    Operation::LoadAllocations,
    Operation::RestoreUnicast {
        replacements: Vec::new(),
    },
    Operation::RestoreMulticastCreators,
    Operation::RestoreMulticastImporters {
        replacements: Vec::new(),
    },
    Operation::RestoreMulticastDevices,
    Operation::RestoreMulticastBindings,
];

/// Bounds every wait that correct code ends promptly, including the coordinator's control
/// and carrier timeouts, only so that a hang fails instead of stalling the suite. No test
/// waits for it to expire. `finish` waits twice as long, so a coordinator stuck on an
/// unexpected request exits first and the harness reports its diagnostics.
const HANG_TIMEOUT: Duration = Duration::from_secs(60);

/// The agent passes the mount namespace, binary, and root descriptors before the
/// checkpoint directory, so the coordinator inherits the directory as descriptor 6.
const CHECKPOINT_FD: RawFd = 6;

/// The agent passes the session socket of a group member's relay as this descriptor.
const SESSION_FD: RawFd = 7;

/// SaveAllocations owners expected by participants 1 and 2.
type Owners = [Vec<AllocationOwner>; 2];

/// Record requests, whose names appear in coordinator diagnostics.
#[derive(Clone, Copy, Debug)]
enum RecordRequest {
    Inspect,
    BeginCheckpoint,
}

impl RecordRequest {
    fn request(self, namespace_pid: NamespacePid) -> Request {
        match self {
            Self::Inspect => Request::Inspect { namespace_pid },
            Self::BeginCheckpoint => Request::BeginCheckpoint { namespace_pid },
        }
    }
}

fn inspection(records: Vec<Record>) -> Result<Reply, String> {
    Ok(Reply::Inspection { records })
}

/// A successful reply to `operation` that copied `bytes` and replaced no handles.
fn completed(operation: &Operation, bytes: u64) -> Result<Reply, String> {
    Ok(match operation {
        Operation::SaveAllocations { .. } => Reply::Saved { bytes },
        Operation::LoadAllocations => Reply::Loaded {
            bytes,
            replacements: Vec::new(),
        },
        Operation::RestoreMulticastCreators => Reply::MulticastRecreated {
            replacements: Vec::new(),
        },
        _ => Reply::Completed,
    })
}

/// `operation` as a participant receives it: with its own owners at SaveAllocations,
/// and with every replacement so far at the steps that import FABRIC handles.
fn addressed(
    operation: &Operation,
    owners: Vec<AllocationOwner>,
    replacements: &[FabricReplacement],
) -> Operation {
    let replacements = replacements.to_vec();
    match operation {
        Operation::SaveAllocations { .. } => Operation::SaveAllocations { owners },
        Operation::RestoreUnicast { .. } => Operation::RestoreUnicast { replacements },
        Operation::RestoreMulticastImporters { .. } => {
            Operation::RestoreMulticastImporters { replacements }
        }
        other => other.clone(),
    }
}

fn owner(owner_pid: NamespacePid, allocation: AllocationReference) -> AllocationOwner {
    AllocationOwner {
        allocation,
        owner_pid,
    }
}

fn reply(connection: UnixStream, namespace_pid: NamespacePid, result: Result<Reply, String>) {
    let response = Response {
        namespace_pid,
        result,
    };
    protocol::send(&connection, &response, None).unwrap();
}

/// Pass `directory` to the coordinator as descriptor `CHECKPOINT_FD` and return the path
/// that names it there, as the agent does.
fn inherit(command: &mut Command, directory: &OwnedFd) -> PathBuf {
    pass(command, directory, CHECKPOINT_FD);
    PathBuf::from(format!("/proc/self/fd/{CHECKPOINT_FD}"))
}

/// Pass `descriptor` to the coordinator as descriptor `number`.
fn pass(command: &mut Command, descriptor: &OwnedFd, number: RawFd) {
    // A number above every descriptor that the coordinator receives keeps one dup2 from
    // replacing the source of another.
    let descriptor = rustix::io::fcntl_dupfd_cloexec(descriptor, 64).unwrap();
    let duplicate = move || {
        // SAFETY: `target` only names the descriptor that dup2 replaces. It is forgotten,
        // so it never closes anything.
        let mut target = unsafe { OwnedFd::from_raw_fd(number) };
        // The coordinator inherits the descriptor only without close-on-exec.
        let result = rustix::io::dup2(&descriptor, &mut target)
            .and_then(|()| rustix::io::fcntl_setfd(&target, FdFlags::empty()));
        std::mem::forget(target);
        result.map_err(Into::into)
    };
    // SAFETY: The child calls only dup2 and fcntl, which are async-signal-safe, between
    // fork and exec.
    unsafe { command.pre_exec(duplicate) };
}

/// Coordinator process, killed on drop. A panic before `finish` returns also prints its stderr.
struct Coordinator(Child);

impl Coordinator {
    fn spawn(command: &mut Command, carrier_timeout: Duration) -> Self {
        let timeout = HANG_TIMEOUT.as_secs().to_string();
        let carrier_timeout = carrier_timeout.as_secs().to_string();
        Self(
            command
                .env("SNAPSHOT_CONTROL_TIMEOUT_SECONDS", &timeout)
                .env("SNAPSHOT_CARRIER_TIMEOUT_SECONDS", &carrier_timeout)
                .stdout(Stdio::null())
                .stderr(Stdio::piped())
                .spawn()
                .unwrap(),
        )
    }

    /// Wait for exit and return the exit status and stderr.
    fn finish(&mut self) -> (ExitStatus, String) {
        let deadline = Instant::now() + 2 * HANG_TIMEOUT;
        let status = loop {
            if let Some(status) = self.0.try_wait().unwrap() {
                break status;
            }
            assert!(Instant::now() < deadline, "coordinator did not exit");
            std::thread::sleep(Duration::from_millis(10));
        };
        let mut stderr = String::new();
        let mut pipe = self.0.stderr.take().unwrap();
        pipe.read_to_string(&mut stderr).unwrap();
        (status, stderr)
    }
}

impl Drop for Coordinator {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
        if std::thread::panicking()
            && let Some(mut pipe) = self.0.stderr.take()
        {
            let mut stderr = String::new();
            let _ = pipe.read_to_string(&mut stderr);
            eprintln!("coordinator stderr:\n{stderr}");
        }
    }
}

/// Participants 1 and 2 listen in a private socket directory and answer as scripted.
struct Harness {
    directory: tempfile::TempDir,
    state: PathBuf,
    listeners: BTreeMap<NamespacePid, UnixListener>,
    /// If set, coordinators reach the state's directory through this inherited descriptor
    /// instead of its path.
    inherited: Option<OwnedFd>,
    /// The coordinator's wait for a SaveAllocations or LoadAllocations reply.
    carrier_timeout: Duration,
    /// If set, the coordinator runs as the only relay of a group, through this agent.
    agent: Option<fake_agent::FakeAgent>,
}

impl Harness {
    fn new() -> Self {
        let directory = tempfile::tempdir().unwrap();
        let state = directory.path().join("cuinterpose.state");
        let listeners = [1, 2]
            .into_iter()
            .map(|pid| {
                let endpoint = protocol::socket_path(directory.path(), pid);
                (pid, UnixListener::bind(endpoint).unwrap())
            })
            .collect();
        Self {
            directory,
            state,
            listeners,
            inherited: None,
            carrier_timeout: HANG_TIMEOUT,
            agent: None,
        }
    }

    fn endpoint(&self, pid: NamespacePid) -> PathBuf {
        protocol::socket_path(self.directory.path(), pid)
    }

    fn participant(&self, pid: NamespacePid) -> String {
        format!("participant {pid} ({})", self.endpoint(pid).display())
    }

    fn assert_no_request(&self, message: &str, timeout: Duration) {
        let mut listeners: Vec<_> = self
            .listeners
            .values()
            .map(|listener| PollFd::new(listener, PollFlags::IN))
            .collect();
        let pending = poll(&mut listeners, Some(&Timespec::try_from(timeout).unwrap())).unwrap();
        assert_eq!(pending, 0, "{message}");
    }

    /// Run one action, which must succeed or fail with every diagnostic in `errors`.
    fn coordinator(&self, mode: &str, errors: &[&str], script: impl FnOnce(&mut Child)) {
        let mut command = Command::new(BINARY);
        command
            .args([mode, "--socket-dir"])
            .arg(self.directory.path())
            .args(["--process", "1", "--process", "2"]);
        if mode != "--inspect" {
            let checkpoint_dir = match &self.inherited {
                Some(directory) => inherit(&mut command, directory),
                None => self.state.parent().unwrap().to_owned(),
            };
            command.arg("--checkpoint-dir").arg(checkpoint_dir);
        }
        if let Some(agent) = &self.agent {
            pass(&mut command, &OwnedFd::from(agent.attach(0)), SESSION_FD);
            let deadline = std::time::SystemTime::now() + 2 * HANG_TIMEOUT;
            let deadline = deadline.duration_since(std::time::UNIX_EPOCH).unwrap();
            command
                .args(["--session-fd", &SESSION_FD.to_string()])
                .args(["--rank", "0", "--relay-ranks", "0"])
                .args([
                    "--round-timeout-seconds",
                    &HANG_TIMEOUT.as_secs().to_string(),
                ])
                .args(["--deadline", &deadline.as_secs().to_string()]);
        }
        // The coordinator runs in the workload's root, where TMPDIR may be missing or on
        // another filesystem, so publication must create its temporary file in the
        // checkpoint directory.
        command.env("TMPDIR", self.directory.path().join("missing"));
        let had_state = self.state.exists();
        let mut coordinator = Coordinator::spawn(&mut command, self.carrier_timeout);
        script(&mut coordinator.0);
        let (status, stderr) = coordinator.finish();
        // The coordinator has exited, so a retry or a later phase would already be queued.
        let unexpected = format!("request after the coordinator exited: {stderr}");
        self.assert_no_request(&unexpected, Duration::ZERO);
        if errors.is_empty() {
            assert!(status.success(), "{stderr}");
            return;
        }
        assert!(!status.success(), "{stderr}");
        for error in errors {
            assert!(stderr.contains(error), "{error:?} not in {stderr:?}");
        }
        if mode == "--prepare" {
            assert_eq!(
                self.state.exists(),
                had_state,
                "failed preparation changed state"
            );
        }
    }

    /// Accept participant `pid`'s next connection and check its request.
    fn request(&self, pid: NamespacePid, expected: Request) -> UnixStream {
        let timeout = HANG_TIMEOUT;
        let listener = &self.listeners[&pid];
        let mut pending = [PollFd::new(listener, PollFlags::IN)];
        let ready = poll(&mut pending, Some(&Timespec::try_from(timeout).unwrap())).unwrap();
        assert_eq!(ready, 1, "no {expected:?} request for {pid}");
        let (connection, _) = listener.accept().unwrap();
        connection.set_read_timeout(Some(timeout)).unwrap();
        connection.set_write_timeout(Some(timeout)).unwrap();
        let (request, _): (Request, _) = protocol::receive(&connection).unwrap();
        // Request has no PartialEq, and its derived Debug output shows every field.
        assert_eq!(format!("{request:?}"), format!("{expected:?}"));
        connection
    }

    fn execute(
        &self,
        pid: NamespacePid,
        operation: &Operation,
        owners: Vec<AllocationOwner>,
    ) -> UnixStream {
        let expected = Request::Execute {
            namespace_pid: pid,
            operation: addressed(operation, owners, &[]),
        };
        self.request(pid, expected)
    }

    /// Answer one Inspect or BeginCheckpoint request per participant, in PID order.
    fn answer(&self, kind: RecordRequest, participants: &[Vec<Record>; 2]) {
        for (pid, records) in (1..).zip(participants) {
            let connection = self.request(pid, kind.request(pid));
            reply(connection, pid, inspection(records.clone()));
        }
    }

    fn phase(&self, operation: &Operation) {
        self.phase_with(operation, [0, 0], &Owners::default());
    }

    /// Complete one phase, holding participant 1's reply until participant 2 replies.
    fn phase_with(&self, operation: &Operation, carrier: [u64; 2], owners: &Owners) {
        let [first, second] = owners.clone();
        // Accepting both requests before any reply detects serial dispatch.
        let held = self.execute(1, operation, first);
        let other = self.execute(2, operation, second);
        reply(other, 2, completed(operation, carrier[1]));
        self.assert_no_request(
            "advanced before every participant replied",
            Duration::from_millis(100),
        );
        reply(held, 1, completed(operation, carrier[0]));
    }

    fn prepare(&self, participants: &[Vec<Record>; 2], owners: &Owners, saved: [u64; 2]) {
        self.coordinator("--prepare", &[], |_| {
            self.answer(RecordRequest::BeginCheckpoint, participants);
            for operation in &PREPARE {
                let save = matches!(operation, Operation::SaveAllocations { .. });
                let carrier = if save { saved } else { [0, 0] };
                self.phase_with(operation, carrier, owners);
            }
        });
    }

    /// Restore `participants`, then report `restored` when inspected again.
    fn restore(
        &self,
        participants: &[Vec<Record>; 2],
        loaded: [u64; 2],
        restored: &[Vec<Record>; 2],
        errors: &[&str],
    ) {
        self.coordinator("--restore", errors, |_| {
            self.answer(RecordRequest::Inspect, participants);
            for operation in &RESTORE {
                let load = matches!(operation, Operation::LoadAllocations);
                let carrier = if load { loaded } else { [0, 0] };
                self.phase_with(operation, carrier, &Owners::default());
            }
            self.answer(RecordRequest::Inspect, restored);
        });
    }

    fn write_state(&self, participants: &[Vec<Record>; 2]) {
        let [first, second] = participants.clone();
        let manifest = Manifest::from([(1, first), (2, second)]);
        std::fs::write(&self.state, protocol::encode(&manifest).unwrap()).unwrap();
    }
}

// Command line.

#[test]
fn invalid_arguments_fail_without_contacting_participants() {
    let harness = Harness::new();
    let dir = harness.directory.path().to_str().unwrap();
    let cases: [(&[&str], &[&str]); 6] = [
        (
            &["--prepare", "--socket-dir", dir, "--process", "1"],
            &["required arguments", "--checkpoint-dir"],
        ),
        (
            &[
                "--inspect",
                "--prepare",
                "--socket-dir",
                dir,
                "--process",
                "1",
            ],
            &["cannot be used with", "--inspect", "--prepare"],
        ),
        (
            &[
                "--checkpoint-dir",
                "/tmp",
                "--socket-dir",
                dir,
                "--process",
                "1",
            ],
            &["an action is required"],
        ),
        (
            &["--inspect", "--socket-dir", "relative", "--process", "1"],
            &["--socket-dir must be an absolute path"],
        ),
        (
            &["--inspect", "--socket-dir", dir, "--process", "0"],
            &["invalid value '0'", "--process"],
        ),
        (
            &[
                "--inspect",
                "--socket-dir",
                dir,
                "--process",
                "1",
                "--process",
                "1",
            ],
            &["duplicate namespace PID"],
        ),
    ];
    for (args, errors) in cases {
        let (status, stderr) =
            Coordinator::spawn(Command::new(BINARY).args(args), HANG_TIMEOUT).finish();
        assert!(!status.success(), "{args:?}: {stderr}");
        for error in errors {
            assert!(
                stderr.contains(error),
                "{args:?}: {error:?} not in {stderr:?}"
            );
        }
        harness.assert_no_request("invalid arguments contacted a participant", Duration::ZERO);
    }
}

#[test]
fn prepare_fails_without_overwriting_existing_state_file() {
    let harness = Harness::new();
    std::fs::write(&harness.state, "previous checkpoint").unwrap();
    let error = format!("{} already exists", harness.state.display());
    harness.coordinator("--prepare", &[&error], |_| {});
    assert_eq!(
        std::fs::read(&harness.state).unwrap(),
        b"previous checkpoint"
    );
}

#[test]
fn prepare_and_restore_reach_checkpoint_directory_through_inherited_descriptor() {
    let mut harness = Harness::new();
    // Like the agent's os.Open, this opens the directory read-only and close-on-exec.
    harness.inherited = Some(File::open(harness.directory.path()).unwrap().into());
    let participants = [vec![allocation(), mapping()], vec![]];
    harness.prepare(&participants, &Owners::default(), [0, 0]);
    let published = std::fs::read(&harness.state).unwrap();
    let saved: Manifest = protocol::decode(&published).unwrap();
    assert_eq!(saved, (1..).zip(participants.clone()).collect::<Manifest>());
    harness.restore(&participants, [0, 0], &participants, &[]);
    let error = format!("/proc/self/fd/{CHECKPOINT_FD}/cuinterpose.state already exists");
    harness.coordinator("--prepare", &[&error], |_| {});
    assert_eq!(std::fs::read(&harness.state).unwrap(), published);
}

// Record requests.

#[test]
fn inspect_requests_records_from_every_participant() {
    let harness = Harness::new();
    harness.coordinator("--inspect", &[], |_| {
        let participants = [vec![shared(), mapping()], vec![shared()]];
        harness.answer(RecordRequest::Inspect, &participants);
    });
}

#[test]
fn invalid_topology_fails_inspect_and_prepare_before_any_phase() {
    let harness = Harness::new();
    let error = "participant 1: invalid mapping or mapping out of bounds";
    let participants = [
        vec![allocation(), with!(mapping(), Mapping.size = 8192)],
        vec![],
    ];
    for (mode, kind) in [
        ("--inspect", RecordRequest::Inspect),
        ("--prepare", RecordRequest::BeginCheckpoint),
    ] {
        harness.coordinator(mode, &[error], |_| harness.answer(kind, &participants));
    }
}

#[test]
fn record_request_error_or_wrong_reply_names_participant() {
    let harness = Harness::new();
    let cases = [
        (
            "--prepare",
            RecordRequest::BeginCheckpoint,
            Err("cannot freeze registry".to_owned()),
            "cannot freeze registry",
        ),
        (
            "--inspect",
            RecordRequest::Inspect,
            completed(&Operation::PrepareMulticast, 0),
            "expected inspection reply, actual Completed",
        ),
    ];
    for (mode, kind, result, detail) in cases {
        let error = format!("{kind:?}: {}: {detail}", harness.participant(2));
        harness.coordinator(mode, &[&error], |_| {
            reply(harness.request(1, kind.request(1)), 1, inspection(vec![]));
            reply(harness.request(2, kind.request(2)), 2, result);
        });
    }
}

#[test]
fn missing_participant_socket_fails_with_connect_error() {
    let mut harness = Harness::new();
    harness.listeners.remove(&2);
    std::fs::remove_file(harness.endpoint(2)).unwrap();
    let request = format!(
        "{}: Inspect {{ namespace_pid: 2 }}",
        harness.endpoint(2).display()
    );
    let error = format!(
        "Inspect: {}: {request}: connect failed",
        harness.participant(2)
    );
    harness.coordinator("--inspect", &[&error], |_| {
        let connection = harness.request(1, RecordRequest::Inspect.request(1));
        reply(connection, 1, inspection(vec![]));
    });
}

// Phase dispatch.

#[test]
fn participant_refusal_in_any_phase_names_that_phase() {
    let harness = Harness::new();
    for (mode, kind, operations) in [
        ("--prepare", RecordRequest::BeginCheckpoint, &PREPARE[..]),
        ("--restore", RecordRequest::Inspect, &RESTORE[..]),
    ] {
        for (index, operation) in operations.iter().enumerate() {
            if mode == "--restore" {
                harness.write_state(&[vec![], vec![]]);
            }
            let error = format!("{operation}: {}: refused", harness.participant(1));
            harness.coordinator(mode, &[&error], |_| {
                harness.answer(kind, &[vec![], vec![]]);
                for preceding in &operations[..index] {
                    harness.phase(preceding);
                }
                let [refused, other] = [1, 2].map(|pid| harness.execute(pid, operation, vec![]));
                reply(refused, 1, Err("refused".to_owned()));
                reply(other, 2, completed(operation, 0));
            });
        }
    }
}

#[test]
fn phase_waits_for_every_participant_and_reports_each_failure_without_retry() {
    let harness = Harness::new();
    let error = format!(
        "PrepareMulticast: {}: refused\n{}: ",
        harness.participant(1),
        harness.participant(2)
    );
    harness.coordinator("--prepare", &[&error, "receive failed"], |coordinator| {
        harness.answer(RecordRequest::BeginCheckpoint, &[vec![], vec![]]);
        let [refused, lost] =
            [1, 2].map(|pid| harness.execute(pid, &Operation::PrepareMulticast, vec![]));
        reply(refused, 1, Err("refused".to_owned()));
        std::thread::sleep(Duration::from_millis(100));
        assert!(
            coordinator.try_wait().unwrap().is_none(),
            "exited before every participant replied"
        );
        // Close without replying. A retry would still be queued after the coordinator exits.
        drop(lost);
    });
}

#[test]
fn silent_participant_fails_its_phase_at_the_carrier_timeout() {
    let mut harness = Harness::new();
    // Only the wait that must expire is short, so a slow host cannot fail another one.
    harness.carrier_timeout = Duration::from_secs(1);
    let error = format!("SaveAllocations: {}: ", harness.participant(1));
    let mut silent = None;
    // EAGAIN is the read timeout. A closed connection fails differently.
    harness.coordinator("--prepare", &[&error, "(os error 11)"], |_| {
        harness.answer(RecordRequest::BeginCheckpoint, &[vec![], vec![]]);
        let [multicast, save, _] = PREPARE;
        harness.phase(&multicast);
        let [held, other] = [1, 2].map(|pid| harness.execute(pid, &save, vec![]));
        reply(other, 2, completed(&save, 0));
        // Keep the connection open until the coordinator exits.
        silent = Some(held);
    });
    drop(silent);
}

#[test]
fn completion_reply_with_wrong_pid_step_or_byte_count_fails() {
    let harness = Harness::new();
    // Participant 1 alone holds a shared allocation, so it must save 4096 bytes.
    let participants = [vec![shared()], vec![]];
    let owners = [vec![owner(1, A)], vec![]];
    let [multicast, save, _] = PREPARE;
    let exchange = format!("PrepareMulticast: {}: ", harness.participant(1));
    let cases = [
        (
            &multicast,
            3,
            completed(&multicast, 0),
            vec![
                exchange.as_str(),
                "namespace PID changed: expected 1, actual 3",
            ],
        ),
        (
            &multicast,
            1,
            completed(&Operation::LoadAllocations, 0),
            vec!["participant 1: unexpected reply to PrepareMulticast: Loaded"],
        ),
        (
            &multicast,
            1,
            inspection(vec![]),
            vec!["participant 1: unexpected reply to PrepareMulticast: Inspection"],
        ),
        (
            &save,
            1,
            completed(&save, 0),
            vec!["participant 1: SaveAllocations copied 0 carrier bytes, expected 4096"],
        ),
    ];
    for (operation, namespace_pid, result, errors) in cases {
        harness.coordinator("--prepare", &errors, |_| {
            harness.answer(RecordRequest::BeginCheckpoint, &participants);
            for preceding in PREPARE.iter().take_while(|next| *next != operation) {
                harness.phase(preceding);
            }
            let [first, second] = owners.clone();
            let first = harness.execute(1, operation, first);
            let second = harness.execute(2, operation, second);
            reply(first, namespace_pid, result);
            reply(second, 2, completed(operation, 0));
        });
    }
}

#[test]
fn state_write_failure_after_prepare_sends_no_further_requests() {
    let mut harness = Harness::new();
    harness.state = harness.directory.path().join("missing/cuinterpose.state");
    let error = format!("publish checkpoint state {}", harness.state.display());
    harness.coordinator("--prepare", &[&error], |_| {
        harness.answer(RecordRequest::BeginCheckpoint, &[vec![], vec![]]);
        for operation in &PREPARE {
            harness.phase(operation);
        }
    });
}

#[test]
fn carrier_size_overflow_fails_before_any_phase() {
    let harness = Harness::new();
    let records = [(3, u64::MAX), (4, 1)].map(|(id, size)| {
        let allocation = AllocationReference {
            id: [id; 16],
            creator_pid: 2,
        };
        with!(
            shared(),
            Allocation.allocation = allocation,
            Allocation.size = size
        )
    });
    harness.coordinator(
        "--prepare",
        &["participant 2: carrier size overflow"],
        |_| {
            harness.answer(RecordRequest::BeginCheckpoint, &[vec![], records.to_vec()]);
        },
    );
}

// Carrier ownership.

#[test]
fn live_creator_saves_shared_backing_even_if_importer_has_lower_pid() {
    let harness = Harness::new();
    let holder = with!(shared(), Allocation.allocation = A2);
    let owners = [vec![owner(2, A2)], vec![owner(2, A2)]];
    harness.prepare(&[vec![holder.clone()], vec![holder]], &owners, [0, 4096]);
}

#[test]
fn importer_saves_entire_backing_after_creator_released_it() {
    let harness = Harness::new();
    // The creator has released every local reference, and the importer maps only part of
    // the backing.
    let imported = with!(
        shared(),
        Allocation.size = 8192,
        Allocation.handle_types = 0,
        Allocation.virtual_allocation_handle_count = 0
    );
    let partial = with!(mapping(), Mapping.address = 0x30000, Mapping.offset = 4096);
    let participants = [vec![], vec![imported, partial]];
    harness.prepare(&participants, &[vec![], vec![owner(2, A)]], [0, 8192]);
    // LoadAllocations sizes come from the saved state, not the live inspection.
    harness.restore(&[vec![], vec![]], [0, 8192], &participants, &[]);
}

// Restore.

#[test]
fn state_file_sorts_records_and_restore_ignores_record_order() {
    let harness = Harness::new();
    let other = AllocationReference {
        id: [3; 16],
        creator_pid: 2,
    };
    let other = with!(allocation(), Allocation.allocation = other);
    let unsorted = [vec![mapping(), allocation()], vec![other.clone()]];
    let sorted = [vec![allocation(), mapping()], vec![other]];
    harness.prepare(&unsorted, &Owners::default(), [0, 0]);
    let saved: Manifest = protocol::decode(&std::fs::read(&harness.state).unwrap()).unwrap();
    assert_eq!(saved, (1..).zip(sorted.clone()).collect::<Manifest>());
    // Verification sorts both the restored records and the saved state.
    harness.restore(&unsorted, [0, 0], &unsorted, &[]);
    harness.write_state(&unsorted);
    harness.restore(&unsorted, [0, 0], &sorted, &[]);
}

#[test]
fn restore_with_missing_corrupt_or_mismatched_state_contacts_no_participant() {
    let harness = Harness::new();
    let unparsable = format!("cannot parse {}", harness.state.display());
    let mismatched = "restored processes do not match the checkpointed participants";
    // The coordinator restores processes 1 and 2.
    let checkpointed = |pids: &[NamespacePid]| {
        let manifest: Manifest = pids.iter().map(|&pid| (pid, Vec::new())).collect();
        protocol::encode(&manifest).unwrap()
    };
    let cases: [(Option<Vec<u8>>, &str); 4] = [
        (None, &unparsable),
        (Some(b"not-cuinterpose-state\n".to_vec()), &unparsable),
        (Some(checkpointed(&[1])), mismatched),
        (Some(checkpointed(&[1, 2, 3])), mismatched),
    ];
    for (state, error) in cases {
        match state {
            Some(state) => std::fs::write(&harness.state, state).unwrap(),
            None => std::fs::remove_file(&harness.state).unwrap_or(()),
        }
        harness.coordinator("--restore", &[error], |_| {});
    }
}

#[test]
fn restore_validates_saved_and_live_topologies_before_loading() {
    let harness = Harness::new();
    let error = "participant 1: unsupported allocation properties";
    let valid = [vec![allocation()], vec![]];
    let invalid = [
        vec![with!(allocation(), Allocation.allocation_type = 0)],
        vec![],
    ];
    for (saved, live) in [(&invalid, &valid), (&valid, &invalid)] {
        harness.write_state(saved);
        harness.coordinator("--restore", &[error], |_| {
            harness.answer(RecordRequest::Inspect, live);
        });
    }
}

#[test]
fn restore_fails_when_restored_records_differ_from_checkpoint() {
    let harness = Harness::new();
    let checkpoint = [vec![allocation(), mapping()], vec![]];
    harness.write_state(&checkpoint);
    let moved = with!(mapping(), Mapping.address = 0x30000);
    for (restored, detail) in [
        (vec![allocation(), moved], "record 1: expected Mapping"),
        (vec![allocation()], "expected 2 records, actual 1 records"),
    ] {
        let error =
            format!("restored topology does not match the checkpoint for participant 1: {detail}");
        harness.restore(&checkpoint, [0, 0], &[restored, vec![]], &[&error]);
    }
}

// Peer carriers (#550).

#[test]
fn unexported_allocation_with_peer_access_is_saved_to_host_carrier() {
    let harness = Harness::new();
    // An unexported allocation that another GPU can access has one holder.
    let accessed = with!(
        allocation(),
        Allocation.peer_access_granted = true,
        Allocation.handle_types = 0
    );
    let participants = [vec![accessed, mapping()], vec![]];
    harness.prepare(&participants, &[vec![owner(1, A)], vec![]], [4096, 0]);
    harness.restore(&participants, [4096, 0], &participants, &[]);
}

// FABRIC sharing within one Pod.

#[test]
fn fabric_importer_imports_the_handle_that_its_creator_exported_at_restore() {
    let harness = Harness::new();
    let fabric = CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_FABRIC.0;
    let [old, new] = [1, 2].map(|byte| FabricHandle([byte; FABRIC_HANDLE_BYTES]));
    // A fabric import names no creator, and its size is unknown to the importer.
    let local = AllocationReference {
        id: [9; 16],
        creator_pid: 0,
    };
    let records = |handle| {
        [
            vec![
                with!(shared(), Allocation.handle_types = fabric),
                mapping(),
                Record::FabricExport {
                    allocation: A,
                    handle,
                },
            ],
            vec![
                with!(
                    shared(),
                    Allocation.allocation = local,
                    Allocation.handle_types = fabric,
                    Allocation.size = 0
                ),
                with!(mapping(), Mapping.allocation = local),
                Record::FabricImport {
                    allocation: local,
                    handle,
                },
            ],
        ]
    };
    let owners = [
        vec![owner(1, A)],
        vec![AllocationOwner {
            allocation: local,
            owner_pid: 1,
        }],
    ];
    harness.prepare(&records(old), &owners, [4096, 0]);
    let replacement = FabricReplacement { old, new };
    harness.coordinator("--restore", &[], |_| {
        harness.answer(RecordRequest::Inspect, &records(old));
        let [creator, importer] =
            [1, 2].map(|pid| harness.execute(pid, &Operation::LoadAllocations, vec![]));
        let loaded = Reply::Loaded {
            bytes: 4096,
            replacements: vec![replacement],
        };
        reply(creator, 1, Ok(loaded));
        reply(importer, 2, completed(&Operation::LoadAllocations, 0));
        for operation in &RESTORE[1..] {
            let operation = addressed(operation, Vec::new(), &[replacement]);
            for pid in [1, 2] {
                let request = Request::Execute {
                    namespace_pid: pid,
                    operation: operation.clone(),
                };
                reply(harness.request(pid, request), pid, completed(&operation, 0));
            }
        }
        harness.answer(RecordRequest::Inspect, &records(new));
    });
}

// Relay mode.

#[test]
fn relay_alone_in_its_group_runs_every_mode_through_the_session() {
    let mut harness = Harness::new();
    harness.agent = Some(fake_agent::FakeAgent::default());
    let participants = [vec![shared(), mapping()], vec![shared()]];
    harness.coordinator("--inspect", &[], |_| {
        harness.answer(RecordRequest::Inspect, &participants);
    });
    let owners = [vec![owner(1, A)], vec![owner(1, A)]];
    harness.prepare(&participants, &owners, [4096, 0]);
    harness.restore(&participants, [4096, 0], &participants, &[]);
    let agent = harness.agent.as_ref().unwrap();
    assert!(agent.fails().is_empty());
    assert_eq!(
        agent.keys(),
        ["inspect.2.cmd", "prepare.5.cmd", "restore.9.cmd"]
    );
}

#[test]
fn relay_refusal_fails_the_session_before_any_phase() {
    let mut harness = Harness::new();
    harness.agent = Some(fake_agent::FakeAgent::default());
    let error = "participant 1: invalid mapping or mapping out of bounds";
    let participants = [
        vec![allocation(), with!(mapping(), Mapping.size = 8192)],
        vec![],
    ];
    harness.coordinator("--prepare", &[error], |_| {
        harness.answer(RecordRequest::BeginCheckpoint, &participants);
    });
    let agent = harness.agent.as_ref().unwrap();
    assert_eq!(agent.fails(), [(0, "Refused".to_owned())]);
}

#[test]
fn session_arguments_are_all_or_nothing() {
    let harness = Harness::new();
    let dir = harness.directory.path().to_str().unwrap();
    let inspect = ["--inspect", "--socket-dir", dir, "--process", "1"];
    let session = [
        "--session-fd",
        "0",
        "--rank",
        "1",
        "--relay-ranks",
        "0,1",
        "--round-timeout-seconds",
        "60",
        "--deadline",
        "1",
    ];
    let cases: [(Vec<&str>, &[&str]); 4] = [
        ([&inspect[..], &["--rank", "1"]].concat(), &["--session-fd"]),
        (
            [&inspect[..], &session[..2]].concat(),
            &["required arguments", "--rank"],
        ),
        (
            [
                &inspect[..],
                &session[..4],
                &["--relay-ranks", "0,2"],
                &session[6..],
            ]
            .concat(),
            &["--relay-ranks does not contain --rank 1"],
        ),
        // Descriptor 0 is the coordinator's standard input, which is not a socket.
        (
            [&inspect[..], &session[..]].concat(),
            &["--session-fd 0 is not a socket"],
        ),
    ];
    for (args, errors) in cases {
        let (status, stderr) = Coordinator::spawn(
            Command::new(BINARY).args(&args).stdin(Stdio::null()),
            HANG_TIMEOUT,
        )
        .finish();
        assert!(!status.success(), "{args:?}: {stderr}");
        for error in errors {
            assert!(
                stderr.contains(error),
                "{args:?}: {error:?} not in {stderr:?}"
            );
        }
        harness.assert_no_request("invalid arguments contacted a participant", Duration::ZERO);
    }
}
