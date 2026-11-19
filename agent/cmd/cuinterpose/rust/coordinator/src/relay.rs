// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! A relay runs one mode for its Pod. Each step is one round, which runs the step on the
//! Pod's processes. The leader checks every round against the group's topology and keeps
//! the group state between rounds, such as the FABRIC handles that restored creators
//! replaced. A Pod alone leads a group of one.

use crate::group::{HandleTable, Pods, Rank, Topology};
use anyhow::{Context, Result, bail, ensure};
use cuinterpose_protocol::{
    self as protocol, AllocationOwner, FabricReplacement, ImportKind, Manifest, NamespacePid,
    Operation,
};
use std::collections::BTreeMap;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Mode {
    Inspect,
    Prepare,
    Restore,
}

/// One round's command: the step that every relay runs on its own processes, with the
/// arguments of that step. Arguments meant for one process are
/// keyed by its rank and PID, and each relay uses its own.
#[derive(Clone, Debug, PartialEq, Eq)]
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
}

impl Command {
    /// The operation that this command runs on process `pid` of `rank`, or None when it
    /// gathers records.
    pub fn operation(&self, rank: Rank, pid: NamespacePid) -> Option<Operation> {
        Some(match self {
            Self::Inspect | Self::BeginCheckpoint { .. } => return None,
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
        })
    }
}

/// What a Pod's processes returned from one step.
#[derive(Clone, Debug)]
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

/// This Pod's side of each step.
pub trait Pod {
    /// Run `command` on every process of this Pod, which has rank `rank`. The relay names
    /// the step in an error, so the Pod names only its processes.
    fn run(&mut self, command: &Command, rank: Rank) -> Result<Outcome>;
    /// The mode has ended for every Pod.
    fn finish(&mut self) -> Result<()>;
}

/// Run `mode` for this Pod, which leads a group of one.
pub fn run(mode: Mode, pod: &mut dyn Pod) -> Result<()> {
    Relay { mode, pod }.lead()
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

struct Relay<'a> {
    mode: Mode,
    pod: &'a mut dyn Pod,
}

impl Relay<'_> {
    /// Run one command on this Pod, the only Pod of its group.
    fn round(&mut self, command: &Command) -> Result<BTreeMap<Rank, Outcome>> {
        let own = self
            .pod
            .run(command, 0)
            .with_context(|| format!("{command}"))?;
        Ok(BTreeMap::from([(0, own)]))
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
        self.pod.finish()
    }
}
