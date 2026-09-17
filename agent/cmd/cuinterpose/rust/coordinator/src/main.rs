// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Short-lived participant validation, global CUDA lifecycle barriers, and state publication.

mod report;
mod state;
mod topology;

use anyhow::{Context, Result, bail, ensure};
use clap::Parser;
use cuinterpose_protocol::{
    self as protocol, ContentStorage, Operation, Participant, ParticipantId, Reply, Request,
    Response,
};
use report::{Event, Transfer, write as report};
use std::collections::BTreeMap;
use std::io::{Read, Write};
use std::net::Shutdown;
use std::os::fd::{FromRawFd, OwnedFd};
use std::os::unix::net::{SocketAddr, UnixStream};
use std::path::PathBuf;
use std::time::Instant;
use topology::Allocation;

#[derive(Parser)]
struct Arguments {
    #[arg(long, group = "action")]
    prepare: bool,
    #[arg(long, group = "action")]
    restore: bool,
    #[arg(long, group = "action")]
    identify: bool,
    #[arg(long, group = "action")]
    state_participants: bool,
    #[arg(long, value_parser = ["host-carrier", "pagebroker"], default_value = "host-carrier")]
    content_storage: String,
    #[arg(long, num_args = 2, action = clap::ArgAction::Append)]
    allocation_session: Vec<String>,
    /// Agent-owned socket: preflight acknowledgment, then native-ready observed PIDs.
    #[arg(long, requires = "restore")]
    restore_ready_fd: Option<i32>,
    #[arg(long)]
    proc_root: String,
    #[arg(long)]
    checkpoint_dir: PathBuf,
    #[arg(long)]
    control_dir: String,
    #[arg(long = "process", required = true, num_args = 2, action = clap::ArgAction::Append,
          value_parser = clap::value_parser!(i32).range(1..))]
    processes: Vec<i32>,
}

fn unix_ms() -> u128 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
}

struct Peer {
    pid: i32,
    endpoint: String,
    id: ParticipantId,
    session: Option<OwnedFd>,
    storage: ContentStorage,
}

struct Inspection {
    participant: Participant,
    raw_imports: u64,
    unsupported_creations: u64,
}

// Transport errors retain their cause; remote refusals are application errors.
fn exchange(endpoint: &str, request: &Request, descriptor: Option<&OwnedFd>) -> Result<Response> {
    let socket = UnixStream::connect(endpoint)
        .with_context(|| format!("{endpoint}: {request:?}: connect failed"))?;
    let operation = match request {
        Request::Execute { operation, .. } => Some(*operation),
        _ => None,
    };
    let timeout = Some(protocol::timeout(operation));
    socket
        .set_read_timeout(timeout)
        .with_context(|| format!("{endpoint}: {request:?}: set read timeout failed"))?;
    socket
        .set_write_timeout(timeout)
        .with_context(|| format!("{endpoint}: {request:?}: set write timeout failed"))?;
    protocol::send(&socket, request, descriptor)
        .with_context(|| format!("{endpoint}: {request:?}: send failed"))?;
    let (response, fd): (Response, _) = protocol::receive(&socket)
        .with_context(|| format!("{endpoint}: {request:?}: receive failed"))?;
    ensure!(
        fd.is_none(),
        "{endpoint}: {request:?}: unexpected descriptor"
    );
    Ok(response)
}

impl Peer {
    fn identify(endpoint: String) -> Result<Self> {
        let response = exchange(&endpoint, &Request::Handshake, None)?;
        ensure!(
            matches!(
                response.result.map_err(anyhow::Error::msg)?,
                Reply::Handshake
            ),
            "{endpoint}: unexpected handshake response"
        );
        Ok(Self {
            pid: 0,
            endpoint,
            id: response.participant,
            session: None,
            storage: ContentStorage::HostCarrier,
        })
    }

    fn inspect(&self) -> Result<Inspection> {
        let response = exchange(
            &self.endpoint,
            &Request::Inspect {
                participant: self.id,
            },
            None,
        )?;
        ensure!(
            response.participant == self.id,
            "{}: participant changed",
            self.endpoint
        );
        match response.result.map_err(anyhow::Error::msg)? {
            Reply::Inspection {
                records,
                live_raw_imports,
                unsupported_creations,
            } => Ok(Inspection {
                participant: Participant {
                    id: self.id,
                    records,
                },
                raw_imports: live_raw_imports,
                unsupported_creations,
            }),
            _ => bail!("{}: unexpected inspection reply", self.endpoint),
        }
    }

    fn execute(
        &self,
        operation: Operation,
        expected_bytes: u64,
        session: Option<OwnedFd>,
    ) -> Result<u32> {
        let response = exchange(
            &self.endpoint,
            &Request::Execute {
                participant: self.id,
                operation,
                content_storage: self.storage,
            },
            session.as_ref(),
        )?;
        ensure!(
            response.participant == self.id,
            "{}: participant changed",
            self.endpoint
        );
        match response.result.map_err(anyhow::Error::msg)? {
            Reply::Completed {
                operation: actual,
                bytes,
                copy_us,
            } if actual == operation && bytes == expected_bytes => Ok(copy_us),
            _ => bail!(
                "{}: unexpected {operation:?} response or transfer size",
                self.endpoint
            ),
        }
    }
}

/// Join every started exchange, including when one participant fails. A phase
/// cannot advance until every rank has replied; a bounded worker pool is unsafe.
fn command_all(
    peers: &mut [Peer],
    operation: Operation,
    allocations: &[Allocation],
    mut ready: Option<UnixStream>,
) -> Result<u32> {
    std::thread::scope(|scope| {
        let mut jobs = Vec::with_capacity(peers.len());
        let count = peers.len();
        let mut pending: BTreeMap<_, _> = peers.iter_mut().map(|peer| (peer.pid, peer)).collect();
        ensure!(pending.len() == count, "duplicate process PID");
        let schedule = (|| -> Result<()> {
            if let Some(socket) = &mut ready {
                // All identities, topology, and session capabilities were checked
                // before allowing the first native restore.
                socket.write_all(&0i32.to_be_bytes())?;
            }
            while !pending.is_empty() {
                let peer = if let Some(socket) = &mut ready {
                    let mut pid = [0; 4];
                    socket
                        .read_exact(&mut pid)
                        .context("native restore readiness ended early")?;
                    pending
                        .remove(&i32::from_be_bytes(pid))
                        .context("unknown or duplicate ready PID")?
                } else {
                    pending.pop_first().expect("pending is nonempty").1
                };
                let session = if matches!(
                    operation,
                    Operation::SaveAllocations | Operation::LoadAllocations
                ) {
                    peer.session.take()
                } else {
                    None
                };
                let bytes = allocations
                    .iter()
                    .filter(|a| a.preserve_content && a.creator == peer.id)
                    .try_fold(0u64, |sum, a| {
                        sum.checked_add(a.size).context("allocation size overflow")
                    })?;
                let cancel = ready.as_ref().map(UnixStream::try_clone).transpose()?;
                jobs.push(std::thread::Builder::new().spawn_scoped(scope, move || {
                    let start = Instant::now();
                    if operation == Operation::LoadAllocations {
                        report(
                            Event::RankLoadStarted {
                                pid: peer.pid,
                                unix_ms: unix_ms(),
                            },
                            start,
                            1,
                        )?;
                    }
                    let result = peer.execute(operation, bytes, session);
                    if result.is_err() {
                        // Wake readiness scheduling and the agent immediately;
                        // scoped threads still drain before topology can advance.
                        if let Some(socket) = cancel {
                            let _ = socket.shutdown(Shutdown::Both);
                        }
                    }
                    if operation == Operation::LoadAllocations {
                        report(
                            Event::RankLoadFinished {
                                pid: peer.pid,
                                unix_ms: unix_ms(),
                                succeeded: result.is_ok(),
                            },
                            start,
                            1,
                        )?;
                    }
                    result
                })?);
            }
            if let Some(socket) = &mut ready {
                let mut extra = [0];
                ensure!(
                    socket.read(&mut extra)? == 0,
                    "unexpected extra readiness message"
                );
            }
            Ok(())
        })();
        let mut longest = 0;
        let mut failure = schedule.err();
        for job in jobs {
            match job.join() {
                Ok(Ok(copy_us)) => longest = longest.max(copy_us),
                Ok(Err(error)) => {
                    failure.get_or_insert(error);
                }
                Err(_) => {
                    failure.get_or_insert_with(|| anyhow::anyhow!("participant exchange panicked"));
                }
            }
        }
        match failure {
            Some(error) => Err(error),
            None => Ok(longest),
        }
    })
}

fn inspect(peers: &[Peer]) -> Result<(Vec<Participant>, u64, u64)> {
    let mut participants = Vec::with_capacity(peers.len());
    let (mut raw, mut unsupported) = (0, 0);
    for peer in peers {
        let inspection = peer.inspect()?;
        participants.push(inspection.participant);
        raw += inspection.raw_imports;
        unsupported += inspection.unsupported_creations;
    }
    Ok((participants, raw, unsupported))
}

fn transfer(
    peers: &mut [Peer],
    operation: Operation,
    allocations: &[Allocation],
    ready: Option<UnixStream>,
) -> Result<()> {
    let start = Instant::now();
    let copy_us =
        command_all(peers, operation, allocations, ready).context("allocation transfer")?;
    let (count, bytes) =
        allocations
            .iter()
            .filter(|a| a.preserve_content)
            .try_fold((0, 0u64), |(n, sum), a| {
                Ok::<_, anyhow::Error>((
                    n + 1,
                    sum.checked_add(a.size)
                        .context("allocation size overflow")?,
                ))
            })?;
    let metrics = Transfer {
        allocation_count: count,
        allocation_bytes: bytes,
        gb_per_s: bytes as f64 / 1e9 / start.elapsed().as_secs_f64(),
        copy_gb_per_s: if copy_us == 0 {
            0.0
        } else {
            bytes as f64 / 1000.0 / f64::from(copy_us)
        },
    };
    let event = match operation {
        Operation::SaveAllocations => Event::SaveAllocations(metrics),
        Operation::LoadAllocations => Event::LoadAllocations(metrics),
        _ => bail!("not an allocation transfer"),
    };
    report(event, start, peers.len())?;
    Ok(())
}

fn run() -> Result<()> {
    let args = Arguments::parse();
    ensure!(
        args.prepare || args.restore || args.identify || args.state_participants,
        "an action is required"
    );
    ensure!(
        args.control_dir.starts_with('/'),
        "--control-dir must be an absolute path"
    );
    let path = args.checkpoint_dir.join("cuinterpose.state");
    let mut expected = if args.prepare || args.identify {
        Vec::new()
    } else {
        state::read(&path).with_context(|| format!("cannot parse {}", path.display()))?
    };
    if args.state_participants {
        topology::validate(&expected)?;
        let ids: Vec<_> = expected
            .iter()
            .map(|participant| participant.id.to_string())
            .collect();
        serde_json::to_writer(std::io::stdout().lock(), &ids)?;
        return Ok(());
    }
    let start = Instant::now();
    let mut peers = Vec::with_capacity(args.processes.len() / 2);
    for process in args.processes.chunks_exact(2) {
        let (observed, namespace) = (process[0], process[1]);
        let control = &args.control_dir;
        let endpoint = if args.proc_root.is_empty() {
            format!("{control}/cuinterpose-{namespace}.sock")
        } else {
            format!(
                "{}/{observed}/root{control}/cuinterpose-{namespace}.sock",
                args.proc_root
            )
        };
        SocketAddr::from_pathname(&endpoint)?;
        let mut peer = Peer::identify(endpoint)?;
        peer.pid = observed;
        peers.push(peer);
    }
    if args.identify {
        let (participants, raw, unsupported) = inspect(&peers)?;
        ensure!(
            raw == 0 && unsupported == 0,
            "unsupported CUDA sharing state"
        );
        topology::validate(&participants)?;
        let ids: Vec<_> = peers.iter().map(|peer| peer.id.to_string()).collect();
        serde_json::to_writer(std::io::stdout().lock(), &ids)?;
        return Ok(());
    }
    let storage = if args.content_storage == "pagebroker" {
        ContentStorage::Pagebroker
    } else {
        ContentStorage::HostCarrier
    };
    let mut sessions = BTreeMap::new();
    let mut seen_fds = std::collections::BTreeSet::new();
    let ready = if let Some(fd) = args.restore_ready_fd {
        ensure!(
            storage == ContentStorage::Pagebroker && fd >= 3,
            "readiness requires PageBroker restore"
        );
        seen_fds.insert(fd);
        // The agent explicitly passes this socket, disjoint from session FDs.
        let socket = unsafe { UnixStream::from_raw_fd(fd) };
        rustix::io::fcntl_setfd(&socket, rustix::io::FdFlags::CLOEXEC)?;
        socket.set_read_timeout(Some(protocol::timeout(Some(Operation::LoadAllocations))))?;
        socket.set_write_timeout(Some(protocol::timeout(None)))?;
        Some(socket)
    } else {
        None
    };
    for pair in args.allocation_session.chunks_exact(2) {
        let id: ParticipantId = pair[0].parse()?;
        let fd: i32 = pair[1].parse()?;
        ensure!(
            fd >= 3 && seen_fds.insert(fd) && !sessions.contains_key(&id),
            "duplicate or invalid session capability"
        );
        // These descriptors are explicitly inherited from the trusted agent.
        let descriptor = unsafe { OwnedFd::from_raw_fd(fd) };
        rustix::io::fcntl_setfd(&descriptor, rustix::io::FdFlags::CLOEXEC)?;
        sessions.insert(id, descriptor);
    }
    for peer in &mut peers {
        peer.storage = storage;
        peer.session = sessions.remove(&peer.id);
        ensure!(
            peer.session.is_some() == (storage == ContentStorage::Pagebroker),
            "missing or unexpected allocation session for {}",
            peer.id
        );
    }
    ensure!(
        sessions.is_empty(),
        "allocation sessions do not match participants"
    );
    if args.prepare {
        let (mut participants, raw, unsupported) = inspect(&peers)?;
        report(
            Event::Inspect {
                records: participants.iter().map(|p| p.records.len()).sum(),
                live_raw_imports: raw,
                unsupported_exportable_creations: unsupported,
            },
            start,
            peers.len(),
        )?;
        ensure!(
            raw == 0,
            "prepare refused: participants hold {raw} live raw imports"
        );
        ensure!(
            unsupported == 0,
            "prepare refused: participants created {unsupported} CUDA resources with unsupported exportable handle types"
        );
        let start = Instant::now();
        let allocations = topology::validate(&participants)?;
        report(Event::Validate, start, peers.len())?;
        let start = Instant::now();
        command_all(&mut peers, Operation::PrepareMulticast, &[], None)
            .context("multicast teardown")?;
        report(Event::PrepareMulticast, start, peers.len())?;
        transfer(&mut peers, Operation::SaveAllocations, &allocations, None)?;
        let start = Instant::now();
        command_all(&mut peers, Operation::PrepareUnicast, &[], None)?;
        report(Event::PrepareUnicast, start, peers.len())?;
        let start = Instant::now();
        state::write_atomic(&path, &mut participants)?;
        report(Event::StateWrite, start, peers.len())?;
    } else {
        peers.sort_by_key(|p| p.id);
        expected.sort_by_key(|p| p.id);
        ensure!(
            peers.iter().map(|p| p.id).eq(expected.iter().map(|p| p.id)),
            "restored processes do not match the checkpointed participants"
        );
        let allocations = topology::validate(&expected)?;
        report(Event::Handshake, start, peers.len())?;
        transfer(&mut peers, Operation::LoadAllocations, &allocations, ready)?;
        let start = Instant::now();
        command_all(&mut peers, Operation::RestoreUnicast, &[], None)?;
        report(Event::RestoreUnicast, start, peers.len())?;
        let start = Instant::now();
        for operation in [
            Operation::RestoreMulticastCreators,
            Operation::RestoreMulticastImporters,
            Operation::RestoreMulticastDevices,
            Operation::RestoreMulticastBindings,
        ] {
            command_all(&mut peers, operation, &[], None)?;
        }
        report(Event::RestoreMulticast, start, peers.len())?;
        let start = Instant::now();
        // Re-identify before inspecting, preserving the final identity barrier.
        for peer in &peers {
            ensure!(
                Peer::identify(peer.endpoint.clone())?.id == peer.id,
                "restored participant changed"
            );
        }
        let (mut participants, raw, unsupported) = inspect(&peers)?;
        ensure!(
            raw == 0 && unsupported == 0,
            "restored participant has unsupported CUDA state"
        );
        topology::validate(&participants)?;
        for (actual, expected) in participants.iter_mut().zip(&mut expected) {
            actual.records.sort();
            expected.records.sort();
            ensure!(
                actual.id == expected.id && actual.records == expected.records,
                "restored topology does not match the checkpoint"
            );
        }
        report(Event::Validate, start, peers.len())?;
    }
    Ok(())
}

fn main() -> std::process::ExitCode {
    match run() {
        Ok(()) => std::process::ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("cuinterpose-coordinator: {error:#}");
            std::process::ExitCode::FAILURE
        }
    }
}
