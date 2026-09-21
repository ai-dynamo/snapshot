// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use anyhow::{Context, bail, ensure};
use cuinterpose_protocol::{AllocationId, AllocationReference, BindingSource, Manifest, Record};
use std::collections::BTreeMap;

// CUmemAllocationHandleType values used by the Linux FD transport.
const CU_MEM_HANDLE_TYPE_NONE: u32 = 0;
const CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR: u32 = 1;

pub struct AllocationSummary {
    pub reference: AllocationReference,
    pub size: u64,
    pub preserve_content: bool,
    anchor: bool,
}
struct Multicast {
    reference: AllocationReference,
    size: u64,
    handle_types: u64,
    flags: u64,
    num_devices: u32,
    creators: u32,
    devices: BTreeMap<i32, bool>,
}

pub fn validate(participants: &Manifest) -> Result<Vec<AllocationSummary>> {
    let mut allocations: BTreeMap<AllocationId, AllocationSummary> = BTreeMap::new();
    let mut multicasts: BTreeMap<AllocationId, Multicast> = BTreeMap::new();
    if participants.is_empty() {
        bail!("topology validate failed: no participants");
    }
    // Gather definitions before references. Participant/entry ordering must
    // not determine whether an import or multicast dependency is valid.
    for (namespace_pid, participant) in participants {
        for record in participant {
            match record {
                Record::Allocation {
                    allocation,
                    content,
                    size,
                    handle_types,
                    virtual_allocation_handle_count,
                    ..
                } => {
                    if allocation.creator_pid == *namespace_pid {
                        ensure!(
                            (*handle_types == CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
                                || (*handle_types == CU_MEM_HANDLE_TYPE_NONE && *content))
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
                            preserve_content: *content,
                        });
                    } else if *content {
                        bail!(
                            "participant {namespace_pid}: allocation content flag on importer of {allocation:?}"
                        );
                    }
                }
                Record::Multicast {
                    allocation,
                    devices,
                    size,
                    handle_types,
                    flags,
                    ..
                } => {
                    if *handle_types != u64::from(CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR)
                        || *size == 0
                        || *devices == 0
                    {
                        bail!(
                            "participant {namespace_pid}: invalid multicast properties for {allocation:?}: size={size}, handle_types={handle_types}, devices={devices}"
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
                    if multicast.reference != *allocation
                        || multicast.handle_types != *handle_types
                        || multicast.flags != *flags
                        || multicast.num_devices != *devices
                    {
                        bail!(
                            "participant {namespace_pid}: inconsistent multicast properties for {allocation:?}: expected creator={}, handle_types={}, flags={}, devices={}; got creator={}, handle_types={handle_types}, flags={flags}, devices={devices}",
                            multicast.reference.creator_pid,
                            multicast.handle_types,
                            multicast.flags,
                            multicast.num_devices,
                            allocation.creator_pid
                        );
                    }
                    multicast.size = multicast.size.max(*size);
                    if namespace_pid == &allocation.creator_pid {
                        multicast.creators += 1;
                    }
                }
                _ => {}
            }
        }
    }
    for record in participants.values().flatten() {
        if let Record::MulticastDevice { allocation, device } = record
            && multicasts
                .get_mut(&allocation.id)
                .with_context(|| format!("missing multicast object {allocation:?}"))?
                .devices
                .insert(*device, false)
                .is_some()
        {
            bail!("duplicate multicast device {device} for {allocation:?}");
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
                    offset,
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
                    if *size == 0
                        || offset
                            .checked_add(*size)
                            .is_none_or(|end| end > multicast.size)
                    {
                        bail!(
                            "participant {namespace_pid}: invalid multicast binding for {allocation:?}: offset={offset}, size={size}, multicast_size={}",
                            multicast.size
                        );
                    }
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
                        .get_mut(device)
                        .with_context(|| format!("participant {namespace_pid}: multicast binding device {device} is absent for {allocation:?}"))? = true;
                }
                Record::MulticastMapping {
                    allocation,
                    address,
                    size,
                    offset,
                    ..
                } => {
                    let multicast = multicasts
                        .get(&allocation.id)
                        .with_context(|| format!("missing multicast object {allocation:?}"))?;
                    ensure!(
                        multicast.reference == *allocation,
                        "participant {namespace_pid}: inconsistent multicast creator for {allocation:?}"
                    );
                    if *address == 0
                        || *size == 0
                        || offset
                            .checked_add(*size)
                            .is_none_or(|end| end > multicast.size)
                    {
                        bail!(
                            "participant {namespace_pid}: invalid multicast mapping for {allocation:?}: address={address:#x}, offset={offset}, size={size}, multicast_size={}",
                            multicast.size
                        );
                    }
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
                "incomplete multicast binding group {:?}: unbound devices {:?}",
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
