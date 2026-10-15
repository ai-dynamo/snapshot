// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use anyhow::{Context, bail, ensure};
use cudarc::driver::sys::{CUmemAllocationHandleType, CUmemAllocationType, CUmemLocationType};
use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, Manifest, MemoryLocation, NamespacePid,
    Record,
};
use std::collections::{BTreeMap, BTreeSet};

pub struct AllocationSummary {
    pub reference: AllocationReference,
    pub size: u64,
    pub shared: bool,
    /// Any holder granted another GPU access.
    pub peer_access_granted: bool,
    pub owner_pid: NamespacePid,
    pub holders: BTreeSet<NamespacePid>,
    location: MemoryLocation,
}

impl AllocationSummary {
    pub fn uses_host_carrier(&self) -> bool {
        self.shared || self.peer_access_granted
    }
}
struct Multicast {
    reference: AllocationReference,
    size: u64,
    handle_types: u64,
    flags: u64,
    num_devices: u32,
    creators: u32,
}

/// Collect definitions, local holder anchors, device attachments and bindings without
/// requiring references to appear after their definitions. Then check references,
/// bounds and group completeness against the collected state without changing it.
pub fn validate(participants: &Manifest) -> Result<Vec<AllocationSummary>> {
    let mut allocations: BTreeMap<AllocationId, AllocationSummary> = BTreeMap::new();
    let mut multicasts: BTreeMap<AllocationId, Multicast> = BTreeMap::new();
    let mut anchors = BTreeSet::new();
    // CUDA device ordinals are local to each process, so each device must be attached
    // and bound in the same participant.
    let mut devices: BTreeMap<AllocationId, BTreeSet<(NamespacePid, i32)>> = BTreeMap::new();
    let mut bindings = BTreeSet::new();
    if participants.is_empty() {
        bail!("topology validate failed: no participants");
    }
    for (namespace_pid, participant) in participants {
        for record in participant {
            match record {
                Record::Allocation {
                    allocation,
                    shared,
                    peer_access_granted,
                    size,
                    allocation_type,
                    handle_types,
                    location,
                    virtual_allocation_handle_count,
                } => {
                    ensure!(
                        *allocation_type
                            == CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED as u32
                            && [
                                CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE as u32,
                                CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA as u32,
                            ]
                            .contains(&location.location_type),
                        "participant {namespace_pid}: unsupported allocation properties for {allocation:?}: allocation_type={allocation_type}, location={location:?}"
                    );
                    ensure!(
                        participants.contains_key(&allocation.creator_pid),
                        "participant {namespace_pid}: missing creator participant for {allocation:?}"
                    );
                    ensure!(
                        *size > 0,
                        "participant {namespace_pid}: invalid allocation extent for {allocation:?}"
                    );
                    // Creators may be nonexportable. CUDA reports no handle types on imports.
                    let known_handle_types = *handle_types == 0
                        || *handle_types
                            == CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
                                .0;
                    if allocation.creator_pid == *namespace_pid {
                        // Only exportable backing can be shared.
                        ensure!(
                            known_handle_types && (*handle_types != 0 || !*shared),
                            "participant {namespace_pid}: invalid allocation creator {allocation:?}: handle_types={handle_types}"
                        );
                    } else {
                        ensure!(
                            *shared && known_handle_types,
                            "participant {namespace_pid}: invalid imported allocation {allocation:?}"
                        );
                    }
                    let known =
                        allocations
                            .entry(allocation.id)
                            .or_insert_with(|| AllocationSummary {
                                reference: *allocation,
                                size: *size,
                                shared: *shared,
                                peer_access_granted: false,
                                owner_pid: *namespace_pid,
                                holders: BTreeSet::new(),
                                location: *location,
                            });
                    ensure!(
                        known.reference == *allocation
                            && known.size == *size
                            && known.shared == *shared
                            && known.location.location_type == location.location_type
                            // Device ordinals are per process, while NUMA node IDs are system-wide.
                            && (location.location_type
                                != CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA as u32
                                || known.location.id == location.id),
                        "participant {namespace_pid}: inconsistent allocation metadata for {allocation:?}"
                    );
                    // Each holder grants peer access through its own mappings.
                    known.peer_access_granted |= *peer_access_granted;
                    ensure!(
                        known.holders.insert(*namespace_pid),
                        "participant {namespace_pid}: duplicate allocation record for {allocation:?}"
                    );
                    if *virtual_allocation_handle_count != 0 {
                        anchors.insert((allocation.id, *namespace_pid));
                    }
                }
                Record::Multicast {
                    allocation,
                    properties:
                        cuinterpose_protocol::MulticastProperties {
                            devices,
                            size,
                            handle_types,
                            flags,
                        },
                    ..
                } => {
                    if *handle_types
                        != u64::from(
                            CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0,
                        )
                        || *size == 0
                        || *devices == 0
                        || *flags != 0
                    {
                        bail!(
                            "participant {namespace_pid}: invalid multicast properties for {allocation:?}: size={size}, handle_types={handle_types}, devices={devices}, flags={flags}"
                        );
                    }
                    let multicast = multicasts
                        .entry(allocation.id)
                        .or_insert_with(|| Multicast {
                            reference: *allocation,
                            size: *size,
                            handle_types: *handle_types,
                            flags: *flags,
                            num_devices: *devices,
                            creators: 0,
                        });
                    if multicast.size != *size
                        || multicast.reference != *allocation
                        || multicast.handle_types != *handle_types
                        || multicast.flags != *flags
                        || multicast.num_devices != *devices
                    {
                        bail!(
                            "participant {namespace_pid}: inconsistent multicast properties for {allocation:?}: expected creator={}, size={}, handle_types={}, flags={}, devices={}; got creator={}, size={size}, handle_types={handle_types}, flags={flags}, devices={devices}",
                            multicast.reference.creator_pid,
                            multicast.size,
                            multicast.handle_types,
                            multicast.flags,
                            multicast.num_devices,
                            allocation.creator_pid
                        );
                    }
                    if namespace_pid == &allocation.creator_pid {
                        multicast.creators += 1;
                    }
                }
                Record::Mapping { allocation, .. } => {
                    anchors.insert((allocation.id, *namespace_pid));
                }
                Record::MulticastDevice { allocation, device } => {
                    ensure!(
                        devices
                            .entry(allocation.id)
                            .or_default()
                            .insert((*namespace_pid, *device)),
                        "participant {namespace_pid}: duplicate multicast device {device} for {allocation:?}"
                    );
                }
                Record::MulticastBinding {
                    allocation, device, ..
                } => {
                    bindings.insert((allocation.id, *namespace_pid, *device));
                }
                _ => {}
            }
        }
    }
    for allocation in allocations.values_mut() {
        // Prefer the original creator while it has local state. Otherwise an existing
        // holder saves the backing without changing its identity or remote lifetime.
        allocation.owner_pid = if allocation
            .holders
            .contains(&allocation.reference.creator_pid)
        {
            allocation.reference.creator_pid
        } else {
            *allocation.holders.first().unwrap()
        };
        for holder in &allocation.holders {
            ensure!(
                anchors.contains(&(allocation.reference.id, *holder)),
                "participant {holder}: missing allocation anchor for {:?}",
                allocation.reference
            );
        }
    }
    for (namespace_pid, participant) in participants {
        for record in participant {
            match record {
                Record::Allocation { allocation, .. } => {
                    ensure!(
                        allocations
                            .get(&allocation.id)
                            .is_some_and(|known| known.reference == *allocation),
                        "participant {namespace_pid}: missing creator for {allocation:?}"
                    );
                }
                Record::Mapping {
                    allocation,
                    address,
                    size,
                    offset,
                    ..
                } => {
                    let known = allocations.get(&allocation.id).with_context(|| {
                        format!("participant {namespace_pid}: missing creator for {allocation:?}")
                    })?;
                    ensure!(
                        known.reference == *allocation && known.holders.contains(namespace_pid),
                        "participant {namespace_pid}: inconsistent allocation creator for {allocation:?}"
                    );
                    if *address == 0
                        || *size == 0
                        || offset.checked_add(*size).is_none_or(|end| end > known.size)
                    {
                        bail!(
                            "participant {namespace_pid}: invalid mapping or mapping out of bounds for {allocation:?}: address={address:#x}, offset={offset}, size={size}, allocation_size={}",
                            known.size
                        );
                    }
                }
                Record::MulticastBinding {
                    allocation,
                    source,
                    size,
                    device,
                    ..
                } => {
                    let multicast = multicasts
                        .get(&allocation.id)
                        .with_context(|| format!("missing multicast object {allocation:?}"))?;
                    ensure!(
                        multicast.reference == *allocation,
                        "participant {namespace_pid}: inconsistent multicast creator for {allocation:?}"
                    );
                    // CUDA has already accepted the binding, whose rounded capacity can
                    // exceed the creation size, so only relationships between processes
                    // need validation here.
                    let member = match source {
                        BindingSource::Memory(range) => Some(*range),
                        BindingSource::Address {
                            address,
                            tracked_member,
                        } => {
                            ensure!(
                                *address != 0,
                                "participant {namespace_pid}: invalid multicast member of {allocation:?}"
                            );
                            *tracked_member
                        }
                    };
                    if let Some(range) = member {
                        let allocation = allocations
                            .get(&range.allocation.id)
                            .with_context(|| format!("participant {namespace_pid}: invalid multicast member {:?} of {allocation:?}", range.allocation))?;
                        ensure!(
                            allocation.reference == range.allocation,
                            "participant {namespace_pid}: inconsistent allocation creator for {:?}",
                            range.allocation
                        );
                        if range
                            .offset
                            .checked_add(*size)
                            .is_none_or(|end| end > allocation.size)
                        {
                            bail!(
                                "participant {namespace_pid}: multicast binding out of member bounds for {:?}: offset={}, size={size}, allocation_size={}",
                                range.allocation,
                                range.offset,
                                allocation.size
                            );
                        }
                    }
                    ensure!(
                        devices
                            .get(&allocation.id)
                            .is_some_and(|attached| attached.contains(&(*namespace_pid, *device))),
                        "participant {namespace_pid}: multicast binding device {device} is not attached in this participant for {allocation:?}"
                    );
                }
                Record::MulticastMapping { allocation, .. }
                | Record::MulticastDevice { allocation, .. } => {
                    let multicast = multicasts
                        .get(&allocation.id)
                        .with_context(|| format!("missing multicast object {allocation:?}"))?;
                    ensure!(
                        multicast.reference == *allocation,
                        "participant {namespace_pid}: inconsistent multicast creator for {allocation:?}"
                    );
                }
                _ => {}
            }
        }
    }
    for multicast in multicasts.values() {
        if multicast.creators != 1 {
            bail!(
                "multicast group {:?} must have exactly one creator; found {}",
                multicast.reference,
                multicast.creators
            );
        }
        let attached = devices.get(&multicast.reference.id);
        let device_count = attached.map_or(0, BTreeSet::len);
        if device_count != multicast.num_devices as usize {
            bail!(
                "incomplete multicast device group {:?}: expected {}, found {}",
                multicast.reference,
                multicast.num_devices,
                device_count
            );
        }
        let unbound: Vec<_> = attached
            .into_iter()
            .flatten()
            .filter(|(pid, device)| !bindings.contains(&(multicast.reference.id, *pid, *device)))
            .collect();
        if !unbound.is_empty() {
            bail!(
                "incomplete multicast binding group {:?}: unbound participant/device pairs {:?}",
                multicast.reference,
                unbound
            );
        }
    }
    Ok(allocations.into_values().collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_records::*;
    use cuinterpose_protocol::{BindingVersion, MemberRange, MulticastProperties};

    #[test]
    fn references_can_precede_definitions_in_every_participant() {
        let allocation = AllocationReference {
            creator_pid: 2,
            id: [1; 16],
        };
        let multicast = AllocationReference {
            creator_pid: 2,
            id: [2; 16],
        };
        let object = Record::Multicast {
            allocation: multicast,
            properties: MulticastProperties {
                devices: 2,
                size: 4096,
                handle_types: u64::from(
                    CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0,
                ),
                flags: 0,
            },
            virtual_multicast_handle_count: 1,
        };
        let device = Record::MulticastDevice {
            allocation: multicast,
            device: 0,
        };
        let binding = Record::MulticastBinding {
            allocation: multicast,
            source: BindingSource::Memory(MemberRange {
                allocation,
                offset: 0,
            }),
            size: 4096,
            offset: 0,
            flags: 0,
            version: BindingVersion::V1,
            device: 0,
        };
        let creator = vec![
            binding.clone(),
            device.clone(),
            object.clone(),
            Record::Mapping {
                allocation,
                address: 0x10000,
                size: 4096,
                offset: 0,
                access: Vec::new(),
            },
            Record::Allocation {
                allocation,
                size: 4096,
                shared: true,
                peer_access_granted: false,
                allocation_type: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED as u32,
                handle_types: CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0,
                location: MemoryLocation {
                    location_type: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE as u32,
                    id: 0,
                },
                virtual_allocation_handle_count: 0,
            },
        ];
        let importer = vec![binding, device, object];
        for creator_shift in 0..creator.len() {
            for importer_shift in 0..importer.len() {
                let mut creator = creator.clone();
                creator.rotate_left(creator_shift);
                let mut importer = importer.clone();
                importer.rotate_left(importer_shift);
                let summary = validate(&BTreeMap::from([(1, importer), (2, creator)])).unwrap();
                assert_eq!(summary.len(), 1);
                assert_eq!(summary[0].reference, allocation);
            }
        }
    }

    #[test]
    fn creators_may_be_nonexportable() {
        let allocation = AllocationReference {
            creator_pid: 1,
            id: [1; 16],
        };
        let record = |handle_types, shared| Record::Allocation {
            allocation,
            size: 4096,
            shared,
            peer_access_granted: true,
            allocation_type: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED as u32,
            handle_types,
            location: MemoryLocation {
                location_type: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE as u32,
                id: 0,
            },
            virtual_allocation_handle_count: 1,
        };
        for handle_types in [
            0,
            CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0,
        ] {
            let summary =
                validate(&BTreeMap::from([(1, vec![record(handle_types, false)])])).unwrap();
            assert!(summary[0].uses_host_carrier());
        }
        for (handle_types, shared) in [
            (0, true),
            (
                CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_FABRIC.0,
                false,
            ),
        ] {
            assert!(validate(&BTreeMap::from([(1, vec![record(handle_types, shared)])])).is_err());
        }
    }

    /// Validation error for the records of participants 1 and 2, if any.
    struct Rejection(Option<String>);

    impl Rejection {
        #[track_caller]
        fn names(self, check: &str) {
            let named = self.0.as_ref().is_some_and(|error| error.contains(check));
            assert!(named, "{:?} lacks {check:?}", self.0);
        }
    }

    fn rejection(first: &[Record], second: &[Record]) -> Rejection {
        let manifest = Manifest::from([(1, first.to_vec()), (2, second.to_vec())]);
        Rejection(validate(&manifest).err().map(|error| format!("{error:#}")))
    }

    #[test]
    fn validate_rejection_names_failed_check() {
        let null_address = BindingSource::Address {
            address: 0,
            tracked_member: None,
        };
        // Participant 1 holds `record` after any valid records it references.
        let alone = |record| rejection(&[record], &[]);
        let after_allocation = |record| rejection(&[allocation(), record], &[]);
        let after_multicast = |record| rejection(&[multicast(), record], &[]);
        let after_both = |record| rejection(&[allocation(), multicast(), record], &[]);
        // Participant 2 holds `record` after participant 1 creates and shares `A`.
        let imported = |record| rejection(&[shared()], &[record]);

        alone(with!(allocation(), Allocation.location.location_type = 0))
            .names("participant 1: unsupported allocation properties");
        alone(with!(allocation(), Allocation.allocation_type = 0))
            .names("participant 1: unsupported allocation properties");
        alone(with!(allocation(), Allocation.allocation.creator_pid = 3))
            .names("participant 1: missing creator participant");
        alone(with!(allocation(), Allocation.size = 0))
            .names("participant 1: invalid allocation extent");
        alone(with!(shared(), Allocation.handle_types = 8))
            .names("participant 1: invalid allocation creator");
        rejection(&[allocation()], &[allocation()])
            .names("participant 2: invalid imported allocation");
        imported(with!(shared(), Allocation.handle_types = 8))
            .names("participant 2: invalid imported allocation");
        rejection(&[allocation()], &[shared()])
            .names("participant 2: inconsistent allocation metadata");
        rejection(
            &[allocation()],
            &[with!(allocation(), Allocation.allocation = A2)],
        )
        .names("participant 2: inconsistent allocation metadata");
        imported(with!(shared(), Allocation.size = 8192))
            .names("participant 2: inconsistent allocation metadata");
        imported(with!(shared(), Allocation.location = numa(0)))
            .names("participant 2: inconsistent allocation metadata");
        rejection(
            &[with!(shared(), Allocation.location = numa(1))],
            &[with!(shared(), Allocation.location = numa(0))],
        )
        .names("participant 2: inconsistent allocation metadata");
        after_allocation(allocation()).names("participant 1: duplicate allocation record");
        imported(with!(
            shared(),
            Allocation.virtual_allocation_handle_count = 0
        ))
        .names("participant 2: missing allocation anchor");
        alone(mapping()).names("participant 1: missing creator for");
        after_allocation(with!(mapping(), Mapping.allocation = A2))
            .names("participant 1: inconsistent allocation creator");
        rejection(&[shared()], &[mapping()])
            .names("participant 2: inconsistent allocation creator");
        after_allocation(with!(mapping(), Mapping.address = 0))
            .names("participant 1: invalid mapping or mapping out of bounds");
        after_allocation(with!(mapping(), Mapping.size = 0))
            .names("participant 1: invalid mapping or mapping out of bounds");
        // Ends one byte past `A`.
        after_allocation(with!(mapping(), Mapping.offset = 1))
            .names("participant 1: invalid mapping or mapping out of bounds");
        after_allocation(with!(mapping(), Mapping.offset = u64::MAX))
            .names("participant 1: invalid mapping or mapping out of bounds");
        alone(with!(multicast(), Multicast.properties.flags = 1))
            .names("participant 1: invalid multicast properties");
        alone(with!(multicast(), Multicast.properties.size = 0))
            .names("participant 1: invalid multicast properties");
        alone(with!(multicast(), Multicast.properties.devices = 0))
            .names("participant 1: invalid multicast properties");
        alone(with!(multicast(), Multicast.properties.handle_types = 0))
            .names("participant 1: invalid multicast properties");
        rejection(
            &[multicast()],
            &[with!(multicast(), Multicast.properties.size = 8192)],
        )
        .names("participant 2: inconsistent multicast properties");
        rejection(&[device(), device()], &[]).names("participant 1: duplicate multicast device 0");
        alone(binding()).names("missing multicast object");
        after_multicast(with!(binding(), MulticastBinding.allocation = M2))
            .names("participant 1: inconsistent multicast creator");
        after_multicast(with!(binding(), MulticastBinding.source = null_address))
            .names("participant 1: invalid multicast member of");
        after_multicast(binding())
            .names("participant 1: invalid multicast member AllocationReference");
        after_both(with!(binding(), MulticastBinding.source = member(A2, 0)))
            .names("participant 1: inconsistent allocation creator");
        // Ends one byte past `A`.
        after_both(with!(binding(), MulticastBinding.source = member(A, 1)))
            .names("participant 1: multicast binding out of member bounds");
        after_both(with!(
            binding(),
            MulticastBinding.source = member(A, u64::MAX)
        ))
        .names("participant 1: multicast binding out of member bounds");
        rejection(
            &[allocation(), multicast(), device(), binding()],
            &[multicast(), binding()],
        )
        .names("participant 2: multicast binding device 0 is not attached in this participant");
        alone(multicast_mapping()).names("missing multicast object");
        alone(device()).names("missing multicast object");
        after_multicast(with!(multicast_mapping(), MulticastMapping.allocation = M2))
            .names("participant 1: inconsistent multicast creator");
        rejection(&[], &[multicast()]).names("must have exactly one creator; found 0");
        after_multicast(multicast()).names("must have exactly one creator; found 2");
        alone(multicast()).names("incomplete multicast device group");
        after_multicast(device()).names("incomplete multicast binding group");
    }

    #[test]
    fn validate_accepts_different_device_ordinals_for_one_allocation_across_processes() {
        let other = MemoryLocation { id: 1, ..DEVICE };
        let importer = with!(shared(), Allocation.location = other);
        validate(&Manifest::from([(1, vec![shared()]), (2, vec![importer])])).unwrap();
    }

    #[test]
    fn validate_accepts_multicast_binding_and_mapping_beyond_requested_object_size() {
        let creator = vec![
            allocation(),
            multicast(),
            device(),
            with!(binding(), MulticastBinding.offset = 4096),
            with!(multicast_mapping(), MulticastMapping.size = 8192),
        ];
        validate(&Manifest::from([(1, creator)])).unwrap();
    }
}
