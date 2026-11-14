// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The Pods of a group validate as one topology. Each Pod names its processes by PID in
//! its own namespace, so a group PID puts the Pod's rank above every Linux PID. A FABRIC
//! import does not name its creator, so it resolves to the process that exported the
//! same handle, in any Pod. A Pod alone is a group of one, which validates exactly as a
//! single Pod always has.

use crate::topology::{self, AllocationSummary};
use anyhow::{Context, Result, bail, ensure};
use cuinterpose_protocol::{
    AllocationId, AllocationOwner, AllocationReference, BindingSource, FabricHandle,
    FabricReplacement, Manifest, MemberRange, MemoryLocation, MulticastProperties, NamespacePid,
    Operation, Record, Reply,
};
use std::collections::BTreeMap;

pub type Rank = u32;

/// Every Pod's records by rank, each in the Pod's own PIDs and references.
pub type Pods = BTreeMap<Rank, Manifest>;

/// Linux PIDs stay below 2^22 (`PID_MAX_LIMIT`), so the rank fills the bits above them.
/// Rank 0's group PIDs equal its own PIDs.
const RANK_SHIFT: u32 = 22;
pub const MAX_RANK: Rank = (1 << (NamespacePid::BITS - RANK_SHIFT)) - 1;

/// The owner named to a holder in another Pod. It is never a PID, and holders only
/// compare an owner with their own PID.
pub const REMOTE_OWNER: NamespacePid = NamespacePid::MAX;

fn group_pid(rank: Rank, pid: NamespacePid) -> NamespacePid {
    rank << RANK_SHIFT | pid
}

fn rank_of(group_pid: NamespacePid) -> Rank {
    group_pid >> RANK_SHIFT
}

fn local_pid(group_pid: NamespacePid) -> NamespacePid {
    group_pid & ((1 << RANK_SHIFT) - 1)
}

/// A Pod alone, as a group of one.
pub fn alone(manifest: Manifest) -> Pods {
    Pods::from([(0, manifest)])
}

/// A group's records, validated as one topology.
pub struct Topology {
    allocations: Vec<AllocationSummary>,
    /// For each process by group PID, its own reference to each allocation it holds,
    /// keyed by the allocation's ID in the group.
    references: BTreeMap<NamespacePid, BTreeMap<AllocationId, AllocationReference>>,
    /// The bytes that each process, by group PID, copies to and from its host carrier:
    /// the size of every host-carrier allocation that it owns.
    carrier_bytes: BTreeMap<NamespacePid, u64>,
    /// Whether the topology spans Pods, so messages name each process's rank.
    grouped: bool,
}

impl Topology {
    /// Resolve every Pod's records into group identities and validate them as one
    /// topology. FABRIC importers take the creator's allocation size, location, and
    /// multicast properties, which they cannot observe themselves.
    pub fn validate(pods: &Pods) -> Result<Self> {
        let creators = Creators::collect(pods)?;
        let mut group = Manifest::new();
        let mut references = BTreeMap::new();
        for (&rank, pod) in pods {
            ensure!(
                rank <= MAX_RANK,
                "rank {rank} exceeds the largest rank {MAX_RANK}"
            );
            for (&pid, records) in pod {
                ensure!(
                    pid != 0 && pid >> RANK_SHIFT == 0,
                    "rank {rank}: namespace PID {pid} is not a Linux PID"
                );
                let process = group_pid(rank, pid);
                let (resolved, held) = creators.resolve(rank, process, records)?;
                group.insert(process, resolved);
                references.insert(process, held);
            }
        }
        let grouped = pods.len() > 1;
        let mut validated = topology::validate(&group);
        if grouped {
            validated =
                validated.context("group topology, where participant IDs are (rank << 22) | PID");
        }
        let allocations = validated?;
        let mut carrier_bytes: BTreeMap<_, u64> =
            group.keys().map(|process| (*process, 0)).collect();
        for allocation in allocations.iter().filter(|a| a.uses_host_carrier()) {
            let bytes = carrier_bytes.entry(allocation.owner_pid).or_default();
            *bytes = bytes.checked_add(allocation.size).with_context(|| {
                format!(
                    "{}: carrier size overflow",
                    participant(grouped, allocation.owner_pid)
                )
            })?;
        }
        Ok(Self {
            allocations,
            references,
            carrier_bytes,
            grouped,
        })
    }

    /// The owner of each host-carrier allocation that `pid` in `rank` holds, by that
    /// process's own reference, for SaveAllocations. An owner in another Pod is
    /// `REMOTE_OWNER`.
    pub fn owners(&self, rank: Rank, pid: NamespacePid) -> Vec<AllocationOwner> {
        let process = group_pid(rank, pid);
        self.allocations
            .iter()
            .filter(|a| a.uses_host_carrier() && a.holders.contains(&process))
            .map(|allocation| AllocationOwner {
                allocation: self.references[&process][&allocation.reference.id],
                owner_pid: if rank_of(allocation.owner_pid) == rank {
                    local_pid(allocation.owner_pid)
                } else {
                    REMOTE_OWNER
                },
            })
            .collect()
    }

    /// Check what `pid` in `rank` replied to `operation`, including the bytes that it
    /// copied to or from its host carrier, and return the FABRIC handles that it exported
    /// in place of checkpointed ones.
    pub fn completed(
        &self,
        rank: Rank,
        pid: NamespacePid,
        operation: &Operation,
        reply: Reply,
    ) -> Result<Vec<FabricReplacement>> {
        let process = group_pid(rank, pid);
        let participant = participant(self.grouped, process);
        let copied = |bytes: u64| {
            let expected = self
                .carrier_bytes
                .get(&process)
                .with_context(|| format!("{participant} is not in the topology"))?;
            ensure!(
                bytes == *expected,
                "{participant}: {operation} copied {bytes} carrier bytes, expected {expected}"
            );
            Ok(())
        };
        match (operation, reply) {
            (Operation::SaveAllocations { .. }, Reply::Saved { bytes }) => {
                copied(bytes)?;
                Ok(Vec::new())
            }
            (
                Operation::LoadAllocations,
                Reply::Loaded {
                    bytes,
                    replacements,
                },
            ) => {
                copied(bytes)?;
                Ok(replacements)
            }
            (Operation::RestoreMulticastCreators, Reply::MulticastRecreated { replacements }) => {
                Ok(replacements)
            }
            (
                Operation::PrepareMulticast
                | Operation::PrepareUnicast
                | Operation::RestoreUnicast { .. }
                | Operation::RestoreMulticastImporters { .. }
                | Operation::RestoreMulticastDevices
                | Operation::RestoreMulticastBindings,
                Reply::Completed,
            ) => Ok(Vec::new()),
            (_, reply) => bail!("{participant}: unexpected reply to {operation}: {reply:?}"),
        }
    }
}

/// How messages name a process: by its PID alone, or in a group by its rank too.
fn participant(grouped: bool, process: NamespacePid) -> String {
    if grouped {
        format!(
            "rank {} participant {}",
            rank_of(process),
            local_pid(process)
        )
    } else {
        format!("participant {process}")
    }
}

/// What creators record and their importers cannot observe.
#[derive(Default)]
struct Creators {
    /// The creator of each exported FABRIC handle, in group identities.
    exporters: BTreeMap<FabricHandle, AllocationReference>,
    allocations: BTreeMap<AllocationId, (u64, MemoryLocation)>,
    multicasts: BTreeMap<AllocationId, MulticastProperties>,
}

impl Creators {
    fn collect(pods: &Pods) -> Result<Self> {
        let mut creators = Self::default();
        for (&rank, pod) in pods {
            for (&pid, records) in pod {
                let process = group_pid(rank, pid);
                for record in records {
                    match record {
                        Record::FabricExport { allocation, handle } => {
                            ensure!(
                                allocation.creator_pid == pid,
                                "participant {process}: fabric export of {allocation:?} by a process that did not create it"
                            );
                            let creator = AllocationReference {
                                creator_pid: process,
                                ..*allocation
                            };
                            if let Some(known) = creators.exporters.insert(*handle, creator)
                                && known != creator
                            {
                                bail!("fabric handle {handle:?} is exported by two creators");
                            }
                        }
                        Record::Allocation {
                            allocation,
                            size,
                            location,
                            ..
                        } if allocation.creator_pid == pid => {
                            creators
                                .allocations
                                .insert(allocation.id, (*size, *location));
                        }
                        Record::Multicast {
                            allocation,
                            properties,
                            ..
                        } if allocation.creator_pid == pid => {
                            creators.multicasts.insert(allocation.id, *properties);
                        }
                        _ => {}
                    }
                }
            }
        }
        Ok(creators)
    }

    /// Resolve the records of `process`, in `rank`, into group identities. Returns the
    /// resolved records, without the FABRIC records that joined them, and the process's
    /// own reference to each allocation it holds, by the allocation's ID in the group.
    fn resolve(
        &self,
        rank: Rank,
        process: NamespacePid,
        records: &[Record],
    ) -> Result<(Vec<Record>, BTreeMap<AllocationId, AllocationReference>)> {
        let mut imports = BTreeMap::new();
        for record in records {
            if let Record::FabricImport { allocation, handle } = record {
                ensure!(
                    allocation.creator_pid == 0,
                    "participant {process}: fabric import {allocation:?} names a creator"
                );
                let creator = self.exporters.get(handle).with_context(|| {
                    format!(
                        "participant {process}: fabric import {handle:?} has no exporter in the group"
                    )
                })?;
                imports.insert(*allocation, *creator);
            }
        }
        let resolve = |reference: AllocationReference| match imports.get(&reference) {
            Some(creator) => *creator,
            // A creator PID of 0 without a fabric import stays unresolved, and
            // validation reports its missing creator.
            None if reference.creator_pid == 0 => reference,
            None => AllocationReference {
                creator_pid: group_pid(rank, reference.creator_pid),
                ..reference
            },
        };
        let mut resolved = Vec::with_capacity(records.len());
        let mut held = BTreeMap::new();
        for record in records {
            if matches!(
                record,
                Record::FabricExport { .. } | Record::FabricImport { .. }
            ) {
                continue;
            }
            let mut record = record.clone();
            let local = rewrite(&mut record, &resolve);
            // An importer cannot observe what its creator recorded, so it takes that.
            let imported = imports.contains_key(&local);
            match &mut record {
                Record::Allocation {
                    allocation,
                    size,
                    location,
                    ..
                } => {
                    if imported
                        && let Some((creator_size, creator_location)) =
                            self.allocations.get(&allocation.id)
                    {
                        *size = *creator_size;
                        *location = *creator_location;
                    }
                    held.insert(allocation.id, local);
                }
                Record::Multicast {
                    allocation,
                    properties,
                    ..
                } if imported => {
                    if let Some(creator) = self.multicasts.get(&allocation.id) {
                        *properties = *creator;
                    }
                }
                _ => {}
            }
            resolved.push(record);
        }
        Ok((resolved, held))
    }
}

/// Replace every allocation reference in `record`, and return the record's own
/// allocation reference from before.
fn rewrite(
    record: &mut Record,
    resolve: &impl Fn(AllocationReference) -> AllocationReference,
) -> AllocationReference {
    let range = |range: &mut MemberRange| range.allocation = resolve(range.allocation);
    let allocation = match record {
        Record::Allocation { allocation, .. }
        | Record::Mapping { allocation, .. }
        | Record::Multicast { allocation, .. }
        | Record::MulticastDevice { allocation, .. }
        | Record::MulticastMapping { allocation, .. }
        | Record::FabricExport { allocation, .. }
        | Record::FabricImport { allocation, .. } => allocation,
        Record::MulticastBinding {
            allocation, source, ..
        } => {
            match source {
                BindingSource::Memory(member) => range(member),
                BindingSource::Address { tracked_member, .. } => {
                    tracked_member.iter_mut().for_each(range)
                }
            }
            allocation
        }
    };
    let local = *allocation;
    *allocation = resolve(local);
    local
}

/// Old FABRIC handles and the ones that their restored creators exported in place of
/// them. Importers look up the handle that they recorded.
#[derive(Debug, Default)]
pub struct HandleTable(BTreeMap<FabricHandle, FabricHandle>);

impl HandleTable {
    /// Add the replacements that one restore phase returned.
    pub fn record(
        &mut self,
        replacements: impl IntoIterator<Item = FabricReplacement>,
    ) -> Result<()> {
        for FabricReplacement { old, new } in replacements {
            if let Some(known) = self.0.insert(old, new)
                && known != new
            {
                bail!("fabric handle {old:?} was replaced by two handles");
            }
        }
        Ok(())
    }

    pub fn replacements(&self) -> Vec<FabricReplacement> {
        self.0
            .iter()
            .map(|(old, new)| FabricReplacement {
                old: *old,
                new: *new,
            })
            .collect()
    }

    fn translate(&self, record: &mut Record) {
        if let Record::FabricExport { handle, .. } | Record::FabricImport { handle, .. } = record
            && let Some(new) = self.0.get(handle)
        {
            *handle = *new;
        }
    }
}

/// Check that every restored process holds the records it was checkpointed with, after
/// replacing each recorded FABRIC handle with the one that restore exported.
pub fn verify_restored(saved: &Pods, restored: &Pods, handles: &HandleTable) -> Result<()> {
    for (rank, pod) in restored {
        for (id, actual) in pod {
            let participant = participant(saved.len() > 1, group_pid(*rank, *id));
            let mut expected = saved
                .get(rank)
                .and_then(|saved| saved.get(id))
                .with_context(|| format!("restored {participant} is not in the manifest"))?
                .clone();
            expected
                .iter_mut()
                .for_each(|record| handles.translate(record));
            let mut actual = actual.clone();
            actual.sort();
            expected.sort();
            ensure!(
                actual.len() == expected.len(),
                "restored topology does not match the checkpoint for {participant}: expected {} records, actual {} records",
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
                    "restored topology does not match the checkpoint for {participant}: record {index}: expected {expected:?}, actual {actual:?}"
                );
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_records::*;
    use cudarc::driver::sys::CUmemAllocationHandleType;

    const FABRIC: u32 = CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_FABRIC.0;
    /// Allocation `X` and multicast object `T`, created by process 1 of rank 0.
    const X: AllocationReference = AllocationReference {
        id: [5; 16],
        creator_pid: 1,
    };
    const T: AllocationReference = AllocationReference {
        id: [6; 16],
        creator_pid: 1,
    };

    fn handle(byte: u8) -> FabricHandle {
        FabricHandle([byte; 64])
    }

    /// How an importer names a FABRIC import, with an ID of its own.
    fn import(byte: u8) -> AllocationReference {
        AllocationReference {
            id: [byte; 16],
            creator_pid: 0,
        }
    }

    /// Shared FABRIC allocation `X`, mapped and exported as handle 1.
    fn creator() -> Vec<Record> {
        vec![
            with!(
                shared(),
                Allocation.allocation = X,
                Allocation.handle_types = FABRIC
            ),
            with!(mapping(), Mapping.allocation = X),
            Record::FabricExport {
                allocation: X,
                handle: handle(1),
            },
        ]
    }

    /// An importer of `handle`, which records only what it can observe.
    fn importer(handle: FabricHandle) -> Vec<Record> {
        let local = import(9);
        vec![
            with!(
                shared(),
                Allocation.allocation = local,
                Allocation.handle_types = FABRIC,
                Allocation.size = 0
            ),
            with!(mapping(), Mapping.allocation = local),
            Record::FabricImport {
                allocation: local,
                handle,
            },
        ]
    }

    fn pods(ranks: impl IntoIterator<Item = (Rank, Vec<(NamespacePid, Vec<Record>)>)>) -> Pods {
        ranks
            .into_iter()
            .map(|(rank, pod)| (rank, pod.into_iter().collect()))
            .collect()
    }

    fn refusal(pods: &Pods) -> String {
        format!("{:#}", Topology::validate(pods).err().expect("accepted"))
    }

    /// The carrier bytes that `pid` in `rank` copies, and the owners it is told.
    fn carrier(topology: &Topology, rank: Rank, pid: NamespacePid) -> (u64, Vec<AllocationOwner>) {
        (
            topology.carrier_bytes[&group_pid(rank, pid)],
            topology.owners(rank, pid),
        )
    }

    #[test]
    fn a_pod_alone_validates_and_names_owners_as_before() {
        let topology = Topology::validate(&alone(Manifest::from([
            (1, vec![shared(), mapping()]),
            (2, vec![shared()]),
        ])))
        .unwrap();
        assert_eq!(
            carrier(&topology, 0, 1),
            (
                4096,
                vec![AllocationOwner {
                    allocation: A,
                    owner_pid: 1
                }]
            )
        );
        let (bytes, owners) = carrier(&topology, 0, 2);
        assert_eq!((bytes, owners[0].owner_pid), (0, 1));
        // A Pod alone keeps the per-Pod messages.
        let invalid = alone(Manifest::from([(
            1,
            vec![allocation(), with!(mapping(), Mapping.size = 8192)],
        )]));
        assert!(
            refusal(&invalid).starts_with("participant 1: invalid mapping"),
            "{}",
            refusal(&invalid)
        );
    }

    #[test]
    fn equal_pids_in_different_pods_stay_distinct() {
        let other = with!(allocation(), Allocation.allocation.id = [7; 16]);
        let topology = Topology::validate(&pods([
            (0, vec![(1, vec![allocation(), mapping()])]),
            (1, vec![(1, vec![other])]),
        ]))
        .unwrap();
        assert!(topology.owners(0, 1).is_empty());
        // The same records twice name one allocation twice.
        let error = refusal(&pods([
            (0, vec![(1, vec![allocation()])]),
            (1, vec![(1, vec![allocation()])]),
        ]));
        assert!(
            error.contains("inconsistent allocation metadata"),
            "{error}"
        );
        assert!(error.contains("(rank << 22) | PID"), "{error}");
    }

    #[test]
    fn fabric_importer_in_another_pod_resolves_to_its_creator() {
        let topology = Topology::validate(&pods([
            (0, vec![(1, creator())]),
            (1, vec![(1, importer(handle(1)))]),
        ]))
        .unwrap();
        // Only the creator saves, and the importer names the owner as remote.
        assert_eq!(
            carrier(&topology, 0, 1),
            (
                4096,
                vec![AllocationOwner {
                    allocation: X,
                    owner_pid: 1
                }]
            )
        );
        assert_eq!(
            carrier(&topology, 1, 1),
            (
                0,
                vec![AllocationOwner {
                    allocation: import(9),
                    owner_pid: REMOTE_OWNER
                }]
            )
        );
    }

    #[test]
    fn fabric_importer_in_the_same_pod_names_its_creator_as_owner() {
        let topology = Topology::validate(&alone(Manifest::from([
            (1, creator()),
            (2, importer(handle(1))),
        ])))
        .unwrap();
        assert_eq!(
            topology.owners(0, 2),
            [AllocationOwner {
                allocation: import(9),
                owner_pid: 1
            }]
        );
    }

    #[test]
    fn fabric_records_that_do_not_join_are_refused() {
        let error = refusal(&pods([
            (0, vec![(1, creator())]),
            (1, vec![(1, importer(handle(2)))]),
        ]));
        assert!(error.contains("has no exporter in the group"), "{error}");
        // An importer alone cannot find its creator.
        let error = refusal(&pods([(1, vec![(1, importer(handle(1)))])]));
        assert!(error.contains("has no exporter in the group"), "{error}");
        let mut twice = creator();
        twice[0] = with!(twice[0].clone(), Allocation.allocation.id = [8; 16]);
        twice.push(Record::FabricExport {
            allocation: AllocationReference { id: [8; 16], ..X },
            handle: handle(1),
        });
        let error = refusal(&alone(Manifest::from([(1, twice)])));
        assert!(error.contains("exported by two creators"), "{error}");
        let error = refusal(&alone(Manifest::from([
            (1, creator()),
            (
                2,
                vec![Record::FabricExport {
                    allocation: X,
                    handle: handle(1),
                }],
            ),
        ])));
        assert!(error.contains("did not create it"), "{error}");
        let mut named = importer(handle(1));
        named[2] = Record::FabricImport {
            allocation: X,
            handle: handle(1),
        };
        let error = refusal(&pods([(0, vec![(1, creator())]), (1, vec![(1, named)])]));
        assert!(error.contains("names a creator"), "{error}");
    }

    #[test]
    fn posix_sharing_cannot_span_pods() {
        // Process 1 of rank 1 holds a POSIX import from its own Pod's process 1.
        let error = refusal(&pods([
            (0, vec![(1, vec![shared(), mapping()])]),
            (1, vec![(2, vec![shared(), mapping()])]),
        ]));
        assert!(error.contains("missing creator participant"), "{error}");
    }

    /// Multicast object `T` for two devices: rank 0 creates and exports it as handle 3,
    /// rank 1 imports it, and each Pod binds a member allocation of its own.
    fn team(member_rank: Rank) -> (Pods, Pods) {
        let properties = cuinterpose_protocol::MulticastProperties {
            devices: 2,
            size: 4096,
            handle_types: u64::from(FABRIC),
            flags: 0,
        };
        let holder = |object: AllocationReference, allocation: AllocationReference| {
            vec![
                with!(
                    shared(),
                    Allocation.allocation = allocation,
                    Allocation.handle_types = FABRIC
                ),
                with!(mapping(), Mapping.allocation = allocation),
                with!(device(), MulticastDevice.allocation = object),
                with!(
                    binding(),
                    MulticastBinding.allocation = object,
                    MulticastBinding.source = member(allocation, 0)
                ),
            ]
        };
        let mut creator = holder(T, X);
        creator.push(with!(
            multicast(),
            Multicast.allocation = T,
            Multicast.properties = properties
        ));
        creator.push(Record::FabricExport {
            allocation: T,
            handle: handle(3),
        });
        let local = import(4);
        let mut importer = holder(local, AllocationReference { id: [7; 16], ..X });
        importer.push(with!(
            multicast(),
            Multicast.allocation = local,
            Multicast.properties = cuinterpose_protocol::MulticastProperties {
                devices: 0,
                size: 0,
                handle_types: u64::from(FABRIC),
                flags: 0,
            }
        ));
        importer.push(Record::FabricImport {
            allocation: local,
            handle: handle(3),
        });
        (
            pods([
                (0, vec![(1, creator.clone())]),
                (member_rank, vec![(1, importer.clone())]),
            ]),
            pods([(0, vec![(1, creator)])]),
        )
    }

    #[test]
    fn multicast_team_may_span_pods() {
        let (group, creator_alone) = team(1);
        let topology = Topology::validate(&group).unwrap();
        assert_eq!(carrier(&topology, 0, 1).0, 4096);
        assert_eq!(carrier(&topology, 1, 1).0, 4096);
        let error = refusal(&creator_alone);
        assert!(
            error.contains("incomplete multicast device group"),
            "{error}"
        );
    }

    #[test]
    fn replies_must_match_their_step_and_the_carrier_bytes() {
        let topology = Topology::validate(&pods([
            (0, vec![(1, creator())]),
            (1, vec![(1, importer(handle(1)))]),
        ]))
        .unwrap();
        let save = Operation::SaveAllocations { owners: Vec::new() };
        let saved = |rank, bytes| topology.completed(rank, 1, &save, Reply::Saved { bytes });
        saved(0, 4096).unwrap();
        saved(1, 0).unwrap();
        let error = format!("{:#}", saved(1, 4096).unwrap_err());
        assert_eq!(
            error,
            "rank 1 participant 1: SaveAllocations copied 4096 carrier bytes, expected 0"
        );
        // An owner returns the FABRIC handles that it exported when it loads.
        let replacement = FabricReplacement {
            old: handle(1),
            new: handle(2),
        };
        let loaded = Reply::Loaded {
            bytes: 4096,
            replacements: vec![replacement],
        };
        assert_eq!(
            topology
                .completed(0, 1, &Operation::LoadAllocations, loaded)
                .unwrap(),
            [replacement]
        );
        let error = format!(
            "{:#}",
            topology
                .completed(0, 1, &Operation::PrepareUnicast, Reply::Saved { bytes: 0 })
                .unwrap_err()
        );
        assert!(
            error.starts_with("rank 0 participant 1: unexpected reply to PrepareUnicast"),
            "{error}"
        );
    }

    #[test]
    fn pids_and_ranks_must_fit_group_identities() {
        let error = refusal(&pods([(0, vec![(1 << 22, vec![])])]));
        assert!(error.contains("is not a Linux PID"), "{error}");
        let error = refusal(&pods([(MAX_RANK + 1, vec![(1, vec![])])]));
        assert!(error.contains("exceeds the largest rank"), "{error}");
    }

    #[test]
    fn handle_table_keeps_one_replacement_per_handle() {
        let mut table = HandleTable::default();
        let replacement = FabricReplacement {
            old: handle(1),
            new: handle(2),
        };
        table.record([replacement, replacement]).unwrap();
        assert_eq!(table.replacements(), [replacement]);
        let other = FabricReplacement {
            new: handle(3),
            ..replacement
        };
        let error = format!("{:#}", table.record([other]).unwrap_err());
        assert!(error.contains("replaced by two handles"), "{error}");
    }

    #[test]
    fn restored_records_match_after_handle_translation() {
        let saved = pods([
            (0, vec![(1, creator())]),
            (1, vec![(1, importer(handle(1)))]),
        ]);
        let mut restored = saved.clone();
        for pod in restored.values_mut() {
            for records in pod.values_mut() {
                for record in records {
                    if let Record::FabricExport { handle: h, .. }
                    | Record::FabricImport { handle: h, .. } = record
                    {
                        *h = handle(2);
                    }
                }
            }
        }
        let mut table = HandleTable::default();
        let error = format!(
            "{:#}",
            verify_restored(&saved, &restored, &table).unwrap_err()
        );
        assert!(
            error.starts_with(
                "restored topology does not match the checkpoint for rank 0 participant 1"
            ),
            "{error}"
        );
        table
            .record([FabricReplacement {
                old: handle(1),
                new: handle(2),
            }])
            .unwrap();
        verify_restored(&saved, &restored, &table).unwrap();
        let mut extra = restored.clone();
        extra.get_mut(&1).unwrap().insert(2, Vec::new());
        let error = format!("{:#}", verify_restored(&saved, &extra, &table).unwrap_err());
        assert_eq!(
            error,
            "restored rank 1 participant 2 is not in the manifest"
        );
    }
}
