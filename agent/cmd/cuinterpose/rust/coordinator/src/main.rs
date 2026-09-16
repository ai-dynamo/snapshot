// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Short-lived participant validation, global CUDA lifecycle barriers, and state publication.

mod report;
mod state;
mod topology;

use anyhow::{Context, Result, bail, ensure};
use clap::Parser;
use cuinterpose_protocol::{
    self as protocol, Operation, Participant, ParticipantId, Reply, Request, Response,
};
use report::{Event, Transfer, write as report};
use std::os::unix::net::{SocketAddr, UnixStream};
use std::path::PathBuf;
use std::time::Instant;
use topology::Allocation;

#[derive(Parser)]
struct Arguments {
    #[arg(long, required_unless_present = "restore", conflicts_with = "restore")]
    prepare: bool,
    #[arg(long)]
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
    endpoint: String,
    id: ParticipantId,
}

struct Inspection {
    participant: Participant,
    raw_imports: u64,
    unsupported_creations: u64,
}

// Transport errors retain their cause; remote refusals are application errors.
fn exchange(endpoint: &str, request: &Request) -> Result<Response> {
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
    protocol::send(&socket, request, None)
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
        let response = exchange(&endpoint, &Request::Handshake)?;
        ensure!(
            matches!(
                response.result.map_err(anyhow::Error::msg)?,
                Reply::Handshake
            ),
            "{endpoint}: unexpected handshake response"
        );
        Ok(Self {
            endpoint,
            id: response.participant,
        })
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
fn command_all(peers: &[Peer], operation: Operation, allocations: &[Allocation]) -> Result<u32> {
    std::thread::scope(|scope| {
        let mut jobs = Vec::with_capacity(peers.len());
        for peer in peers {
            let bytes = allocations
                .iter()
                .filter(|a| a.preserve_content && a.creator == peer.id)
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

fn transfer(peers: &[Peer], operation: Operation, allocations: &[Allocation]) -> Result<()> {
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
    ensure!(
        args.control_dir.starts_with('/'),
        "--control-dir must be an absolute path"
    );
    let path = args.checkpoint_dir.join("cuinterpose.state");
    let mut expected = if args.prepare {
        Vec::new()
    } else {
        state::read(&path).with_context(|| format!("cannot parse {}", path.display()))?
    };
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
        peers.push(Peer::identify(endpoint)?);
    }
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
        command_all(&peers, Operation::PrepareMulticast, &[]).context("multicast teardown")?;
        report(Event::PrepareMulticast, start, peers.len())?;
        transfer(&peers, Operation::SaveAllocations, &allocations)?;
        let start = Instant::now();
        command_all(&peers, Operation::PrepareUnicast, &[])?;
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
        transfer(&peers, Operation::LoadAllocations, &allocations)?;
        let start = Instant::now();
        command_all(&peers, Operation::RestoreUnicast, &[])?;
        report(Event::RestoreUnicast, start, peers.len())?;
        let start = Instant::now();
        for operation in [
            Operation::RestoreMulticastCreators,
            Operation::RestoreMulticastImporters,
            Operation::RestoreMulticastDevices,
            Operation::RestoreMulticastBindings,
        ] {
            command_all(&peers, operation, &[])?;
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
