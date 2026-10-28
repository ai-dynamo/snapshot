// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! A relay runs one mode for its Pod as part of a group of Pods. Each step is one round:
//! the leader, the relay of the lowest rank that runs one, inserts the step and its
//! arguments, every relay runs the step on its own processes, and each other relay
//! inserts its reply, which the leader gathers. The leader keeps the group state, so
//! relays only run the steps that they are sent.

use crate::group::{CarrierPlan, HandleTable, MAX_RANK, Pods, Rank, Topology};
use crate::session::{self, Session};
use anyhow::{Context, Result, bail, ensure};
use cuinterpose_protocol::{FabricUpdate, Manifest, NamespacePid, Operation};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::time::{Duration, Instant, SystemTime};

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

/// The step that a round runs on every Pod.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum Step {
    Inspect,
    BeginCheckpoint,
    PrepareMulticast,
    SaveAllocations,
    PrepareUnicast,
    LoadAllocations,
    RestoreUnicast,
    RestoreMulticastCreators,
    RestoreMulticastImporters,
    RestoreMulticastDevices,
    RestoreMulticastBindings,
}

impl Step {
    /// The lifecycle operation that this step runs on each process, if it is one.
    pub fn operation(self) -> Option<Operation> {
        Some(match self {
            Self::Inspect | Self::BeginCheckpoint => return None,
            Self::PrepareMulticast => Operation::PrepareMulticast,
            Self::SaveAllocations => Operation::SaveAllocations,
            Self::PrepareUnicast => Operation::PrepareUnicast,
            Self::LoadAllocations => Operation::LoadAllocations,
            Self::RestoreUnicast => Operation::RestoreUnicast,
            Self::RestoreMulticastCreators => Operation::RestoreMulticastCreators,
            Self::RestoreMulticastImporters => Operation::RestoreMulticastImporters,
            Self::RestoreMulticastDevices => Operation::RestoreMulticastDevices,
            Self::RestoreMulticastBindings => Operation::RestoreMulticastBindings,
        })
    }
}

/// A step's arguments. Values meant for one rank are keyed by rank, and each relay uses
/// its own.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Args {
    /// SaveAllocations and LoadAllocations: what each process copies, by rank and PID.
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub plans: BTreeMap<Rank, BTreeMap<NamespacePid, CarrierPlan>>,
    /// Restore steps after LoadAllocations: the old and new FABRIC handles so far.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub imports: Vec<FabricUpdate>,
}

impl Args {
    fn is_empty(&self) -> bool {
        self.plans.is_empty() && self.imports.is_empty()
    }
}

/// One round's command, `{step, args}`, or `{done: true}` at the end of the mode.
#[derive(Debug, Default, PartialEq, Serialize, Deserialize)]
struct Command {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    step: Option<Step>,
    #[serde(default, skip_serializing_if = "Args::is_empty")]
    args: Args,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    done: bool,
}

/// What a Pod's processes returned from one step.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Outcome {
    /// Inspect and BeginCheckpoint: the Pod's current records.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub records: Option<Manifest>,
    /// Inspect at restore: the records that the Pod saved at capture.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub saved: Option<Manifest>,
    /// FABRIC handles that the Pod's creators exported in place of checkpointed ones.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub exports: Vec<FabricUpdate>,
}

/// `{status: "ok", result: outcome}`, or `{status: "error", result: message}`.
#[derive(Debug, PartialEq, Serialize, Deserialize)]
#[serde(tag = "status", content = "result", rename_all = "snake_case")]
enum Reply {
    Ok(Outcome),
    Error(String),
}

/// This Pod's side of each step.
pub trait Pod {
    /// Run `step` on every process of this Pod, with this Pod's plans.
    fn run(
        &mut self,
        step: Step,
        plans: &BTreeMap<NamespacePid, CarrierPlan>,
        imports: &[FabricUpdate],
    ) -> Result<Outcome>;

    /// The mode has ended for every Pod.
    fn finish(&mut self) -> Result<()>;
}

/// This relay's place in the group, as its agent passes it.
pub struct Group {
    pub rank: Rank,
    /// Every rank that runs a relay. The lowest leads the group and gathers from the rest.
    pub relays: Vec<Rank>,
    /// Bounds each gather after the first.
    pub round_timeout: Duration,
    /// The attempt's deadline.
    pub deadline: SystemTime,
}

impl Group {
    /// A Pod alone. It waits for no other relay, so it needs no timeouts.
    fn alone() -> Self {
        Self {
            rank: 0,
            relays: vec![0],
            round_timeout: Duration::ZERO,
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

/// The leader refused the group's topology.
#[derive(Debug)]
struct Refused;

impl std::fmt::Display for Refused {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("the leader refused the group topology")
    }
}

impl std::error::Error for Refused {}

/// Run `mode` for this Pod, in its group when `member` names one and alone otherwise. A
/// Pod alone leads a group of one, without a session. In a group, any failure ends the
/// attempt for every member, as `Refused` when the leader refuses the topology and
/// `MemberFailed` otherwise.
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
        let reason = if error.downcast_ref::<Refused>().is_some() {
            "Refused"
        } else {
            "MemberFailed"
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

    fn run_step(&mut self, step: Step, args: &Args) -> Result<Outcome> {
        let plans = args
            .plans
            .get(&self.group.rank)
            .cloned()
            .unwrap_or_default();
        self.pod.run(step, &plans, &args.imports)
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
            if command.done {
                return self.pod.finish();
            }
            let step = command
                .step
                .with_context(|| format!("the command for round {round} has no step"))?;
            let result = self.run_step(step, &command.args);
            let reply = match &result {
                Ok(outcome) => Reply::Ok(outcome.clone()),
                Err(error) => Reply::Error(format!("{error:#}")),
            };
            let key = self.reply_key(round, self.group.rank);
            self.session()
                .insert(&key, &rmp_serde::to_vec_named(&reply)?)?;
            result.with_context(|| format!("round {round} ({step:?})"))?;
            self.round += 1;
        }
    }

    /// Insert one step, run it here, and gather every other relay's outcome. A Pod alone
    /// only runs it here.
    fn round(&mut self, step: Step, args: Args) -> Result<BTreeMap<Rank, Outcome>> {
        let round = self.round;
        self.round += 1;
        let started = Instant::now();
        let command = Command {
            step: Some(step),
            args,
            done: false,
        };
        let key = self.command_key(round);
        if let Some(session) = &mut self.session {
            session.insert(&key, &rmp_serde::to_vec_named(&command)?)?;
        }
        let own = self
            .run_step(step, &command.args)
            .with_context(|| format!("{step:?}"))?;
        let mut outcomes = BTreeMap::from([(self.group.rank, own)]);
        // At restore each relay starts after its own CRIU and CUDA restore, so only the
        // deadline bounds the first round.
        let wait = if round == 1 {
            self.group.until_deadline()
        } else {
            self.group.round_timeout.saturating_sub(started.elapsed())
        };
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
        let values = session.get_all(&keys, wait)?;
        let missing: Vec<Rank> = others
            .iter()
            .zip(&values)
            .filter(|(_, value)| value.is_none())
            .map(|(rank, _)| *rank)
            .collect();
        ensure!(
            missing.is_empty(),
            "ranks {missing:?} did not reply to round {round} ({step:?}) in time"
        );
        for (rank, value) in others.into_iter().zip(values) {
            let reply: Reply = rmp_serde::from_slice(&value.unwrap_or_default())
                .with_context(|| format!("decode the reply of rank {rank} to round {round}"))?;
            match reply {
                Reply::Ok(outcome) => outcomes.insert(rank, outcome),
                Reply::Error(message) => bail!("rank {rank}: {step:?}: {message}"),
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

    fn records(&mut self, step: Step) -> Result<(Pods, Pods)> {
        let mut current = Pods::new();
        let mut saved = Pods::new();
        for (rank, outcome) in self.round(step, Args::default())? {
            current.insert(
                rank,
                outcome
                    .records
                    .with_context(|| format!("rank {rank} sent no records"))?,
            );
            if let Some(records) = outcome.saved {
                saved.insert(rank, records);
            }
        }
        Ok((current, saved))
    }

    fn plans(
        &self,
        topology: &Topology,
        pods: &Pods,
        operation: Operation,
    ) -> Result<BTreeMap<Rank, BTreeMap<NamespacePid, CarrierPlan>>> {
        pods.iter()
            .map(|(rank, pod)| {
                let plans = pod
                    .keys()
                    .map(|pid| {
                        topology
                            .plan(operation, *rank, *pid)
                            .with_context(|| {
                                format!("{operation:?}: rank {rank} participant {pid}")
                            })
                            .map(|plan| (*pid, plan))
                    })
                    .collect::<Result<_>>()?;
                Ok((*rank, plans))
            })
            .collect()
    }

    /// Run a lifecycle step on every Pod and return the FABRIC handles that they exported.
    fn execute(&mut self, step: Step, args: Args) -> Result<Vec<FabricUpdate>> {
        Ok(self
            .round(step, args)?
            .into_values()
            .flat_map(|outcome| outcome.exports)
            .collect())
    }

    fn lead(&mut self) -> Result<()> {
        let refused = |error: anyhow::Error| error.context(Refused);
        match self.mode {
            Mode::Inspect => {
                let (pods, _) = self.records(Step::Inspect)?;
                Topology::validate(&pods).map_err(refused)?;
            }
            Mode::Prepare => {
                // Inspect does not freeze registries, so the topology is validated again
                // after BeginCheckpoint does. It freezes registries, not GPU work. From
                // here preparation is fail-stop: callers must abandon the checkpoint and
                // every source process.
                let (pods, _) = self.records(Step::BeginCheckpoint)?;
                let topology = Topology::validate(&pods).map_err(refused)?;
                self.execute(Step::PrepareMulticast, Args::default())?;
                let plans = self.plans(&topology, &pods, Operation::SaveAllocations)?;
                self.execute(
                    Step::SaveAllocations,
                    Args {
                        plans,
                        ..Args::default()
                    },
                )?;
                self.execute(Step::PrepareUnicast, Args::default())?;
            }
            Mode::Restore => {
                let (current, saved) = self.records(Step::Inspect)?;
                ensure!(
                    saved.keys().eq(current.keys()),
                    "ranks {:?} sent no saved records",
                    current
                        .keys()
                        .filter(|rank| !saved.contains_key(rank))
                        .collect::<Vec<_>>()
                );
                Topology::validate(&current).map_err(refused)?;
                let topology = Topology::validate(&saved).map_err(refused)?;
                let plans = self.plans(&topology, &saved, Operation::LoadAllocations)?;
                // Importers need the FABRIC handles that creators export in the step
                // before, in any Pod.
                let mut handles = HandleTable::default();
                handles.record(self.execute(
                    Step::LoadAllocations,
                    Args {
                        plans,
                        ..Args::default()
                    },
                )?)?;
                for step in [
                    Step::RestoreUnicast,
                    Step::RestoreMulticastCreators,
                    Step::RestoreMulticastImporters,
                    Step::RestoreMulticastDevices,
                    Step::RestoreMulticastBindings,
                ] {
                    let imports = handles.imports();
                    let args = Args {
                        imports,
                        ..Args::default()
                    };
                    handles.record(self.execute(step, args)?)?;
                }
                let (restored, _) = self.records(Step::Inspect)?;
                crate::group::verify_restored(&saved, &restored, &handles)?;
            }
        }
        // The leader publishes its own state before it lets the others finish.
        self.pod.finish()?;
        let done = Command {
            done: true,
            ..Command::default()
        };
        let key = self.command_key(self.round);
        if let Some(session) = &mut self.session {
            session.insert(&key, &rmp_serde::to_vec_named(&done)?)?;
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

    type Calls = Arc<Mutex<Vec<(Step, BTreeMap<NamespacePid, CarrierPlan>, Vec<FabricUpdate>)>>>;

    /// One Pod whose shims answer as scripted.
    struct Scripted {
        rank: Rank,
        records: Manifest,
        saved: Option<Manifest>,
        restored: bool,
        failing: Option<Step>,
        slow: Option<(Step, Duration)>,
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
                calls: Calls::default(),
                finished: false,
            }
        }
    }

    impl Pod for Scripted {
        fn run(
            &mut self,
            step: Step,
            plans: &BTreeMap<NamespacePid, CarrierPlan>,
            imports: &[FabricUpdate],
        ) -> Result<Outcome> {
            self.calls
                .lock()
                .unwrap()
                .push((step, plans.clone(), imports.to_vec()));
            if let Some((slow, delay)) = self.slow
                && slow == step
            {
                std::thread::sleep(delay);
            }
            if self.failing == Some(step) {
                bail!("participant {PID}: refused");
            }
            if step.operation().is_none() {
                let records = if self.restored {
                    records(self.rank, restored(1), restored(2))
                } else {
                    self.records.clone()
                };
                return Ok(Outcome {
                    records: Some(records),
                    saved: self.saved.clone(),
                    ..Outcome::default()
                });
            }
            let export = |byte| FabricUpdate {
                old: handle(byte),
                new: handle(restored(byte)),
            };
            let exports = match (self.rank, step) {
                (0, Step::LoadAllocations) => vec![export(1)],
                (0, Step::RestoreMulticastCreators) => vec![export(2)],
                _ => Vec::new(),
            };
            self.restored |= step == Step::RestoreMulticastBindings;
            Ok(Outcome {
                exports,
                ..Outcome::default()
            })
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
        round_timeout: Duration,
    ) -> Vec<(Result<()>, Scripted)> {
        let relays: Vec<Rank> = vec![0, 1, 2];
        let threads: Vec<_> = pods
            .into_iter()
            .map(|mut pod| {
                let session = Session::new(agent.attach(pod.rank));
                let group = Group {
                    rank: pod.rank,
                    relays: relays.clone(),
                    round_timeout,
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
            for (result, pod) in group(&agent, mode, pods(), LONG, LONG) {
                result.unwrap();
                assert!(pod.finished);
            }
        }
        let mut restore = pods();
        for pod in &mut restore {
            pod.saved = Some(pod.records.clone());
        }
        let calls: Vec<_> = restore.iter().map(|pod| Arc::clone(&pod.calls)).collect();
        for (result, pod) in group(&agent, Mode::Restore, restore, LONG, LONG) {
            result.unwrap();
            assert!(pod.finished && pod.restored);
        }
        assert!(agent.fails().is_empty());
        // The leader removes each round's keys and leaves only the end of each mode.
        assert_eq!(
            agent.keys(),
            ["inspect.2.cmd", "prepare.5.cmd", "restore.9.cmd"]
        );
        let steps: Vec<Step> = calls[1].lock().unwrap().iter().map(|call| call.0).collect();
        assert_eq!(
            steps,
            [
                Step::Inspect,
                Step::LoadAllocations,
                Step::RestoreUnicast,
                Step::RestoreMulticastCreators,
                Step::RestoreMulticastImporters,
                Step::RestoreMulticastDevices,
                Step::RestoreMulticastBindings,
                Step::Inspect,
            ]
        );
        // Each rank loads only its own member, and rank 0 also loads X.
        for (rank, calls) in calls.iter().enumerate() {
            let calls = calls.lock().unwrap();
            let load = &calls[1].1;
            assert_eq!(load.keys().collect::<Vec<_>>(), [&PID]);
            assert_eq!(load[&PID].bytes, if rank == 0 { 8192 } else { 4096 });
            // Importers get the handles that the creator exported in earlier rounds.
            let updates = |calls: &[(Step, _, Vec<FabricUpdate>)], step| {
                calls
                    .iter()
                    .find(|call| call.0 == step)
                    .unwrap()
                    .2
                    .iter()
                    .map(|update| update.old)
                    .collect::<Vec<_>>()
            };
            assert_eq!(updates(&calls, Step::RestoreUnicast), [handle(1)]);
            assert_eq!(
                updates(&calls, Step::RestoreMulticastImporters),
                [handle(1), handle(2)]
            );
        }
    }

    #[test]
    fn save_plans_name_owners_in_each_pods_own_references() {
        let agent = FakeAgent::default();
        let pods = pods();
        let calls: Vec<_> = pods.iter().map(|pod| Arc::clone(&pod.calls)).collect();
        for (result, _) in group(&agent, Mode::Prepare, pods, LONG, LONG) {
            result.unwrap();
        }
        let save = |rank: usize| {
            calls[rank]
                .lock()
                .unwrap()
                .iter()
                .find(|call| call.0 == Step::SaveAllocations)
                .unwrap()
                .1[&PID]
                .clone()
        };
        assert_eq!(save(0).bytes, 8192);
        assert!(save(0).owners.iter().all(|owner| owner.owner_pid == PID));
        let imported = AllocationReference {
            id: [21; 16],
            creator_pid: 0,
        };
        assert_eq!(save(1).bytes, 4096);
        assert!(
            save(1)
                .owners
                .iter()
                .any(|owner| owner.allocation == imported && owner.owner_pid == REMOTE_OWNER)
        );
    }

    #[test]
    fn leader_refusal_fails_the_session_as_refused() {
        let agent = FakeAgent::default();
        let mut pods = pods();
        // Rank 2 imports a handle that nobody exported.
        pods[2].records = records(2, 9, 2);
        let results = group(&agent, Mode::Inspect, pods, LONG, LONG);
        for (result, _) in &results {
            assert!(result.is_err());
        }
        let leader = format!("{:#}", results[0].0.as_ref().unwrap_err());
        assert!(leader.contains("has no exporter in the group"), "{leader}");
        let follower = format!("{:#}", results[1].0.as_ref().unwrap_err());
        assert!(follower.contains("ABORTED (Refused)"), "{follower}");
        assert_eq!(agent.fails(), [(0, "Refused".to_owned())]);
    }

    #[test]
    fn follower_step_error_fails_the_session_as_member_failed() {
        let agent = FakeAgent::default();
        let mut pods = pods();
        pods[1].failing = Some(Step::SaveAllocations);
        let results = group(&agent, Mode::Prepare, pods, LONG, LONG);
        assert!(results.iter().all(|(result, _)| result.is_err()));
        let follower = format!("{:#}", results[1].0.as_ref().unwrap_err());
        assert!(follower.contains("participant 7: refused"), "{follower}");
        // The follower and the leader that reads its error reply both fail the session.
        assert!(agent.fails().contains(&(1, "MemberFailed".to_owned())));
        // Nothing runs after the failed round.
        let unicast = |pod: &Scripted| {
            pod.calls
                .lock()
                .unwrap()
                .iter()
                .any(|call| call.0 == Step::PrepareUnicast)
        };
        assert!(!results.iter().any(|(_, pod)| unicast(pod)));
    }

    #[test]
    fn silent_rank_fails_the_session_when_its_round_times_out() {
        let agent = FakeAgent::default();
        let mut pods = pods();
        pods[2].slow = Some((Step::PrepareMulticast, Duration::from_secs(2)));
        let results = group(
            &agent,
            Mode::Prepare,
            pods,
            LONG,
            Duration::from_millis(200),
        );
        assert!(results.iter().all(|(result, _)| result.is_err()));
        let leader = format!("{:#}", results[0].0.as_ref().unwrap_err());
        assert!(
            leader.contains("ranks [2] did not reply to round 2 (PrepareMulticast) in time"),
            "{leader}"
        );
        assert_eq!(agent.fails(), [(0, "MemberFailed".to_owned())]);
    }

    #[test]
    fn first_round_waits_until_the_deadline() {
        let agent = FakeAgent::default();
        // Rank 2 never starts, as when its restore has not finished.
        let mut pods = pods();
        pods.pop();
        let started = Instant::now();
        let results = group(
            &agent,
            Mode::Inspect,
            pods,
            Duration::from_secs(1),
            Duration::from_millis(1),
        );
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
            round_timeout: LONG,
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
            round_timeout: LONG,
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
        let inspect = Command {
            step: Some(Step::Inspect),
            ..Command::default()
        };
        assert_eq!(encode(&inspect), b"\x81\xa4step\xa7INSPECT");
        let done = Command {
            done: true,
            ..Command::default()
        };
        assert_eq!(encode(&done), b"\x81\xa4done\xc3");
        let restore = Command {
            step: Some(Step::RestoreUnicast),
            args: Args {
                imports: vec![FabricUpdate {
                    old: handle(1),
                    new: handle(2),
                }],
                ..Args::default()
            },
            done: false,
        };
        let decoded: Command = rmp_serde::from_slice(&encode(&restore)).unwrap();
        assert_eq!(decoded, restore);
        for reply in [
            Reply::Ok(Outcome {
                records: Some(records(1, 1, 2)),
                ..Outcome::default()
            }),
            Reply::Error("participant 7: refused".into()),
        ] {
            let bytes = rmp_serde::to_vec_named(&reply).unwrap();
            assert!(bytes.starts_with(b"\x82\xa6status"));
            assert_eq!(rmp_serde::from_slice::<Reply>(&bytes).unwrap(), reply);
        }
    }
}
