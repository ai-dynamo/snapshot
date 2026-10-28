// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Validate participants, coordinate CUDA lifecycle phases, and publish state, for one Pod
//! alone or as the relay of one Pod in a group that shares CUDA memory across Pods.

#[cfg(test)]
mod fake_agent;
mod group;
mod relay;
mod session;
mod state;
#[cfg(test)]
mod test_records;
mod topology;

use anyhow::{Context, Result, bail, ensure};
use clap::Parser;
use cuinterpose_protocol::{
    self as protocol, Manifest, NamespacePid, Operation, Record, Reply, Request, Response,
};
use group::Rank;
use relay::{Command, Mode, Outcome};
use session::Session;
use std::collections::{BTreeMap, BTreeSet};
use std::os::unix::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

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
    /// The agent's end of the attempt's session, inherited from the agent. Without it, the
    /// Pod runs alone.
    #[arg(long, requires_all = ["rank", "relay_ranks", "round_timeout_seconds", "deadline"])]
    session_fd: Option<i32>,
    /// This Pod's rank in the group.
    #[arg(long, requires = "session_fd")]
    rank: Option<group::Rank>,
    /// Every rank that runs a relay. The lowest leads the group.
    #[arg(long, requires = "session_fd", value_delimiter = ',')]
    relay_ranks: Vec<group::Rank>,
    /// Bounds the leader's wait for the replies of each round after the first.
    #[arg(long, requires = "session_fd", value_parser = clap::value_parser!(u64).range(1..))]
    round_timeout_seconds: Option<u64>,
    /// The attempt's deadline in seconds since the Unix epoch.
    #[arg(long, requires = "session_fd")]
    deadline: Option<u64>,
}

impl Arguments {
    /// The group that `--session-fd` and its companions describe, if any.
    fn group(&self) -> Result<Option<(i32, relay::Group)>> {
        let Some(fd) = self.session_fd else {
            return Ok(None);
        };
        let required = "--session-fd requires its group arguments";
        let mut group = relay::Group {
            rank: self.rank.context(required)?,
            relays: self.relay_ranks.clone(),
            round_timeout: Duration::from_secs(self.round_timeout_seconds.context(required)?),
            deadline: SystemTime::UNIX_EPOCH
                + Duration::from_secs(self.deadline.context(required)?),
        };
        group.check()?;
        Ok(Some((fd, group)))
    }
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
        Request::Execute { operation, .. } => protocol::operation_timeout(operation),
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

    fn execute(&self, operation: Operation) -> Result<Reply> {
        self.request(&Request::Execute {
            namespace_pid: self.namespace_pid,
            operation,
        })
    }
}

/// Every rank must reply before a phase can advance, so a bounded worker pool could
/// deadlock a rank waiting for an exchange that has not started. Every started exchange
/// is joined even if a participant fails. Each participant runs its own entry of
/// `operations`, which all name the same step.
fn command_all(
    peers: &[Peer],
    operations: &BTreeMap<NamespacePid, Operation>,
) -> Result<BTreeMap<NamespacePid, Reply>> {
    std::thread::scope(|scope| {
        let mut jobs = Vec::with_capacity(peers.len());
        let mut failures = Vec::new();
        for peer in peers {
            let operation = operations[&peer.namespace_pid].clone();
            match std::thread::Builder::new().spawn_scoped(scope, move || peer.execute(operation)) {
                Ok(job) => jobs.push((peer, job)),
                Err(error) => failures.push(format!(
                    "participant {} ({}): could not start exchange: {error}",
                    peer.namespace_pid,
                    peer.endpoint.display()
                )),
            }
        }
        let mut replies = BTreeMap::new();
        for (peer, job) in jobs {
            let participant = format!(
                "participant {} ({})",
                peer.namespace_pid,
                peer.endpoint.display()
            );
            match job.join() {
                Ok(Ok(reply)) => {
                    replies.insert(peer.namespace_pid, reply);
                }
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
        let step = operations.values().next().map(ToString::to_string);
        ensure!(
            failures.is_empty(),
            "{}: {}",
            step.unwrap_or_default(),
            failures.join("\n")
        );
        Ok(replies)
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

/// This Pod's processes, on which its relay runs the steps of a mode.
struct LocalPod<'a> {
    peers: &'a [Peer],
    /// Prepare: where the records frozen by BeginCheckpoint are published.
    state: Option<PathBuf>,
    /// Restore: the records published at capture.
    saved: Option<Manifest>,
    frozen: Option<Manifest>,
}

impl<'a> LocalPod<'a> {
    fn new(peers: &'a [Peer], state: Option<PathBuf>, saved: Option<Manifest>) -> Self {
        Self {
            peers,
            state,
            saved,
            frozen: None,
        }
    }
}

impl relay::Pod for LocalPod<'_> {
    fn run(&mut self, command: &Command, rank: Rank) -> Result<Outcome> {
        let request = match command {
            Command::Inspect => Some(RecordRequest::Inspect),
            Command::BeginCheckpoint => Some(RecordRequest::BeginCheckpoint),
            _ => None,
        };
        if let Some(request) = request {
            let records = collect_records(self.peers, request)?;
            if matches!(request, RecordRequest::BeginCheckpoint) {
                self.frozen = Some(records.clone());
            }
            return Ok(Outcome::Records {
                current: records,
                saved: self.saved.clone(),
            });
        }
        let operations = self
            .peers
            .iter()
            .map(|peer| {
                let operation = command
                    .operation(rank, peer.namespace_pid)
                    .with_context(|| format!("{command} runs no operation"))?;
                Ok((peer.namespace_pid, operation))
            })
            .collect::<Result<_>>()?;
        Ok(Outcome::Replies(command_all(self.peers, &operations)?))
    }

    fn finish(&mut self) -> Result<()> {
        let Some(path) = &self.state else {
            return Ok(());
        };
        let records = self
            .frozen
            .as_mut()
            .context("preparation finished without BeginCheckpoint")?;
        // Publication failure is also fail-stop, even if publication succeeded before a
        // directory sync failed. Prepared processes cannot resume safely.
        state::write_atomic(path, records)
            .with_context(|| format!("publish checkpoint state {}", path.display()))
    }
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
    let membership = args.group()?;
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
    let (mode, state, saved) = if args.inspect {
        (Mode::Inspect, None, None)
    } else {
        let path = args
            .checkpoint_dir
            .context("--checkpoint-dir is required")?
            .join("cuinterpose.state");
        if args.prepare {
            // Reject existing output before BeginCheckpoint freezes any participant.
            match std::fs::symlink_metadata(&path) {
                Ok(_) => bail!(
                    "{} already exists; use a new checkpoint directory",
                    path.display()
                ),
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(error) => return Err(error.into()),
            }
            (Mode::Prepare, Some(path), None)
        } else {
            let saved =
                state::read(&path).with_context(|| format!("cannot parse {}", path.display()))?;
            ensure!(
                seen.iter().eq(saved.keys()),
                "restored processes do not match the checkpointed participants"
            );
            (Mode::Restore, None, Some(saved))
        }
    };
    let member = match membership {
        Some((fd, group)) => Some((Session::inherit(fd)?, group)),
        None => None,
    };
    relay::run(member, mode, &mut LocalPod::new(&peers, state, saved))
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
            (execute(Operation::PrepareMulticast), control),
            (
                execute(Operation::SaveAllocations { owners: Vec::new() }),
                carrier,
            ),
            (execute(Operation::PrepareUnicast), control),
            (execute(Operation::LoadAllocations), carrier),
            (
                execute(Operation::RestoreUnicast {
                    replacements: Vec::new(),
                }),
                control,
            ),
            (execute(Operation::RestoreMulticastCreators), control),
            (
                execute(Operation::RestoreMulticastImporters {
                    replacements: Vec::new(),
                }),
                control,
            ),
            (execute(Operation::RestoreMulticastDevices), control),
            (execute(Operation::RestoreMulticastBindings), control),
        ];
        for (request, expected) in cases {
            assert_eq!(request_timeout(&request), expected, "{request:?}");
        }
    }
}
