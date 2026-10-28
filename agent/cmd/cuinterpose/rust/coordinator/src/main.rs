// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Validate participants, coordinate CUDA lifecycle phases, and publish state.

mod group;
mod state;
#[cfg(test)]
mod test_records;
mod topology;

use anyhow::{Context, Result, bail, ensure};
use clap::Parser;
use cuinterpose_protocol::{
    self as protocol, FabricUpdate, Manifest, NamespacePid, Operation, Record, Reply, Request,
    Response,
};
use group::{CarrierPlan, HandleTable, Topology};
use std::collections::BTreeSet;
use std::os::unix::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::time::Duration;

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
    socket_dir: String,
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

/// Bound the wait for a participant's reply. Saving and loading allocation contents
/// can take far longer than a control request.
fn request_timeout(request: &Request) -> Duration {
    match request {
        Request::Execute { operation, .. } => protocol::operation_timeout(*operation),
        Request::SaveAllocations { .. } => protocol::operation_timeout(Operation::SaveAllocations),
        _ => protocol::control_timeout(),
    }
}

// Transport errors retain their cause. Remote refusals are application errors.
fn exchange(endpoint: &Path, request: &Request) -> Result<Response> {
    let display = endpoint.display();
    let socket = protocol::connect(endpoint, protocol::control_timeout())
        .with_context(|| format!("{display}: {request:?}: connect failed"))?;
    let timeout = Some(request_timeout(request));
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

    /// Run one phase and return the FABRIC handles that it exported in place of checkpointed
    /// ones.
    fn execute(
        &self,
        operation: Operation,
        plan: CarrierPlan,
        imports: &[FabricUpdate],
    ) -> Result<Vec<FabricUpdate>> {
        let expected_bytes = plan.bytes;
        let request = if operation == Operation::SaveAllocations {
            Request::SaveAllocations {
                namespace_pid: self.namespace_pid,
                owners: plan.owners,
            }
        } else {
            Request::Execute {
                namespace_pid: self.namespace_pid,
                operation,
                imports: imports.to_vec(),
            }
        };
        match self.request(&request)? {
            Reply::Completed {
                operation: actual,
                bytes,
                exports,
            } => {
                ensure!(
                    actual == operation && bytes == expected_bytes,
                    "unexpected response or transfer size: expected operation {operation:?}, actual {actual:?}, expected_bytes {expected_bytes}, bytes {bytes}"
                );
                Ok(exports)
            }
            actual => bail!(
                "expected {operation:?} completion with {expected_bytes} bytes, actual {actual:?}"
            ),
        }
    }
}

/// Every rank must reply before a phase can advance, so a bounded worker pool could
/// deadlock a rank waiting for an exchange that has not started. Every started exchange
/// is joined even if a participant fails. `plan` says what each participant does, and the
/// returned FABRIC handles replace checkpointed ones.
fn command_all(
    peers: &[Peer],
    operation: Operation,
    plan: &dyn Fn(NamespacePid) -> Result<CarrierPlan>,
    imports: &[FabricUpdate],
) -> Result<Vec<FabricUpdate>> {
    let plans: Vec<_> = peers
        .iter()
        .map(|peer| {
            plan(peer.namespace_pid).with_context(|| {
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
        for (peer, plan) in peers.iter().zip(plans) {
            match std::thread::Builder::new()
                .spawn_scoped(scope, move || peer.execute(operation, plan, imports))
            {
                Ok(job) => jobs.push((peer, job)),
                Err(error) => failures.push(format!(
                    "participant {} ({}): could not start exchange: {error}",
                    peer.namespace_pid,
                    peer.endpoint.display()
                )),
            }
        }
        let mut exports = Vec::new();
        for (peer, job) in jobs {
            let participant = format!(
                "participant {} ({})",
                peer.namespace_pid,
                peer.endpoint.display()
            );
            match job.join() {
                Ok(Ok(exported)) => exports.extend(exported),
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
        Ok(exports)
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
        args.socket_dir.starts_with('/'),
        "--socket-dir must be an absolute path"
    );
    let socket_dir = Path::new(&args.socket_dir);
    let mut seen = BTreeSet::new();
    let mut peers = Vec::with_capacity(args.processes.len());
    for namespace_pid in args.processes {
        ensure!(seen.insert(namespace_pid), "duplicate namespace PID");
        let endpoint = protocol::socket_path(socket_dir, namespace_pid);
        // Reject paths that exceed sockaddr_un or contain a NUL before contacting peers.
        SocketAddr::from_pathname(&endpoint)
            .with_context(|| format!("invalid socket path {}", endpoint.display()))?;
        peers.push(Peer {
            endpoint,
            namespace_pid,
        });
    }
    if args.inspect {
        // Preflight inspection does not freeze the registry, so preparation must
        // validate it again after BeginCheckpoint prevents application mutations.
        Topology::validate(&group::alone(collect_records(
            &peers,
            RecordRequest::Inspect,
        )?))?;
        return Ok(());
    }
    let path = args
        .checkpoint_dir
        .context("--checkpoint-dir is required")?
        .join("cuinterpose.state");
    let expected = if args.prepare {
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
    let mut participants = group::alone(collect_records(
        &peers,
        if args.prepare {
            RecordRequest::BeginCheckpoint
        } else {
            RecordRequest::Inspect
        },
    )?);
    let inspected = Topology::validate(&participants)?;
    if args.prepare {
        let topology = inspected;
        for operation in [
            Operation::PrepareMulticast,
            Operation::SaveAllocations,
            Operation::PrepareUnicast,
        ] {
            command_all(
                &peers,
                operation,
                &|pid| topology.plan(operation, 0, pid),
                &[],
            )
            .with_context(|| format!("{operation:?}"))?;
        }
        // Publication failure is also fail-stop, even if publication succeeded before a
        // directory sync failed. Prepared processes cannot resume safely.
        state::write_atomic(&path, participants.get_mut(&0).unwrap())
            .with_context(|| format!("publish checkpoint state {}", path.display()))?;
    } else {
        let saved = group::alone(expected);
        let topology = Topology::validate(&saved)?;
        drop(inspected);
        drop(participants);
        // Importers need the FABRIC handles that creators export in the phase before.
        let mut handles = HandleTable::default();
        for operation in [
            Operation::LoadAllocations,
            Operation::RestoreUnicast,
            Operation::RestoreMulticastCreators,
            Operation::RestoreMulticastImporters,
            Operation::RestoreMulticastDevices,
            Operation::RestoreMulticastBindings,
        ] {
            let exports = command_all(
                &peers,
                operation,
                &|pid| topology.plan(operation, 0, pid),
                &handles.imports(),
            )
            .with_context(|| format!("{operation:?}"))?;
            handles.record(exports)?;
        }
        let participants = collect_records(&peers, RecordRequest::Inspect)
            .context("inspect restored participants")?;
        group::verify_restored(&saved, &group::alone(participants), &handles)?;
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::process::Command;

    /// Marks the child process that checks timeouts in its own environment.
    const CHILD: &str = "CUINTERPOSE_TIMEOUT_TEST_CHILD";

    #[test]
    fn save_and_load_use_the_carrier_timeout_and_other_requests_the_control_timeout() {
        // Setting variables here would race other tests, so the test binary runs this test
        // again in a child process. Its timeouts differ from each other and from the
        // defaults, so ignoring or misnaming either variable changes the result.
        if std::env::var_os(CHILD).is_none() {
            let name = "tests::save_and_load_use_the_carrier_timeout_and_other_requests_the_control_timeout";
            let output = Command::new(std::env::current_exe().unwrap())
                .args(["--exact", name])
                .env(CHILD, "1")
                .env("SNAPSHOT_CONTROL_TIMEOUT_SECONDS", "20")
                .env("SNAPSHOT_CARRIER_TIMEOUT_SECONDS", "7200")
                .output()
                .unwrap();
            let stdout = String::from_utf8_lossy(&output.stdout);
            // A filter that matches no test also succeeds.
            assert!(
                output.status.success() && stdout.contains("1 passed"),
                "{stdout}{}",
                String::from_utf8_lossy(&output.stderr)
            );
            return;
        }
        let [control, carrier] = [20, 7200].map(Duration::from_secs);
        let namespace_pid = 1;
        let execute = |operation| Request::Execute {
            namespace_pid,
            operation,
            imports: Vec::new(),
        };
        let cases = [
            (Request::BeginCheckpoint { namespace_pid }, control),
            (Request::Inspect { namespace_pid }, control),
            (
                Request::Export {
                    namespace_pid,
                    allocation: test_records::A,
                },
                control,
            ),
            (
                Request::SaveAllocations {
                    namespace_pid,
                    owners: Vec::new(),
                },
                carrier,
            ),
            (execute(Operation::PrepareMulticast), control),
            (execute(Operation::SaveAllocations), carrier),
            (execute(Operation::PrepareUnicast), control),
            (execute(Operation::LoadAllocations), carrier),
            (execute(Operation::RestoreUnicast), control),
            (execute(Operation::RestoreMulticastCreators), control),
            (execute(Operation::RestoreMulticastImporters), control),
            (execute(Operation::RestoreMulticastDevices), control),
            (execute(Operation::RestoreMulticastBindings), control),
        ];
        for (request, expected) in cases {
            assert_eq!(request_timeout(&request), expected, "{request:?}");
        }
    }
}
