// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use anyhow::{Context, bail, ensure};
use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, Manifest, NamespacePid, Record,
};
use std::collections::BTreeMap;

// CUDA allocation properties supported by Linux FD transport and the host carrier.
const CU_MEM_HANDLE_TYPE_NONE: u32 = 0;
const CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR: u32 = 1;
const CU_MEM_ALLOCATION_TYPE_PINNED: u32 = 1;
const CU_MEM_LOCATION_TYPE_DEVICE: u32 = 1;
const CU_MEM_LOCATION_TYPE_HOST_NUMA: u32 = 3;

pub struct AllocationSummary {
    pub reference: AllocationReference,
    pub size: u64,
    pub checkpoint_via_host_carrier: bool,
    anchor: bool,
}
struct Multicast {
    reference: AllocationReference,
    size: u64,
    handle_types: u64,
    flags: u64,
    num_devices: u32,
    creators: u32,
    // CUDA device ordinals are local to each process, so each device must be attached
    // and bound in the same participant.
    devices: BTreeMap<(NamespacePid, i32), bool>,
}

pub fn validate(participants: &Manifest) -> Result<Vec<AllocationSummary>> {
    let mut allocations: BTreeMap<AllocationId, AllocationSummary> = BTreeMap::new();
    let mut multicasts: BTreeMap<AllocationId, Multicast> = BTreeMap::new();
    if participants.is_empty() {
        bail!("topology validate failed: no participants");
    }
    // Collecting definitions before references lets imports and multicast dependencies
    // be validated independently of participant and entry order.
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
                        *allocation_type == CU_MEM_ALLOCATION_TYPE_PINNED
                            && matches!(
                                location.location_type,
                                CU_MEM_LOCATION_TYPE_DEVICE | CU_MEM_LOCATION_TYPE_HOST_NUMA
                            ),
                        "participant {namespace_pid}: unsupported allocation properties for {allocation:?}: allocation_type={allocation_type}, location={location:?}"
                    );
                    if allocation.creator_pid == *namespace_pid {
                        ensure!(
                            (*handle_types == CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
                                || (*handle_types == CU_MEM_HANDLE_TYPE_NONE
                                    && *checkpoint_via_host_carrier))
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
                    if *handle_types != u64::from(CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR)
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
                            devices: BTreeMap::new(),
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
                _ => {}
            }
        }
    }
    for (namespace_pid, participant) in participants {
        for record in participant {
            if let Record::MulticastDevice { allocation, device } = record {
                let multicast = multicasts
                    .get_mut(&allocation.id)
                    .with_context(|| format!("missing multicast object {allocation:?}"))?;
                ensure!(
                    multicast.reference == *allocation,
                    "inconsistent multicast creator for {allocation:?}"
                );
                if multicast
                    .devices
                    .insert((*namespace_pid, *device), false)
                    .is_some()
                {
                    bail!(
                        "participant {namespace_pid}: duplicate multicast device {device} for {allocation:?}"
                    );
                }
            }
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
                    let known = allocations.get_mut(&allocation.id).with_context(|| {
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
                    known.anchor |= allocation.creator_pid == *namespace_pid;
                }
                Record::MulticastBinding {
                    allocation,
                    source,
                    size,
                    device,
                    ..
                } => {
                    let multicast = multicasts
                        .get_mut(&allocation.id)
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
                    *multicast
                        .devices
                        .get_mut(&(*namespace_pid, *device))
                        .with_context(|| format!("participant {namespace_pid}: multicast binding device {device} is not attached in this participant for {allocation:?}"))? = true;
                }
                Record::MulticastMapping { allocation, .. } => {
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
        if multicast.devices.len() != multicast.num_devices as usize {
            bail!(
                "incomplete multicast device group {:?}: expected {}, found {}",
                multicast.reference,
                multicast.num_devices,
                multicast.devices.len()
            );
        }
        if multicast.devices.values().any(|bound| !bound) {
            bail!(
                "incomplete multicast binding group {:?}: unbound participant/device pairs {:?}",
                multicast.reference,
                multicast
                    .devices
                    .iter()
                    .filter_map(|(device, bound)| (!bound).then_some(device))
                    .collect::<Vec<_>>()
            );
        }
    }
    Ok(allocations.into_values().collect())
}
