// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Short-lived participant validation, global CUDA lifecycle barriers, and state publication.

mod state;
mod topology;

use anyhow::{Context, Result, bail, ensure};
use clap::Parser;
use cuinterpose_protocol::{
    self as protocol, Manifest, NamespacePid, Operation, Record, Reply, Request, Response,
};
use std::collections::BTreeSet;
use std::os::unix::net::SocketAddr;
use std::path::{Path, PathBuf};
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

// Transport errors retain their cause; remote refusals are application errors.
fn exchange(endpoint: &Path, request: &Request) -> Result<Response> {
    let display = endpoint.display();
    let socket = protocol::connect(endpoint, protocol::timeout(None))
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
    fn inspect(&self, begin_checkpoint: bool) -> Result<Vec<Record>> {
        let response = exchange(
            &self.endpoint,
            &if begin_checkpoint {
                Request::BeginCheckpoint {
                    namespace_pid: self.namespace_pid,
                }
            } else {
                Request::Inspect {
                    namespace_pid: self.namespace_pid,
                }
            },
        )?;
        ensure!(
            response.namespace_pid == self.namespace_pid,
            "{}: namespace PID changed",
            self.endpoint.display()
        );
        match response.result.map_err(anyhow::Error::msg)? {
            Reply::Inspection { records } => Ok(records),
            _ => bail!("{}: unexpected inspection reply", self.endpoint.display()),
        }
    }

    fn execute(&self, operation: Operation, expected_bytes: u64) -> Result<()> {
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
            } if actual == operation && bytes == expected_bytes => Ok(()),
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
) -> Result<()> {
    let expected_bytes: Vec<_> = peers
        .iter()
        .map(|peer| {
            allocations
                .iter()
                .filter(|a| a.preserve_content && a.reference.creator_pid == peer.namespace_pid)
                .try_fold(0u64, |sum, a| {
                    sum.checked_add(a.size).context("allocation size overflow")
                })
        })
        .collect::<Result<_>>()?;
    std::thread::scope(|scope| {
        let mut jobs = Vec::with_capacity(peers.len());
        for (peer, bytes) in peers.iter().zip(expected_bytes) {
            jobs.push(
                std::thread::Builder::new()
                    .spawn_scoped(scope, move || peer.execute(operation, bytes))?,
            );
        }
        let mut failure = None;
        for job in jobs {
            match job.join() {
                Ok(Ok(())) => {}
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
            None => Ok(()),
        }
    })
}

fn inspect(peers: &[Peer], begin_checkpoint: bool) -> Result<Manifest> {
    peers
        .iter()
        .map(|peer| {
            peer.inspect(begin_checkpoint)
                .map(|records| (peer.namespace_pid, records))
        })
        .collect()
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
    let mut participants = inspect(&peers, args.prepare)?;
    let inspected_allocations = topology::validate(&participants)?;
    if args.prepare {
        let allocations = inspected_allocations;
        command_all(&mut peers, Operation::PrepareMulticast, &[]).context("multicast teardown")?;
        command_all(&mut peers, Operation::SaveAllocations, &allocations)
            .context("save allocations")?;
        command_all(&mut peers, Operation::PrepareUnicast, &[])?;
        state::write_atomic(&path, &mut participants)?;
    } else {
        let allocations = topology::validate(&expected)?;
        drop(inspected_allocations);
        drop(participants);
        command_all(&mut peers, Operation::LoadAllocations, &allocations)
            .context("load allocations")?;
        command_all(&mut peers, Operation::RestoreUnicast, &[])?;
        for operation in [
            Operation::RestoreMulticastCreators,
            Operation::RestoreMulticastImporters,
            Operation::RestoreMulticastDevices,
            Operation::RestoreMulticastBindings,
        ] {
            command_all(&mut peers, operation, &[])?;
        }
        let mut participants = inspect(&peers, false)?;
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
