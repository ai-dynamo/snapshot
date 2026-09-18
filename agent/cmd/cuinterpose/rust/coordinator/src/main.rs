// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Short-lived participant validation, global CUDA lifecycle barriers, and state publication.

mod report;
mod state;
mod topology;

use anyhow::{Context, Result, bail, ensure};
use clap::Parser;
use cuinterpose_protocol::{
    self as protocol, Manifest, Operation, ParticipantDirectory, ParticipantId, ParticipantState,
    Reply, Request, Response,
};
use report::{Event, Transfer, write as report};
use std::collections::BTreeMap;
use std::os::unix::net::{SocketAddr, UnixStream};
use std::path::{Path, PathBuf};
use std::time::Instant;
use topology::Allocation;

#[derive(Parser)]
struct Arguments {
    #[arg(long, group = "action")]
    prepare: bool,
    #[arg(long, group = "action")]
    restore: bool,
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

struct Peer {
    endpoint: PathBuf,
    socket_path: PathBuf,
    id: ParticipantId,
}

struct Inspection {
    participant: ParticipantState,
    raw_imports: u64,
    unsupported_creations: u64,
}

// Transport errors retain their cause; remote refusals are application errors.
fn exchange(endpoint: &Path, request: &Request) -> Result<Response> {
    let display = endpoint.display();
    let socket = UnixStream::connect(endpoint)
        .with_context(|| format!("{display}: {request:?}: connect failed"))?;
    let operation = match request {
        Request::Execute { operation, .. } => Some(*operation),
        _ => None,
    };
    let timeout = Some(protocol::timeout(operation));
    socket
        .set_read_timeout(timeout)
        .with_context(|| format!("{display}: {request:?}: set read timeout failed"))?;
    socket
        .set_write_timeout(timeout)
        .with_context(|| format!("{display}: {request:?}: set write timeout failed"))?;
    protocol::send(&socket, request, None)
        .with_context(|| format!("{display}: {request:?}: send failed"))?;
    let (response, fd): (Response, _) = protocol::receive(&socket)
        .with_context(|| format!("{display}: {request:?}: receive failed"))?;
    ensure!(
        fd.is_none(),
        "{display}: {request:?}: unexpected descriptor"
    );
    Ok(response)
}

impl Peer {
    fn identify(endpoint: PathBuf, socket_path: PathBuf) -> Result<Self> {
        let response = exchange(&endpoint, &Request::Identify)?;
        ensure!(
            matches!(
                response.result.map_err(anyhow::Error::msg)?,
                Reply::Identified
            ),
            "{}: unexpected identify response",
            endpoint.display()
        );
        Ok(Self {
            endpoint,
            socket_path,
            id: response.participant,
        })
    }

    fn rendezvous(&self, participants: &ParticipantDirectory) -> Result<()> {
        let response = exchange(
            &self.endpoint,
            &Request::Rendezvous {
                participant: self.id,
                participants: participants.clone(),
            },
        )?;
        ensure!(
            response.participant == self.id
                && matches!(response.result.map_err(anyhow::Error::msg)?, Reply::Ready),
            "{}: unexpected rendezvous response",
            self.endpoint.display()
        );
        Ok(())
    }

    fn inspect(&self) -> Result<Inspection> {
        let response = exchange(
            &self.endpoint,
            &Request::Inspect {
                participant: self.id,
            },
        )?;
        ensure!(
            response.participant == self.id,
            "{}: participant changed",
            self.endpoint.display()
        );
        match response.result.map_err(anyhow::Error::msg)? {
            Reply::Inspection {
                entries,
                live_raw_imports,
                unsupported_creations,
            } => Ok(Inspection {
                participant: ParticipantState {
                    socket_path: self.socket_path.clone(),
                    entries,
                },
                raw_imports: live_raw_imports,
                unsupported_creations,
            }),
            _ => bail!("{}: unexpected inspection reply", self.endpoint.display()),
        }
    }

    fn execute(&self, operation: Operation, expected_bytes: u64) -> Result<u32> {
        let response = exchange(
            &self.endpoint,
            &Request::Execute {
                participant: self.id,
                operation,
            },
        )?;
        ensure!(
            response.participant == self.id,
            "{}: participant changed",
            self.endpoint.display()
        );
        match response.result.map_err(anyhow::Error::msg)? {
            Reply::Completed {
                operation: actual,
                bytes,
                copy_us,
            } if actual == operation && bytes == expected_bytes => Ok(copy_us),
            _ => bail!(
                "{}: unexpected {operation:?} response or transfer size",
                self.endpoint.display()
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
) -> Result<u32> {
    std::thread::scope(|scope| {
        let mut jobs = Vec::with_capacity(peers.len());
        for peer in peers {
            let bytes = allocations
                .iter()
                .filter(|a| a.preserve_content && a.reference.creator == peer.id)
                .try_fold(0u64, |sum, a| {
                    sum.checked_add(a.size).context("allocation size overflow")
                })?;
            jobs.push(
                std::thread::Builder::new()
                    .spawn_scoped(scope, move || peer.execute(operation, bytes))?,
            );
        }
        let mut longest = 0;
        let mut failure = None;
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

fn inspect(peers: &[Peer]) -> Result<(Manifest, u64, u64)> {
    let mut participants = BTreeMap::new();
    let (mut raw, mut unsupported) = (0, 0);
    for peer in peers {
        let inspection = peer.inspect()?;
        ensure!(
            participants
                .insert(peer.id, inspection.participant)
                .is_none(),
            "duplicate participant identity"
        );
        raw += inspection.raw_imports;
        unsupported += inspection.unsupported_creations;
    }
    Ok((participants, raw, unsupported))
}

fn transfer(peers: &mut [Peer], operation: Operation, allocations: &[Allocation]) -> Result<()> {
    let start = Instant::now();
    let copy_us = command_all(peers, operation, allocations).context("allocation transfer")?;
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
    ensure!(args.prepare || args.restore, "an action is required");
    ensure!(
        args.control_dir.starts_with('/'),
        "--control-dir must be an absolute path"
    );
    let path = args.checkpoint_dir.join("cuinterpose.state");
    let mut expected = if args.prepare {
        Manifest::new()
    } else {
        state::read(&path).with_context(|| format!("cannot parse {}", path.display()))?
    };
    let start = Instant::now();
    let mut peers = Vec::with_capacity(args.processes.len() / 2);
    for process in args.processes.chunks_exact(2) {
        let (observed, namespace) = (process[0], process[1]);
        let control = &args.control_dir;
        let socket_path = PathBuf::from(format!("{control}/cuinterpose-{namespace}.sock"));
        let endpoint = if args.proc_root.is_empty() {
            socket_path.clone()
        } else {
            PathBuf::from(format!(
                "{}/{observed}/root{control}/cuinterpose-{namespace}.sock",
                args.proc_root
            ))
        };
        SocketAddr::from_pathname(&endpoint)?;
        peers.push(Peer::identify(endpoint, socket_path)?);
    }
    let directory: ParticipantDirectory = peers
        .iter()
        .map(|peer| (peer.id, peer.socket_path.clone()))
        .collect();
    ensure!(
        directory.len() == peers.len(),
        "duplicate participant identity"
    );
    if args.restore {
        ensure!(
            directory.keys().eq(expected.keys()),
            "restored processes do not match the checkpointed participants"
        );
    }
    for peer in &peers {
        peer.rendezvous(&directory)?;
    }
    report(Event::Rendezvous, start, peers.len())?;
    if args.prepare {
        let (mut participants, raw, unsupported) = inspect(&peers)?;
        report(
            Event::Inspect {
                entries: participants.values().map(|p| p.entries.len()).sum(),
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
        command_all(&mut peers, Operation::PrepareMulticast, &[]).context("multicast teardown")?;
        report(Event::PrepareMulticast, start, peers.len())?;
        transfer(&mut peers, Operation::SaveAllocations, &allocations)?;
        let start = Instant::now();
        command_all(&mut peers, Operation::PrepareUnicast, &[])?;
        report(Event::PrepareUnicast, start, peers.len())?;
        let start = Instant::now();
        state::write_atomic(&path, &mut participants)?;
        report(Event::StateWrite, start, peers.len())?;
    } else {
        let allocations = topology::validate(&expected)?;
        transfer(&mut peers, Operation::LoadAllocations, &allocations)?;
        let start = Instant::now();
        command_all(&mut peers, Operation::RestoreUnicast, &[])?;
        report(Event::RestoreUnicast, start, peers.len())?;
        let start = Instant::now();
        for operation in [
            Operation::RestoreMulticastCreators,
            Operation::RestoreMulticastImporters,
            Operation::RestoreMulticastDevices,
            Operation::RestoreMulticastBindings,
        ] {
            command_all(&mut peers, operation, &[])?;
        }
        report(Event::RestoreMulticast, start, peers.len())?;
        let start = Instant::now();
        // Re-identify before inspecting, preserving the final identity barrier.
        for peer in &peers {
            ensure!(
                Peer::identify(peer.endpoint.clone(), peer.socket_path.clone())?.id == peer.id,
                "restored participant changed"
            );
        }
        let (mut participants, raw, unsupported) = inspect(&peers)?;
        ensure!(
            raw == 0 && unsupported == 0,
            "restored participant has unsupported CUDA state"
        );
        topology::validate(&participants)?;
        for (id, actual) in &mut participants {
            let expected = expected
                .get_mut(id)
                .context("restored participant is not in the manifest")?;
            actual.entries.sort();
            expected.entries.sort();
            ensure!(
                actual.entries == expected.entries,
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
