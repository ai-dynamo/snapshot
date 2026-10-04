// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Validate participants, coordinate CUDA lifecycle phases, and publish state.

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
    inspect: bool,
    #[arg(long, group = "action")]
    prepare: bool,
    #[arg(long, group = "action")]
    restore: bool,
    #[arg(long, required_unless_present = "inspect", conflicts_with = "inspect")]
    checkpoint_dir: Option<PathBuf>,
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

#[derive(Clone, Copy, Debug)]
enum RecordRequest {
    Inspect,
    BeginCheckpoint,
}

// Transport errors retain their cause. Remote refusals are application errors.
fn exchange(endpoint: &Path, request: &Request) -> Result<Response> {
    let display = endpoint.display();
    let socket = protocol::connect(endpoint, protocol::control_timeout())
        .with_context(|| format!("{display}: {request:?}: connect failed"))?;
    let timeout = Some(match request {
        Request::Execute { operation, .. } => protocol::operation_timeout(*operation),
        _ => protocol::control_timeout(),
    });
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
    fn request(&self, request: &Request) -> Result<Reply> {
        let response = exchange(&self.endpoint, request)?;
        ensure!(
            response.namespace_pid == self.namespace_pid,
            "{}: namespace PID changed: expected {}, actual {}",
            self.endpoint.display(),
            self.namespace_pid,
            response.namespace_pid
        );
        response.result.map_err(anyhow::Error::msg)
    }

    fn records(&self, request: RecordRequest) -> Result<Vec<Record>> {
        let namespace_pid = self.namespace_pid;
        let request = match request {
            RecordRequest::Inspect => Request::Inspect { namespace_pid },
            RecordRequest::BeginCheckpoint => Request::BeginCheckpoint { namespace_pid },
        };
        match self.request(&request)? {
            Reply::Inspection { records } => Ok(records),
            actual => bail!("expected inspection reply, actual {actual:?}"),
        }
    }

    fn execute(&self, operation: Operation, expected_bytes: u64) -> Result<()> {
        match self.request(&Request::Execute {
            namespace_pid: self.namespace_pid,
            operation,
        })? {
            Reply::Completed {
                operation: actual,
                bytes,
            } => {
                ensure!(
                    actual == operation && bytes == expected_bytes,
                    "unexpected response or transfer size: expected operation {operation:?}, actual {actual:?}, expected_bytes {expected_bytes}, bytes {bytes}"
                );
                Ok(())
            }
            actual => bail!(
                "expected {operation:?} completion with {expected_bytes} bytes, actual {actual:?}"
            ),
        }
    }
}

/// Every rank must reply before a phase can advance, so a bounded worker pool could
/// deadlock a rank waiting for an exchange that has not started. Every started exchange
/// is joined even if a participant fails.
fn command_all(
    peers: &[Peer],
    operation: Operation,
    allocations: &[AllocationSummary],
) -> Result<()> {
    let expected_bytes: Vec<_> = peers
        .iter()
        .map(|peer| {
            allocations
                .iter()
                .filter(|a| {
                    a.checkpoint_via_host_carrier && a.reference.creator_pid == peer.namespace_pid
                })
                .try_fold(0u64, |sum, a| {
                    sum.checked_add(a.size).context("allocation size overflow")
                })
                .with_context(|| {
                    format!(
                        "participant {} ({})",
                        peer.namespace_pid,
                        peer.endpoint.display()
                    )
                })
        })
        .collect::<Result<_>>()?;
    std::thread::scope(|scope| {
        let mut jobs = Vec::with_capacity(peers.len());
        let mut failures = Vec::new();
        for (peer, bytes) in peers.iter().zip(expected_bytes) {
            match std::thread::Builder::new()
                .spawn_scoped(scope, move || peer.execute(operation, bytes))
            {
                Ok(job) => jobs.push((peer, job)),
                Err(error) => failures.push(format!(
                    "participant {} ({}): could not start exchange: {error}",
                    peer.namespace_pid,
                    peer.endpoint.display()
                )),
            }
        }
        for (peer, job) in jobs {
            let participant = format!(
                "participant {} ({})",
                peer.namespace_pid,
                peer.endpoint.display()
            );
            match job.join() {
                Ok(Ok(())) => {}
                Ok(Err(error)) => {
                    failures.push(format!("{participant}: {error:#}"));
                }
                Err(payload) => {
                    let message = payload
                        .downcast_ref::<&str>()
                        .copied()
                        .or_else(|| payload.downcast_ref::<String>().map(String::as_str))
                        .unwrap_or("non-string panic payload");
                    failures.push(format!("{participant}: exchange panicked: {message}"));
                }
            }
        }
        ensure!(failures.is_empty(), "{}", failures.join("\n"));
        Ok(())
    })
}

fn collect_records(peers: &[Peer], request: RecordRequest) -> Result<Manifest> {
    peers
        .iter()
        .map(|peer| {
            peer.records(request)
                .with_context(|| {
                    format!(
                        "{request:?}: participant {} ({})",
                        peer.namespace_pid,
                        peer.endpoint.display()
                    )
                })
                .map(|records| (peer.namespace_pid, records))
        })
        .collect()
}

fn run() -> Result<()> {
    let args = Arguments::parse();
    ensure!(
        args.inspect || args.prepare || args.restore,
        "an action is required"
    );
    ensure!(
        args.control_dir.starts_with('/'),
        "--control-dir must be an absolute path"
    );
    let control_dir = Path::new(&args.control_dir);
    let mut seen = BTreeSet::new();
    let mut peers = Vec::with_capacity(args.processes.len());
    for namespace_pid in args.processes {
        ensure!(seen.insert(namespace_pid), "duplicate namespace PID");
        let endpoint = protocol::socket_path(control_dir, namespace_pid);
        // Reject paths that exceed sockaddr_un or contain a NUL before contacting peers.
        SocketAddr::from_pathname(&endpoint)
            .with_context(|| format!("invalid control socket path {}", endpoint.display()))?;
        peers.push(Peer {
            endpoint,
            namespace_pid,
        });
    }
    if args.inspect {
        // Preflight inspection does not freeze the registry, so preparation must
        // validate it again after BeginCheckpoint prevents application mutations.
        topology::validate(&collect_records(&peers, RecordRequest::Inspect)?)?;
        return Ok(());
    }
    let path = args
        .checkpoint_dir
        .context("--checkpoint-dir is required")?
        .join("cuinterpose.state");
    let mut expected = if args.prepare {
        // Reject existing output before BeginCheckpoint freezes any participant.
        match std::fs::symlink_metadata(&path) {
            Ok(_) => bail!(
                "{} already exists; use a new checkpoint directory",
                path.display()
            ),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        Manifest::new()
    } else {
        state::read(&path).with_context(|| format!("cannot parse {}", path.display()))?
    };
    if args.restore {
        ensure!(
            seen.iter().eq(expected.keys()),
            "restored processes do not match the checkpointed participants"
        );
    }
    // BeginCheckpoint freezes registries, not GPU work. From this point preparation
    // is fail-stop: callers must abandon the checkpoint and all source processes.
    let mut participants = collect_records(
        &peers,
        if args.prepare {
            RecordRequest::BeginCheckpoint
        } else {
            RecordRequest::Inspect
        },
    )?;
    let inspected_allocations = topology::validate(&participants)?;
    if args.prepare {
        let allocations = inspected_allocations;
        command_all(&peers, Operation::PrepareMulticast, &[]).context("PrepareMulticast")?;
        command_all(&peers, Operation::SaveAllocations, &allocations).context("SaveAllocations")?;
        command_all(&peers, Operation::PrepareUnicast, &[]).context("PrepareUnicast")?;
        // Publication failure is also fail-stop, even if publication succeeded before a
        // directory sync failed. Prepared processes cannot resume safely.
        state::write_atomic(&path, &mut participants)
            .with_context(|| format!("publish checkpoint state {}", path.display()))?;
    } else {
        let allocations = topology::validate(&expected)?;
        drop(inspected_allocations);
        drop(participants);
        command_all(&peers, Operation::LoadAllocations, &allocations).context("LoadAllocations")?;
        command_all(&peers, Operation::RestoreUnicast, &[]).context("RestoreUnicast")?;
        for operation in [
            Operation::RestoreMulticastCreators,
            Operation::RestoreMulticastImporters,
            Operation::RestoreMulticastDevices,
            Operation::RestoreMulticastBindings,
        ] {
            command_all(&peers, operation, &[]).with_context(|| format!("{operation:?}"))?;
        }
        let mut participants = collect_records(&peers, RecordRequest::Inspect)
            .context("inspect restored participants")?;
        for (id, actual) in &mut participants {
            let expected = expected
                .get_mut(id)
                .with_context(|| format!("restored participant {id} is not in the manifest"))?;
            actual.sort();
            expected.sort();
            ensure!(
                actual.len() == expected.len(),
                "restored topology does not match the checkpoint for participant {id}: expected {} records, actual {} records",
                expected.len(),
                actual.len()
            );
            if let Some((index, (actual, expected))) = actual
                .iter()
                .zip(expected.iter())
                .enumerate()
                .find(|(_, (actual, expected))| actual != expected)
            {
                bail!(
                    "restored topology does not match the checkpoint for participant {id}: record {index}: expected {expected:?}, actual {actual:?}"
                );
            }
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
