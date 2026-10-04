// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use anyhow::{Context, bail, ensure};
use cudarc::driver::sys::{CUmemAllocationHandleType, CUmemAllocationType, CUmemLocationType};
use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, Manifest, NamespacePid, Record,
};
use std::collections::{BTreeMap, BTreeSet};

pub struct AllocationSummary {
    pub reference: AllocationReference,
    pub size: u64,
    pub checkpoint_via_host_carrier: bool,
    /// The creator retains an application handle or a mapping of this allocation.
    /// SaveAllocations needs that handle, or must recover it from a creator mapping,
    /// to copy shared backing into the host carrier. An importer cannot supply the
    /// creator's saved bytes, so importer handles and mappings do not count.
    anchor: bool,
}
struct Multicast {
    reference: AllocationReference,
    size: u64,
    handle_types: u64,
    flags: u64,
    num_devices: u32,
    creators: u32,
}

/// Collect definitions, creator anchors, device attachments and bindings without
/// requiring references to appear after their definitions. Then check references,
/// bounds and group completeness against the collected state without changing it.
pub fn validate(participants: &Manifest) -> Result<Vec<AllocationSummary>> {
    let mut allocations: BTreeMap<AllocationId, AllocationSummary> = BTreeMap::new();
    let mut multicasts: BTreeMap<AllocationId, Multicast> = BTreeMap::new();
    let mut creator_mappings = BTreeSet::new();
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
                    checkpoint_via_host_carrier,
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
                    if allocation.creator_pid == *namespace_pid {
                        ensure!(
                            *handle_types == CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0
                                && *size > 0,
                            "participant {namespace_pid}: invalid allocation creator {allocation:?}: size={size}, handle_types={handle_types}"
                        );
                        let std::collections::btree_map::Entry::Vacant(entry) =
                            allocations.entry(allocation.id)
                        else {
                            bail!(
                                "participant {namespace_pid}: duplicate allocation creator {allocation:?}"
                            );
                        };
                        entry.insert(AllocationSummary {
                            reference: *allocation,
                            size: *size,
                            anchor: *virtual_allocation_handle_count != 0,
                            checkpoint_via_host_carrier: *checkpoint_via_host_carrier,
                        });
                    } else if *checkpoint_via_host_carrier {
                        bail!(
                            "participant {namespace_pid}: allocation checkpoint_via_host_carrier flag on importer of {allocation:?}"
                        );
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
                Record::Mapping { allocation, .. } if allocation.creator_pid == *namespace_pid => {
                    creator_mappings.insert(*allocation);
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
        allocation.anchor |= creator_mappings.contains(&allocation.reference);
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
                        known.reference == *allocation,
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
    for allocation in allocations.values() {
        if !allocation.anchor {
            bail!("missing creator anchor for {:?}", allocation.reference);
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
    use cuinterpose_protocol::{BindingVersion, MemberRange, MemoryLocation, MulticastProperties};

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
                checkpoint_via_host_carrier: true,
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
}
