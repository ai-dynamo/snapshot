// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! A relay runs one mode for its Pod as part of a group of Pods. Each step is one round:
//! the leader, the relay of the lowest rank that runs one, inserts the step and its
//! arguments, every relay runs the step on its own processes, and each other relay
//! inserts its reply, which the leader gathers. The leader keeps the group state, so
//! relays only run the steps that they are sent.

use crate::group::{HandleTable, MAX_RANK, Pods, Rank, Topology};
use crate::session::{self, Session};
use anyhow::{Context, Result, bail, ensure};
use cuinterpose_protocol::{
    self as protocol, AllocationOwner, FabricReplacement, Manifest, NamespacePid, Operation,
};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::time::{Duration, SystemTime};

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
    BeginCheckpoint,
    PrepareMulticast,
    SaveAllocations {
        owners: BTreeMap<Rank, BTreeMap<NamespacePid, Vec<AllocationOwner>>>,
    },
    PrepareUnicast,
    LoadAllocations,
    /// \`replacements\` holds every FABRIC handle that restored creators replaced so far.
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
    /// The operation that this command runs on process \`pid\` of \`rank\`, or None when it
    /// gathers records or ends the mode.
    pub fn operation(&self, rank: Rank, pid: NamespacePid) -> Option<Operation> {
        Some(match self {
            Self::Inspect | Self::BeginCheckpoint | Self::Done => return None,
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
            Self::BeginCheckpoint => "BeginCheckpoint",
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

/// \`{status: "ok", result: outcome}\`, or \`{status: "error", result: message}\`.
#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "status", content = "result", rename_all = "snake_case")]
enum Reply {
    Ok(Outcome),
    Error(String),
}

/// This Pod's side of each step.
pub trait Pod {
    /// Run \`command\` on every process of this Pod, which has rank \`rank\`.
    fn run(&mut self, command: &Command, rank: Rank) -> Result<Outcome>;
    /// The mode has ended for every Pod.
    fn finish(&mut self) -> Result<()>;
}

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

struct Relay<'a> {
    /// None for a Pod alone, which is its own leader.
    session: Option<Session>,
    group: Group,
    mode: Mode,
    pod: &'a mut dyn Pod,
    round: u32,
}

impl Relay<'_> {
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
            // At the deadline the coordinator fails the session, so a follower that waited
            // that long leaves the failure to it.
            let wait = self.group.until_deadline();
            let Some(value) = self.session().get(&key, wait)? else {
                return Err(session::Ended(format!(
                    "no command for round {round} before the deadline"
                ))
                .into());
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
            // At the deadline the coordinator fails the session, so its reason stands.
            return Err(session::Ended(format!(
                "ranks {missing:?} did not reply to round {round} ({command}) before the deadline"
            ))
            .into());
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
                Topology::validate(&pods)?;
            }
            Mode::Prepare => {
                // Inspect does not freeze registries, so the topology is validated again
                // after BeginCheckpoint does. It freezes registries, not GPU work. From
                // here preparation is fail-stop: callers must abandon the checkpoint and
                // every source process.
                let (pods, _) = self.records(Command::BeginCheckpoint)?;
                let topology = Topology::validate(&pods)?;
                self.execute(&topology, Command::PrepareMulticast)?;
                let owners = pods
                    .iter()
                    .map(|(rank, pod)| {
                        let owners = pod
                            .keys()
                            .map(|pid| (*pid, topology.owners(*rank, *pid)))
                            .collect();
                        (*rank, owners)
                    })
                    .collect();
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
                let (restored, _) = self.records(Command::Inspect)?;
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
        AllocationReference, FABRIC_HANDLE_BYTES, FabricHandle, MulticastProperties, Record,
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
                },
                Record::FabricExport {
                    allocation: T,
                    handle: handle(multicast_handle),
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
                },
                Record::FabricImport {
                    allocation: object,
                    handle: handle(multicast_handle),
                },
            ]);
        }
        Manifest::from([(PID, records)])
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
        // The leader removes each round's keys and leaves only the end of each mode.
        assert_eq!(
            agent.keys(),
            ["inspect.2.cmd", "prepare.5.cmd", "restore.9.cmd"]
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
    fn save_owners_name_each_allocation_by_its_holders_own_reference() {
        let agent = FakeAgent::default();
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
    fn silent_rank_leaves_the_failure_to_the_deadline() {
        let agent = FakeAgent::default();
        let mut pods = pods();
        pods[2].slow = Some(("PrepareMulticast", Duration::from_secs(2)));
        let started = Instant::now();
        let results = group(&agent, Mode::Prepare, pods, Duration::from_secs(1));
        assert!(started.elapsed() >= Duration::from_millis(900));
        for (result, _) in &results {
            let error = result.as_ref().unwrap_err();
            assert!(session::ended(error), "{error:#}");
        }
        let leader = format!("{:#}", results[0].0.as_ref().unwrap_err());
        assert!(
            leader.contains(
                "ranks [2] did not reply to round 2 (PrepareMulticast) before the deadline"
            ),
            "{leader}"
        );
        // The coordinator fails the session at the deadline with its own reason.
        assert!(agent.fails().is_empty());
    }

    #[test]
    fn first_round_waits_until_the_deadline() {
        let agent = FakeAgent::default();
        // Rank 2 never starts, as when its restore has not finished.
        let mut pods = pods();
        pods.pop();
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
        for command in [
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
