// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! A relay runs one mode for its Pod as part of a group of Pods. Each step is one round:
//! the leader, the relay of the lowest rank that runs one, inserts the step and its
//! arguments, every relay runs the step on its own processes, and each other relay
//! inserts its reply, which the leader gathers. The leader keeps the group state, so
//! relays only run the steps that they are sent. A Pod alone leads a group of one, without
//! a session.

use crate::group::{HandleTable, MAX_RANK, Pods, Rank, Topology};
use crate::session::{self, Session};
use anyhow::{Context, Result, bail, ensure};
use cuinterpose_protocol::{
    self as protocol, AllocationOwner, FabricReplacement, ImportKind, Manifest, NamespacePid,
    Operation,
};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::time::{Duration, SystemTime};

/// The key under which the leader of an inspection publishes, for each relay rank, the
/// other ranks that share FABRIC memory or multicast objects with it. The agent keeps
/// those ranks in one NVLink fabric at restore. The value is a MessagePack map from each
/// relay rank to a sorted array of ranks, empty for a rank without peers.
const FABRIC_PEERS: &str = "inspect.fabric-peers";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Mode {
    Inspect,
    Prepare,
    Restore,
}

impl Mode {
    fn key(self) -> &'static str {
        match self {
            Self::Inspect => "inspect",
            Self::Prepare => "prepare",
            Self::Restore => "restore",
        }
    }
}

/// One round's command: the step that every relay runs on its own processes, with the
/// arguments of that step, or the end of the mode. Arguments meant for one process are
/// keyed by its rank and PID, and each relay uses its own.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "step", rename_all = "SCREAMING_SNAKE_CASE")]
pub enum Command {
    Inspect,
    /// `imports` names the creator's kind of each FABRIC import of unknown kind.
    BeginCheckpoint {
        imports: BTreeMap<Rank, BTreeMap<NamespacePid, Vec<ImportKind>>>,
    },
    PrepareMulticast,
    SaveAllocations {
        owners: BTreeMap<Rank, BTreeMap<NamespacePid, Vec<AllocationOwner>>>,
    },
    PrepareUnicast,
    LoadAllocations,
    /// `replacements` holds every FABRIC handle that restored creators replaced so far.
    RestoreUnicast {
        replacements: Vec<FabricReplacement>,
    },
    RestoreMulticastCreators,
    RestoreMulticastImporters {
        replacements: Vec<FabricReplacement>,
    },
    RestoreMulticastDevices,
    RestoreMulticastBindings,
    Done,
}

impl Command {
    /// The operation that this command runs on process `pid` of `rank`, or None when it
    /// gathers records or ends the mode.
    pub fn operation(&self, rank: Rank, pid: NamespacePid) -> Option<Operation> {
        Some(match self {
            Self::Inspect | Self::BeginCheckpoint { .. } | Self::Done => return None,
            Self::PrepareMulticast => Operation::PrepareMulticast,
            Self::SaveAllocations { owners } => Operation::SaveAllocations {
                owners: owners
                    .get(&rank)
                    .and_then(|pod| pod.get(&pid))
                    .cloned()
                    .unwrap_or_default(),
            },
            Self::PrepareUnicast => Operation::PrepareUnicast,
            Self::LoadAllocations => Operation::LoadAllocations,
            Self::RestoreUnicast { replacements } => Operation::RestoreUnicast {
                replacements: replacements.clone(),
            },
            Self::RestoreMulticastCreators => Operation::RestoreMulticastCreators,
            Self::RestoreMulticastImporters { replacements } => {
                Operation::RestoreMulticastImporters {
                    replacements: replacements.clone(),
                }
            }
            Self::RestoreMulticastDevices => Operation::RestoreMulticastDevices,
            Self::RestoreMulticastBindings => Operation::RestoreMulticastBindings,
        })
    }
}

impl std::fmt::Display for Command {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(match self {
            Self::Inspect => "Inspect",
            Self::BeginCheckpoint { .. } => "BeginCheckpoint",
            Self::PrepareMulticast => "PrepareMulticast",
            Self::SaveAllocations { .. } => "SaveAllocations",
            Self::PrepareUnicast => "PrepareUnicast",
            Self::LoadAllocations => "LoadAllocations",
            Self::RestoreUnicast { .. } => "RestoreUnicast",
            Self::RestoreMulticastCreators => "RestoreMulticastCreators",
            Self::RestoreMulticastImporters { .. } => "RestoreMulticastImporters",
            Self::RestoreMulticastDevices => "RestoreMulticastDevices",
            Self::RestoreMulticastBindings => "RestoreMulticastBindings",
            Self::Done => "Done",
        })
    }
}

/// What a Pod's processes returned from one step.
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Outcome {
    /// Inspect and BeginCheckpoint: the Pod's current records and, at the first Inspect
    /// of a restore, the records that it saved at capture.
    Records {
        current: Manifest,
        saved: Option<Manifest>,
    },
    /// Every other step: each process's reply, which the leader checks.
    Replies(BTreeMap<NamespacePid, protocol::Reply>),
}

/// `{status: "ok", result: outcome}`, or `{status: "error", result: message}`.
#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "status", content = "result", rename_all = "snake_case")]
enum Reply {
    Ok(Outcome),
    Error(String),
}

/// This Pod's side of each step.
pub trait Pod {
    /// Run `command` on every process of this Pod, which has rank `rank`. The relay names
    /// the step in an error, so the Pod names only its processes.
    fn run(&mut self, command: &Command, rank: Rank) -> Result<Outcome>;
    /// The mode has ended for every Pod.
    fn finish(&mut self) -> Result<()>;
}

/// How long past the deadline a relay that found it passed waits for the session to end.
/// The coordinator fails the session at the deadline by its own clock, which may run
/// behind this one, and an agent that cannot reach the coordinator ends the session
/// itself 5 seconds past the deadline, so the session ends well within this.
const END_WAIT: Duration = Duration::from_secs(60);

/// This relay's place in the group, as its agent passes it.
pub struct Group {
    pub rank: Rank,
    /// Every rank that runs a relay. The lowest leads the group and gathers from the rest.
    pub relays: Vec<Rank>,
    /// The attempt's deadline, which bounds every wait for another relay.
    pub deadline: SystemTime,
}

impl Group {
    /// A Pod alone. It waits for no other relay, so it needs no deadline.
    fn alone() -> Self {
        Self {
            rank: 0,
            relays: vec![0],
            deadline: SystemTime::UNIX_EPOCH,
        }
    }

    pub fn check(&mut self) -> Result<()> {
        self.relays.sort_unstable();
        self.relays.dedup();
        ensure!(
            self.relays.contains(&self.rank),
            "--relay-ranks does not contain --rank {}",
            self.rank
        );
        ensure!(
            self.relays.iter().all(|rank| *rank <= MAX_RANK),
            "--relay-ranks exceed the largest rank {MAX_RANK}"
        );
        Ok(())
    }

    fn leader(&self) -> Rank {
        self.relays[0]
    }

    fn until_deadline(&self) -> Duration {
        self.deadline
            .duration_since(SystemTime::now())
            .unwrap_or_default()
    }
}

/// Run `mode` for this Pod, in its group when `member` names one and alone otherwise. A
/// Pod alone leads a group of one, without a session. In a group, any failure ends the
/// attempt for every member. Inspect runs during admission, before the attempt commits,
/// so its failures end the attempt as `Refused`. Prepare and restore run after the
/// commit, so theirs end it as `MemberFailed`.
pub fn run(member: Option<(Session, Group)>, mode: Mode, pod: &mut dyn Pod) -> Result<()> {
    let (session, group) = match member {
        Some((session, group)) => (Some(session), group),
        None => (None, Group::alone()),
    };
    let mut relay = Relay {
        session,
        group,
        mode,
        pod,
        round: 1,
    };
    let result = if relay.group.rank == relay.group.leader() {
        relay.lead()
    } else {
        relay.follow()
    };
    if let Err(error) = &result
        && let Some(session) = &mut relay.session
        && !session::ended(error)
    {
        let reason = match mode {
            Mode::Inspect => "Refused",
            Mode::Prepare | Mode::Restore => "MemberFailed",
        };
        // A session that ended meanwhile, when another member failed it
        // first, needs no failing.
        if let Err(failure) = session.fail(reason)
            && !session::ended(&failure)
        {
            eprintln!("cuinterpose-coordinator: fail the session: {failure:#}");
        }
    }
    result
}

/// One value for each process of every Pod, keyed by rank and PID as a command's arguments are.
fn per_process<T>(
    pods: &Pods,
    value: impl Fn(Rank, NamespacePid) -> T,
) -> BTreeMap<Rank, BTreeMap<NamespacePid, T>> {
    pods.iter()
        .map(|(rank, pod)| {
            let values = pod.keys().map(|pid| (*pid, value(*rank, *pid))).collect();
            (*rank, values)
        })
        .collect()
}

/// The FABRIC peers of every relay rank, as `FABRIC_PEERS` holds them.
fn fabric_peers(group: &Group, topology: &Topology) -> BTreeMap<Rank, Vec<Rank>> {
    group
        .relays
        .iter()
        .map(|rank| (*rank, topology.fabric_peers(*rank)))
        .collect()
}

/// Refuse any peer of a rank in `current` that inspection did not publish.
fn check_fabric_peers(
    published: &BTreeMap<Rank, Vec<Rank>>,
    current: &BTreeMap<Rank, Vec<Rank>>,
) -> Result<()> {
    let gained: Vec<String> = current
        .iter()
        .filter_map(|(rank, peers)| {
            let known = published.get(rank).map_or(&[][..], Vec::as_slice);
            let gained: Vec<Rank> = peers
                .iter()
                .filter(|peer| !known.contains(peer))
                .copied()
                .collect();
            (!gained.is_empty()).then(|| format!("rank {rank} with ranks {gained:?}"))
        })
        .collect();
    ensure!(
        gained.is_empty(),
        "ranks share FABRIC memory that inspection did not publish: {}",
        gained.join(", ")
    );
    Ok(())
}

struct Relay<'a> {
    /// None for a Pod alone, which is its own leader.
    session: Option<Session>,
    group: Group,
    mode: Mode,
    pod: &'a mut dyn Pod,
    round: u32,
}

impl Relay<'_> {
    // Each mode runs at most once per attempt, which keeps these keys unique in the
    // session. The Done command that ends a mode stays there, so a mode that ran again
    // would collide with it.
    fn command_key(&self, round: u32) -> String {
        format!("{}.{round}.cmd", self.mode.key())
    }

    fn reply_key(&self, round: u32, rank: Rank) -> String {
        format!("{}.{round}.reply.{rank}", self.mode.key())
    }

    /// Run commands until the leader ends the mode. A follower waits for each command
    /// until the deadline, and the leader's failure reaches it as an ended session.
    fn follow(&mut self) -> Result<()> {
        loop {
            let round = self.round;
            let key = self.command_key(round);
            let wait = self.group.until_deadline();
            let Some(value) = self.session().get(&key, wait)? else {
                return Err(
                    self.past_deadline(format!("no command for round {round} before the deadline"))
                );
            };
            let command: Command = rmp_serde::from_slice(&value)
                .with_context(|| format!("decode the command for round {round}"))?;
            if command == Command::Done {
                return self.pod.finish();
            }
            let result = self.pod.run(&command, self.group.rank);
            let reply = match &result {
                Ok(outcome) => Reply::Ok(outcome.clone()),
                Err(error) => Reply::Error(format!("{error:#}")),
            };
            let key = self.reply_key(round, self.group.rank);
            let inserted = self
                .session()
                .insert(&key, &rmp_serde::to_vec_named(&reply)?);
            if let Err(error) = result {
                let context = match inserted {
                    Ok(()) => format!("round {round} ({command})"),
                    // The step's error stays the cause when its reply failed too.
                    Err(failure) => {
                        format!("round {round} ({command}), whose error reply failed ({failure:#})")
                    }
                };
                return Err(error.context(context));
            }
            inserted?;
            self.round += 1;
        }
    }

    /// Insert one command, run it here, and gather every other relay's outcome. A Pod
    /// alone only runs it here.
    fn round(&mut self, command: &Command) -> Result<BTreeMap<Rank, Outcome>> {
        let round = self.round;
        self.round += 1;
        let key = self.command_key(round);
        if let Some(session) = &mut self.session {
            session.insert(&key, &rmp_serde::to_vec_named(command)?)?;
        }
        let own = self
            .pod
            .run(command, self.group.rank)
            .with_context(|| format!("{command}"))?;
        let mut outcomes = BTreeMap::from([(self.group.rank, own)]);
        let others: Vec<Rank> = self
            .group
            .relays
            .iter()
            .copied()
            .filter(|rank| *rank != self.group.rank)
            .collect();
        let keys: Vec<String> = others
            .iter()
            .map(|rank| self.reply_key(round, *rank))
            .collect();
        let Some(session) = &mut self.session else {
            return Ok(outcomes);
        };
        // Each relay bounds its step by its processes' own timeouts and replies with an
        // error when they expire, and a relay or agent that dies ends the session. At
        // restore each relay also starts only after its own CRIU and CUDA restore. So the
        // leader waits for every reply until the deadline.
        let values = session.get_all(&keys, self.group.until_deadline())?;
        let missing: Vec<Rank> = others
            .iter()
            .zip(&values)
            .filter(|(_, value)| value.is_none())
            .map(|(rank, _)| *rank)
            .collect();
        if !missing.is_empty() {
            return Err(self.past_deadline(format!(
                "ranks {missing:?} did not reply to round {round} ({command}) before the deadline"
            )));
        }
        for (rank, value) in others.into_iter().zip(values) {
            let reply: Reply = rmp_serde::from_slice(&value.unwrap_or_default())
                .with_context(|| format!("decode the reply of rank {rank} to round {round}"))?;
            match reply {
                Reply::Ok(outcome) => outcomes.insert(rank, outcome),
                Reply::Error(message) => bail!("rank {rank}: {command}: {message}"),
            };
        }
        // Every relay has read the command, and the session's budget covers the whole
        // attempt, so the round's keys go.
        session.delete(&key)?;
        for key in &keys {
            session.delete(key)?;
        }
        Ok(outcomes)
    }

    /// Gather every Pod's records, with Inspect or BeginCheckpoint, and the records that
    /// each Pod saved at capture, if it has them.
    fn records(&mut self, command: Command) -> Result<(Pods, Pods)> {
        let mut current = Pods::new();
        let mut saved = Pods::new();
        for (rank, outcome) in self.round(&command)? {
            let Outcome::Records {
                current: records,
                saved: saved_records,
            } = outcome
            else {
                bail!("rank {rank} sent no records for {command}");
            };
            current.insert(rank, records);
            if let Some(records) = saved_records {
                saved.insert(rank, records);
            }
        }
        Ok((current, saved))
    }

    /// Run a lifecycle step on every Pod, check every process's reply against the
    /// topology, and return the FABRIC handles that the Pods exported in place of
    /// checkpointed ones.
    fn execute(&mut self, topology: &Topology, command: Command) -> Result<Vec<FabricReplacement>> {
        let mut replacements = Vec::new();
        for (rank, outcome) in self.round(&command)? {
            let Outcome::Replies(replies) = outcome else {
                bail!("rank {rank} sent records for {command}");
            };
            for (pid, reply) in replies {
                let operation = command
                    .operation(rank, pid)
                    .with_context(|| format!("{command} runs no operation"))?;
                replacements.extend(topology.completed(rank, pid, &operation, reply)?);
            }
        }
        Ok(replacements)
    }

    fn lead(&mut self) -> Result<()> {
        match self.mode {
            Mode::Inspect => {
                let (pods, _) = self.records(Command::Inspect)?;
                let topology = Topology::validate(&pods)?;
                // Before Done, so that every relay that sees Done finds the key.
                if let Some(session) = &mut self.session {
                    let peers = fabric_peers(&self.group, &topology);
                    session.insert(FABRIC_PEERS, &rmp_serde::to_vec_named(&peers)?)?;
                }
            }
            Mode::Prepare => {
                // Inspect does not freeze registries, but the application stops changing
                // them before preparation. The inspected topology names the creator's kind
                // of each FABRIC import of unknown kind, which BeginCheckpoint classifies.
                let (pods, _) = self.records(Command::Inspect)?;
                let inspected = Topology::validate(&pods)?;
                let imports = per_process(&pods, |rank, pid| inspected.imports(rank, pid));
                // The topology is validated again after BeginCheckpoint freezes registries,
                // not GPU work. From here preparation is fail-stop: callers must abandon
                // the checkpoint and every source process.
                let (pods, _) = self.records(Command::BeginCheckpoint { imports })?;
                let topology = Topology::validate(&pods)?;
                // Processes ran on after inspection, so they may share FABRIC memory with
                // ranks that the agent did not learn of.
                if let Some(session) = &mut self.session {
                    let published = match session.get(FABRIC_PEERS, Duration::ZERO)? {
                        Some(value) => rmp_serde::from_slice(&value)
                            .with_context(|| format!("decode {FABRIC_PEERS}"))?,
                        None => BTreeMap::new(),
                    };
                    check_fabric_peers(&published, &fabric_peers(&self.group, &topology))?;
                }
                self.execute(&topology, Command::PrepareMulticast)?;
                let owners = per_process(&pods, |rank, pid| topology.owners(rank, pid));
                self.execute(&topology, Command::SaveAllocations { owners })?;
                self.execute(&topology, Command::PrepareUnicast)?;
            }
            Mode::Restore => {
                let (current, saved) = self.records(Command::Inspect)?;
                ensure!(
                    saved.keys().eq(current.keys()),
                    "ranks {:?} sent no saved records",
                    current
                        .keys()
                        .filter(|rank| !saved.contains_key(rank))
                        .collect::<Vec<_>>()
                );
                Topology::validate(&current)?;
                let topology = Topology::validate(&saved)?;
                // Importers need the FABRIC handles that creators export in the step
                // before, in any Pod.
                let mut handles = HandleTable::default();
                handles.record(self.execute(&topology, Command::LoadAllocations)?)?;
                let replacements = handles.replacements();
                self.execute(&topology, Command::RestoreUnicast { replacements })?;
                handles.record(self.execute(&topology, Command::RestoreMulticastCreators)?)?;
                let replacements = handles.replacements();
                self.execute(
                    &topology,
                    Command::RestoreMulticastImporters { replacements },
                )?;
                self.execute(&topology, Command::RestoreMulticastDevices)?;
                self.execute(&topology, Command::RestoreMulticastBindings)?;
                let (restored, _) = self
                    .records(Command::Inspect)
                    .context("inspect restored participants")?;
                crate::group::verify_restored(&saved, &restored, &handles)?;
            }
        }
        // The leader publishes its own state before it lets the others finish.
        self.pod.finish()?;
        let key = self.command_key(self.round);
        if let Some(session) = &mut self.session {
            session.insert(&key, &rmp_serde::to_vec_named(&Command::Done)?)?;
        }
        Ok(())
    }

    /// The error of a relay whose wait for `missing` reached the deadline. The coordinator
    /// fails the session at the deadline with its own reason, and the agent fails the
    /// session when the relay exits with an error, so a relay that exited before the
    /// session ended would replace that reason. The relay waits for the end with a get of
    /// a key that nobody inserts, which the agent answers with how the session ended.
    fn past_deadline(&mut self, missing: String) -> anyhow::Error {
        let key = format!("{}.end", self.mode.key());
        let wait = (self.group.deadline + END_WAIT)
            .duration_since(SystemTime::now())
            .unwrap_or_default();
        let error = match self.session().get(&key, wait) {
            Ok(_) => anyhow::anyhow!("the session did not end within {END_WAIT:?} of the deadline"),
            Err(error) => error,
        };
        error.context(missing)
    }

    /// The session of a follower, which only a group has.
    fn session(&mut self) -> &mut Session {
        self.session
            .as_mut()
            .expect("a Pod alone leads its group of one")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::fake_agent::FakeAgent;
    use crate::group::REMOTE_OWNER;
    use crate::test_records::*;
    use cudarc::driver::sys::CUmemAllocationHandleType;
    use cuinterpose_protocol::{
        AllocationReference, FABRIC_HANDLE_BYTES, FabricHandle, FabricKind, MulticastProperties,
        Record,
    };
    use std::os::unix::net::UnixStream;
    use std::sync::{Arc, Mutex};
    use std::time::Instant;

    const FABRIC: u32 = CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_FABRIC.0;
    /// Every Pod's process has the same PID in its own namespace.
    const PID: NamespacePid = 7;
    /// Shared allocation `X` and multicast object `T`, created by rank 0.
    const X: AllocationReference = AllocationReference {
        id: [5; 16],
        creator_pid: PID,
    };
    const T: AllocationReference = AllocationReference {
        id: [6; 16],
        creator_pid: PID,
    };

    fn handle(byte: u8) -> FabricHandle {
        FabricHandle([byte; FABRIC_HANDLE_BYTES])
    }

    /// The handles that restore exports in place of the checkpointed ones.
    fn restored(byte: u8) -> u8 {
        byte + 100
    }

    /// Rank 0 creates `X` (handle 1) and a three-device team `T` (handle 2). Ranks 1
    /// and 2 import both, and every rank binds a member allocation of its own to `T`.
    fn records(rank: Rank, unicast_handle: u8, multicast_handle: u8) -> Manifest {
        let fabric = |record| with!(record, Allocation.handle_types = FABRIC);
        let member = AllocationReference {
            id: [10 + rank as u8; 16],
            creator_pid: PID,
        };
        let (allocation, object) = if rank == 0 {
            (X, T)
        } else {
            let import = |id| AllocationReference {
                id: [id; 16],
                creator_pid: 0,
            };
            (import(20 + rank as u8), import(30 + rank as u8))
        };
        let mut records = vec![
            fabric(with!(shared(), Allocation.allocation = member)),
            with!(
                mapping(),
                Mapping.allocation = member,
                Mapping.address = 0x20000
            ),
            with!(device(), MulticastDevice.allocation = object),
            with!(
                binding(),
                MulticastBinding.allocation = object,
                MulticastBinding.source = crate::test_records::member(member, 0)
            ),
            with!(mapping(), Mapping.allocation = allocation),
        ];
        let properties = MulticastProperties {
            devices: 3,
            size: 4096,
            handle_types: u64::from(FABRIC),
            flags: 0,
        };
        if rank == 0 {
            records.extend([
                fabric(with!(shared(), Allocation.allocation = X)),
                with!(
                    multicast(),
                    Multicast.allocation = T,
                    Multicast.properties = properties
                ),
                Record::FabricExport {
                    allocation: X,
                    handle: handle(unicast_handle),
                    kind: FabricKind::Memory,
                },
                Record::FabricExport {
                    allocation: T,
                    handle: handle(multicast_handle),
                    kind: FabricKind::Multicast,
                },
            ]);
        } else {
            records.extend([
                fabric(with!(
                    shared(),
                    Allocation.allocation = allocation,
                    Allocation.size = 0
                )),
                with!(
                    multicast(),
                    Multicast.allocation = object,
                    Multicast.properties = MulticastProperties {
                        devices: 0,
                        size: 0,
                        ..properties
                    }
                ),
                Record::FabricImport {
                    allocation,
                    handle: handle(unicast_handle),
                    kind: Some(FabricKind::Memory),
                },
                Record::FabricImport {
                    allocation: object,
                    handle: handle(multicast_handle),
                    kind: Some(FabricKind::Multicast),
                },
            ]);
        }
        Manifest::from([(PID, records)])
    }

    /// `records` with each import of memory as a process reports it before it learns the
    /// kind: a FabricImport without a kind, and no other records.
    fn without_memory_kind(records: Manifest) -> Manifest {
        let unknown = |records: &[Record]| {
            records.iter().find_map(|record| match record {
                Record::FabricImport {
                    allocation,
                    kind: Some(FabricKind::Memory),
                    ..
                } => Some(*allocation),
                _ => None,
            })
        };
        records
            .into_iter()
            .map(|(pid, records)| {
                let import = unknown(&records);
                let records = records
                    .into_iter()
                    .filter_map(|record| match record {
                        Record::Allocation { allocation, .. }
                        | Record::Mapping { allocation, .. }
                            if Some(allocation) == import =>
                        {
                            None
                        }
                        Record::FabricImport {
                            allocation, handle, ..
                        } if Some(allocation) == import => Some(Record::FabricImport {
                            allocation,
                            handle,
                            kind: None,
                        }),
                        record => Some(record),
                    })
                    .collect();
                (pid, records)
            })
            .collect()
    }

    type Calls = Arc<Mutex<Vec<Command>>>;

    /// One Pod whose shims answer as scripted.
    struct Scripted {
        rank: Rank,
        records: Manifest,
        saved: Option<Manifest>,
        restored: bool,
        /// The command, by name, that fails on every process of this Pod.
        failing: Option<&'static str>,
        slow: Option<(&'static str, Duration)>,
        /// The carrier bytes that this Pod reports, if not the ones it owns.
        carrier: Option<u64>,
        /// Whether Inspect finds this Pod's import of `X` without a kind, as before any
        /// call that only one kind accepts.
        unknown_memory: bool,
        calls: Calls,
        finished: bool,
    }

    impl Scripted {
        fn new(rank: Rank) -> Self {
            Self {
                rank,
                records: records(rank, 1, 2),
                saved: None,
                restored: false,
                failing: None,
                slow: None,
                carrier: None,
                unknown_memory: false,
                calls: Calls::default(),
                finished: false,
            }
        }
    }

    impl Pod for Scripted {
        fn run(&mut self, command: &Command, rank: Rank) -> Result<Outcome> {
            assert_eq!(rank, self.rank);
            self.calls.lock().unwrap().push(command.clone());
            let name = command.to_string();
            if let Some((slow, delay)) = self.slow
                && slow == name
            {
                std::thread::sleep(delay);
            }
            if self.failing == Some(name.as_str()) {
                bail!("participant {PID}: refused");
            }
            let Some(operation) = command.operation(rank, PID) else {
                let current = if self.restored {
                    records(self.rank, restored(1), restored(2))
                } else if self.unknown_memory && *command == Command::Inspect {
                    without_memory_kind(self.records.clone())
                } else {
                    self.records.clone()
                };
                return Ok(Outcome::Records {
                    current,
                    saved: self.saved.clone(),
                });
            };
            let replaced = |byte| FabricReplacement {
                old: handle(byte),
                new: handle(restored(byte)),
            };
            // Rank 0 owns its member and `X`, and every other rank only its member.
            let bytes = self
                .carrier
                .unwrap_or(if self.rank == 0 { 8192 } else { 4096 });
            let creator = self.rank == 0;
            let reply = match operation {
                Operation::SaveAllocations { .. } => protocol::Reply::Saved { bytes },
                Operation::LoadAllocations => protocol::Reply::Loaded {
                    bytes,
                    replacements: if creator { vec![replaced(1)] } else { vec![] },
                },
                Operation::RestoreMulticastCreators => protocol::Reply::MulticastRecreated {
                    replacements: if creator { vec![replaced(2)] } else { vec![] },
                },
                Operation::RestoreMulticastBindings => {
                    self.restored = true;
                    protocol::Reply::Completed
                }
                _ => protocol::Reply::Completed,
            };
            Ok(Outcome::Replies(BTreeMap::from([(PID, reply)])))
        }

        fn finish(&mut self) -> Result<()> {
            self.finished = true;
            Ok(())
        }
    }

    /// Run `mode` on every Pod with relays 0 to 2 and return each relay's result.
    fn group(
        agent: &FakeAgent,
        mode: Mode,
        pods: Vec<Scripted>,
        deadline: Duration,
    ) -> Vec<(Result<()>, Scripted)> {
        let relays: Vec<Rank> = vec![0, 1, 2];
        let threads: Vec<_> = pods
            .into_iter()
            .map(|mut pod| {
                let session = Session::new(agent.attach(pod.rank));
                let group = Group {
                    rank: pod.rank,
                    relays: relays.clone(),
                    deadline: SystemTime::now() + deadline,
                };
                std::thread::spawn(move || (run(Some((session, group)), mode, &mut pod), pod))
            })
            .collect();
        threads
            .into_iter()
            .map(|thread| thread.join().unwrap())
            .collect()
    }

    fn pods() -> Vec<Scripted> {
        (0..3).map(Scripted::new).collect()
    }

    /// Pods that share no FABRIC memory: each holds an allocation of its own.
    fn separate() -> Vec<Scripted> {
        (0..3)
            .map(|rank| {
                let own = AllocationReference {
                    id: [40 + rank as u8; 16],
                    creator_pid: PID,
                };
                let mut pod = Scripted::new(rank);
                pod.records = Manifest::from([(
                    PID,
                    vec![
                        with!(allocation(), Allocation.allocation = own),
                        with!(mapping(), Mapping.allocation = own),
                    ],
                )]);
                pod
            })
            .collect()
    }

    /// Inspect `pods`, which publishes their FABRIC peers for a later Prepare.
    fn inspect_pods(agent: &FakeAgent, pods: Vec<Scripted>) {
        for (result, _) in group(agent, Mode::Inspect, pods, LONG) {
            result.unwrap();
        }
    }

    fn inspect(agent: &FakeAgent) {
        inspect_pods(agent, pods());
    }

    const LONG: Duration = Duration::from_secs(60);

    #[test]
    fn three_pods_capture_and_restore_through_the_session() {
        let agent = FakeAgent::default();
        for mode in [Mode::Inspect, Mode::Prepare] {
            for (result, pod) in group(&agent, mode, pods(), LONG) {
                result.unwrap();
                assert!(pod.finished);
            }
        }
        let mut restore = pods();
        for pod in &mut restore {
            pod.saved = Some(pod.records.clone());
        }
        let calls: Vec<_> = restore.iter().map(|pod| Arc::clone(&pod.calls)).collect();
        for (result, pod) in group(&agent, Mode::Restore, restore, LONG) {
            result.unwrap();
            assert!(pod.finished && pod.restored);
        }
        assert!(agent.fails().is_empty());
        // The leader removes each round's keys and leaves only the end of each mode and
        // the FABRIC peers.
        assert_eq!(
            agent.keys(),
            [
                "inspect.2.cmd",
                FABRIC_PEERS,
                "prepare.6.cmd",
                "restore.9.cmd"
            ]
        );
        let steps: Vec<String> = calls[1]
            .lock()
            .unwrap()
            .iter()
            .map(ToString::to_string)
            .collect();
        assert_eq!(
            steps,
            [
                "Inspect",
                "LoadAllocations",
                "RestoreUnicast",
                "RestoreMulticastCreators",
                "RestoreMulticastImporters",
                "RestoreMulticastDevices",
                "RestoreMulticastBindings",
                "Inspect",
            ]
        );
        // The leader accepted every rank's carrier bytes, and importers get the handles
        // that the creator exported in earlier rounds.
        for calls in &calls {
            let replaced = |name: &str| {
                let calls = calls.lock().unwrap();
                let command = calls.iter().find(|command| command.to_string() == name);
                match command {
                    Some(
                        Command::RestoreUnicast { replacements }
                        | Command::RestoreMulticastImporters { replacements },
                    ) => replacements
                        .iter()
                        .map(|entry| entry.old)
                        .collect::<Vec<_>>(),
                    other => panic!("{name}: {other:?}"),
                }
            };
            assert_eq!(replaced("RestoreUnicast"), [handle(1)]);
            assert_eq!(
                replaced("RestoreMulticastImporters"),
                [handle(1), handle(2)]
            );
        }
    }

    #[test]
    fn begin_checkpoint_names_each_rank_the_creators_kind_of_its_imports() {
        let agent = FakeAgent::default();
        inspect(&agent);
        let mut pods = pods();
        // Ranks 1 and 2 have only mapped `X`, which either kind accepts.
        for pod in &mut pods[1..] {
            pod.unknown_memory = true;
        }
        let calls = Arc::clone(&pods[1].calls);
        for (result, _) in group(&agent, Mode::Prepare, pods, LONG) {
            result.unwrap();
        }
        let imports = calls
            .lock()
            .unwrap()
            .iter()
            .find_map(|command| match command {
                Command::BeginCheckpoint { imports } => Some(imports.clone()),
                _ => None,
            })
            .unwrap();
        // Every relay reads the whole command and takes its own rank's entry.
        let memory = |rank: u8| {
            vec![ImportKind {
                allocation: AllocationReference {
                    id: [20 + rank; 16],
                    creator_pid: 0,
                },
                kind: FabricKind::Memory,
            }]
        };
        let expected = BTreeMap::from([
            (0, BTreeMap::from([(PID, Vec::new())])),
            (1, BTreeMap::from([(PID, memory(1))])),
            (2, BTreeMap::from([(PID, memory(2))])),
        ]);
        assert_eq!(imports, expected);
    }

    #[test]
    fn inspect_publishes_the_ranks_that_share_fabric_memory_before_done() {
        let agent = FakeAgent::default();
        inspect(&agent);
        // Ranks 1 and 2 import from rank 0, so the map is {0: [1, 2], 1: [0], 2: [0]}.
        let value = agent.value(FABRIC_PEERS).unwrap();
        assert_eq!(value, b"\x83\x00\x92\x01\x02\x01\x91\x00\x02\x91\x00");
        let peers: BTreeMap<Rank, Vec<Rank>> = rmp_serde::from_slice(&value).unwrap();
        let expected = BTreeMap::from([(0, vec![1, 2]), (1, vec![0]), (2, vec![0])]);
        assert_eq!(peers, expected);
        let inserted = agent.inserted();
        let position = |key: &str| {
            inserted
                .iter()
                .position(|inserted| inserted == key)
                .unwrap()
        };
        assert!(
            position(FABRIC_PEERS) < position("inspect.2.cmd"),
            "{inserted:?}"
        );
    }

    #[test]
    fn inspect_publishes_no_peers_for_ranks_that_share_no_fabric_memory() {
        let agent = FakeAgent::default();
        inspect_pods(&agent, separate());
        let value = agent.value(FABRIC_PEERS).unwrap();
        assert_eq!(value, b"\x83\x00\x90\x01\x90\x02\x90");
    }

    #[test]
    fn prepare_refuses_fabric_peers_that_inspection_did_not_publish() {
        let agent = FakeAgent::default();
        inspect_pods(&agent, separate());
        // Ranks 1 and 2 imported from rank 0 after inspection.
        let pods = pods();
        let calls: Vec<_> = pods.iter().map(|pod| Arc::clone(&pod.calls)).collect();
        let results = group(&agent, Mode::Prepare, pods, LONG);
        assert!(results.iter().all(|(result, _)| result.is_err()));
        let leader = format!("{:#}", results[0].0.as_ref().unwrap_err());
        assert!(
            leader.ends_with(
                "ranks share FABRIC memory that inspection did not publish: \
                 rank 0 with ranks [1, 2], rank 1 with ranks [0], rank 2 with ranks [0]"
            ),
            "{leader}"
        );
        assert_eq!(agent.fails(), [(0, "MemberFailed".to_owned())]);
        // The leader refuses after BeginCheckpoint, before any step that changes processes.
        let prepared = |calls: &Calls| calls.lock().unwrap().contains(&Command::PrepareMulticast);
        assert!(!calls.iter().any(prepared));
    }

    #[test]
    fn save_owners_name_each_allocation_by_its_holders_own_reference() {
        let agent = FakeAgent::default();
        inspect(&agent);
        let pods = pods();
        let calls: Vec<_> = pods.iter().map(|pod| Arc::clone(&pod.calls)).collect();
        for (result, _) in group(&agent, Mode::Prepare, pods, LONG) {
            result.unwrap();
        }
        let owners = |rank: usize| {
            let calls = calls[rank].lock().unwrap();
            let save = calls
                .iter()
                .find(|command| matches!(command, Command::SaveAllocations { .. }))
                .unwrap();
            match save.operation(rank as Rank, PID) {
                Some(Operation::SaveAllocations { owners }) => owners,
                other => panic!("{other:?}"),
            }
        };
        assert!(owners(0).iter().all(|owner| owner.owner_pid == PID));
        let imported = AllocationReference {
            id: [21; 16],
            creator_pid: 0,
        };
        assert!(
            owners(1)
                .iter()
                .any(|owner| owner.allocation == imported && owner.owner_pid == REMOTE_OWNER)
        );
    }

    #[test]
    fn wrong_carrier_bytes_fail_the_session_as_member_failed() {
        let agent = FakeAgent::default();
        inspect(&agent);
        let mut pods = pods();
        pods[1].carrier = Some(0);
        let results = group(&agent, Mode::Prepare, pods, LONG);
        let leader = format!("{:#}", results[0].0.as_ref().unwrap_err());
        assert!(
            leader.contains(
                "rank 1 participant 7: SaveAllocations copied 0 carrier bytes, expected 4096"
            ),
            "{leader}"
        );
        assert_eq!(agent.fails(), [(0, "MemberFailed".to_owned())]);
    }

    #[test]
    fn leader_refusal_fails_the_session_as_refused_only_before_the_commit() {
        // Inspect runs during admission, and Prepare validates again after the commit.
        for (mode, reason) in [(Mode::Inspect, "Refused"), (Mode::Prepare, "MemberFailed")] {
            let agent = FakeAgent::default();
            let mut pods = pods();
            // Rank 2 imports a handle that nobody exported.
            pods[2].records = records(2, 9, 2);
            let results = group(&agent, mode, pods, LONG);
            for (result, _) in &results {
                assert!(result.is_err());
            }
            let leader = format!("{:#}", results[0].0.as_ref().unwrap_err());
            assert!(leader.contains("has no exporter in the group"), "{leader}");
            let follower = format!("{:#}", results[1].0.as_ref().unwrap_err());
            assert!(
                follower.contains(&format!("ABORTED ({reason})")),
                "{follower}"
            );
            assert_eq!(agent.fails(), [(0, reason.to_owned())], "{mode:?}");
        }
    }

    #[test]
    fn follower_step_error_fails_the_session_as_member_failed() {
        let agent = FakeAgent::default();
        inspect(&agent);
        let mut pods = pods();
        pods[1].failing = Some("SaveAllocations");
        let results = group(&agent, Mode::Prepare, pods, LONG);
        assert!(results.iter().all(|(result, _)| result.is_err()));
        let follower = format!("{:#}", results[1].0.as_ref().unwrap_err());
        assert!(follower.contains("participant 7: refused"), "{follower}");
        // The follower and the leader that reads its error reply both fail the session.
        assert!(agent.fails().contains(&(1, "MemberFailed".to_owned())));
        // Nothing runs after the failed round.
        let unicast = |pod: &Scripted| pod.calls.lock().unwrap().contains(&Command::PrepareUnicast);
        assert!(!results.iter().any(|(_, pod)| unicast(pod)));
    }

    #[test]
    fn follower_step_error_outlasts_a_session_that_ended_first() {
        let agent = FakeAgent::default();
        inspect(&agent);
        let mut pods = pods();
        // Rank 2 fails the session while rank 1's step still runs, and then rank 1's step
        // fails too.
        pods[1].slow = Some(("PrepareMulticast", Duration::from_millis(500)));
        for pod in &mut pods[1..] {
            pod.failing = Some("PrepareMulticast");
        }
        let results = group(&agent, Mode::Prepare, pods, LONG);
        let follower = results[1].0.as_ref().unwrap_err();
        assert!(!session::ended(follower), "{follower:#}");
        let follower = format!("{follower:#}");
        assert!(
            follower.contains("whose error reply failed (")
                && follower.contains("ABORTED (MemberFailed)")
                && follower.ends_with("participant 7: refused"),
            "{follower}"
        );
    }

    #[test]
    fn silent_rank_leaves_the_failure_to_the_coordinator_at_the_deadline() {
        let agent = FakeAgent::default();
        inspect(&agent);
        let mut pods = pods();
        pods[2].slow = Some(("PrepareMulticast", Duration::from_secs(2)));
        // The coordinator's clock runs behind the relays', so the session ends after the
        // relays find their deadline passed.
        agent.expire_after(Duration::from_millis(1500));
        let results = group(&agent, Mode::Prepare, pods, Duration::from_secs(1));
        for (result, _) in &results {
            let error = result.as_ref().unwrap_err();
            assert!(session::ended(error), "{error:#}");
            assert!(
                format!("{error:#}").contains("ABORTED (DeadlineExceeded)"),
                "{error:#}"
            );
        }
        let leader = format!("{:#}", results[0].0.as_ref().unwrap_err());
        assert!(
            leader.contains(
                "ranks [2] did not reply to round 3 (PrepareMulticast) before the deadline"
            ),
            "{leader}"
        );
        let follower = format!("{:#}", results[1].0.as_ref().unwrap_err());
        assert!(
            follower.contains("no command for round 4 before the deadline"),
            "{follower}"
        );
        // No relay failed the session, so the coordinator's reason stands.
        assert!(agent.fails().is_empty());
    }

    #[test]
    fn relay_gives_up_on_a_session_that_does_not_end_after_the_deadline() {
        let agent = FakeAgent::default();
        let mut pod = Scripted::new(1);
        // The leader sends no command, and the wait for the session's end runs out soon.
        let group = Group {
            rank: 1,
            relays: vec![0, 1],
            deadline: SystemTime::now() - END_WAIT + Duration::from_millis(500),
        };
        let started = Instant::now();
        let member = Some((Session::new(agent.attach(1)), group));
        let error = run(member, Mode::Inspect, &mut pod).unwrap_err();
        assert!(started.elapsed() >= Duration::from_millis(400));
        assert!(!session::ended(&error), "{error:#}");
        let message = format!("{error:#}");
        assert!(
            message
                .starts_with("no command for round 1 before the deadline: the session did not end"),
            "{message}"
        );
        // A session that did not end is failed as for any other error.
        assert_eq!(agent.fails(), [(1, "Refused".to_owned())]);
    }

    #[test]
    fn first_round_waits_until_the_deadline() {
        let agent = FakeAgent::default();
        // Rank 2 never starts, as when its restore has not finished.
        let mut pods = pods();
        pods.pop();
        agent.expire_after(Duration::from_millis(1500));
        let started = Instant::now();
        let results = group(&agent, Mode::Inspect, pods, Duration::from_secs(1));
        assert!(started.elapsed() >= Duration::from_millis(900));
        let leader = format!("{:#}", results[0].0.as_ref().unwrap_err());
        assert!(
            leader.contains("ranks [2] did not reply to round 1"),
            "{leader}"
        );
    }

    #[test]
    fn closed_session_socket_ends_the_relay() {
        let (relay, agent) = UnixStream::pair().unwrap();
        drop(agent);
        let mut pod = Scripted::new(1);
        let group = Group {
            rank: 1,
            relays: vec![0, 1],
            deadline: SystemTime::now() + LONG,
        };
        let error = run(Some((Session::new(relay), group)), Mode::Inspect, &mut pod).unwrap_err();
        assert!(session::ended(&error), "{error:#}");
        assert!(pod.calls.lock().unwrap().is_empty());
    }

    #[test]
    fn the_lowest_relay_leads_the_group() {
        let group = |rank, relays: &[Rank]| Group {
            rank,
            relays: relays.to_vec(),
            deadline: SystemTime::now(),
        };
        let mut checked = group(1, &[2, 1, 2]);
        checked.check().unwrap();
        assert_eq!(checked.leader(), 1);
        for (mut group, error) in [
            (group(2, &[0, 1]), "does not contain --rank 2"),
            (group(0, &[0, MAX_RANK + 1]), "exceed the largest rank"),
        ] {
            let message = format!("{:#}", group.check().unwrap_err());
            assert!(message.contains(error), "{message}");
        }
    }

    #[test]
    fn commands_and_replies_have_the_documented_shape() {
        let encode = |command: &Command| rmp_serde::to_vec_named(command).unwrap();
        assert_eq!(encode(&Command::Inspect), b"\x81\xa4step\xa7INSPECT");
        assert_eq!(encode(&Command::Done), b"\x81\xa4step\xa4DONE");
        let owner = AllocationOwner {
            allocation: X,
            owner_pid: REMOTE_OWNER,
        };
        let import = ImportKind {
            allocation: X,
            kind: FabricKind::Memory,
        };
        for command in [
            Command::BeginCheckpoint {
                imports: BTreeMap::from([(1, BTreeMap::from([(PID, vec![import])]))]),
            },
            Command::SaveAllocations {
                owners: BTreeMap::from([(1, BTreeMap::from([(PID, vec![owner])]))]),
            },
            Command::RestoreUnicast {
                replacements: vec![FabricReplacement {
                    old: handle(1),
                    new: handle(2),
                }],
            },
        ] {
            let decoded: Command = rmp_serde::from_slice(&encode(&command)).unwrap();
            assert_eq!(decoded, command);
        }
        let saved = BTreeMap::from([(PID, protocol::Reply::Saved { bytes: 4096 })]);
        for reply in [
            Reply::Ok(Outcome::Records {
                current: records(1, 1, 2),
                saved: None,
            }),
            Reply::Ok(Outcome::Replies(saved)),
            Reply::Error("participant 7: refused".into()),
        ] {
            let bytes = rmp_serde::to_vec_named(&reply).unwrap();
            assert!(bytes.starts_with(b"\x82\xa6status"));
            let decoded = rmp_serde::from_slice::<Reply>(&bytes).unwrap();
            // Reply has no PartialEq, and its derived Debug output shows every field.
            assert_eq!(format!("{decoded:?}"), format!("{reply:?}"));
        }
    }
}
