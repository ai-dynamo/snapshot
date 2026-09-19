// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Short-lived participant validation, global CUDA lifecycle barriers, and state publication.

mod report;
mod state;
mod topology;

use anyhow::{Context, Result, bail, ensure};
use clap::Parser;
use cuinterpose_protocol::{
    self as protocol, Manifest, NamespacePid, Operation, Record, Reply, Request, Response,
};
use report::{Event, Transfer, write as report};
use std::collections::{BTreeMap, BTreeSet};
use std::os::unix::net::{SocketAddr, UnixStream};
use std::path::{Path, PathBuf};
use std::time::Instant;
use topology::AllocationSummary;

#[derive(Parser)]
struct Arguments {
    #[arg(long, group = "action")]
    prepare: bool,
    #[arg(long, group = "action")]
    restore: bool,
    #[arg(long)]
    checkpoint_dir: PathBuf,
    #[arg(long)]
    control_dir: String,
    #[arg(long = "process", required = true, action = clap::ArgAction::Append,
          value_parser = clap::value_parser!(u32).range(1..))]
    processes: Vec<NamespacePid>,
}

struct Peer {
    endpoint: PathBuf,
    namespace_pid: NamespacePid,
}

struct Inspection {
    records: Vec<Record>,
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
    fn inspect(&self) -> Result<Inspection> {
        let response = exchange(
            &self.endpoint,
            &Request::Inspect {
                namespace_pid: self.namespace_pid,
            },
        )?;
        ensure!(
            response.namespace_pid == self.namespace_pid,
            "{}: namespace PID changed",
            self.endpoint.display()
        );
        match response.result.map_err(anyhow::Error::msg)? {
            Reply::Inspection {
                records,
                live_raw_imports,
                unsupported_creations,
            } => Ok(Inspection {
                records,
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
                namespace_pid: self.namespace_pid,
                operation,
            },
        )?;
        ensure!(
            response.namespace_pid == self.namespace_pid,
            "{}: namespace PID changed",
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
    allocations: &[AllocationSummary],
) -> Result<u32> {
    std::thread::scope(|scope| {
        let mut jobs = Vec::with_capacity(peers.len());
        for peer in peers {
            let bytes = allocations
                .iter()
                .filter(|a| a.preserve_content && a.reference.creator_pid == peer.namespace_pid)
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
                .insert(peer.namespace_pid, inspection.records)
                .is_none(),
            "duplicate namespace PID"
        );
        raw += inspection.raw_imports;
        unsupported += inspection.unsupported_creations;
    }
    Ok((participants, raw, unsupported))
}

fn transfer(
    peers: &mut [Peer],
    operation: Operation,
    allocations: &[AllocationSummary],
) -> Result<()> {
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
    let control_dir = Path::new(&args.control_dir);
    let mut seen = BTreeSet::new();
    let mut peers = Vec::with_capacity(args.processes.len());
    for namespace_pid in args.processes {
        ensure!(seen.insert(namespace_pid), "duplicate namespace PID");
        let endpoint = protocol::socket_path(control_dir, namespace_pid);
        SocketAddr::from_pathname(&endpoint)?;
        peers.push(Peer {
            endpoint,
            namespace_pid,
        });
    }
    if args.restore {
        ensure!(
            seen.iter().eq(expected.keys()),
            "restored processes do not match the checkpointed participants"
        );
    }
    let (mut participants, raw, unsupported) = inspect(&peers)?;
    report(
        Event::Inspect {
            records: participants.values().map(Vec::len).sum(),
            live_raw_imports: raw,
            unsupported_exportable_creations: unsupported,
        },
        start,
        peers.len(),
    )?;
    ensure!(raw == 0, "participants hold {raw} live raw imports");
    ensure!(
        unsupported == 0,
        "participants created {unsupported} CUDA resources with unsupported exportable handle types"
    );
    let inspected_allocations = topology::validate(&participants)?;
    if args.prepare {
        let start = Instant::now();
        let allocations = inspected_allocations;
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
        drop(inspected_allocations);
        drop(participants);
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
            actual.sort();
            expected.sort();
            ensure!(
                actual == expected,
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
