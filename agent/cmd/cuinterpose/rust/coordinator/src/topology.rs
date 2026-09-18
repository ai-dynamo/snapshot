// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::Result;
use anyhow::{Context, bail, ensure};
use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, CUmemAllocationHandleType, Manifest,
    StateEntry,
};
use std::collections::BTreeMap;

pub struct Allocation {
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

pub fn validate(participants: &Manifest) -> Result<Vec<Allocation>> {
    let mut allocations: BTreeMap<AllocationId, Allocation> = BTreeMap::new();
    let mut multicasts: BTreeMap<AllocationId, Multicast> = BTreeMap::new();
    if participants.is_empty() {
        bail!("topology validate failed: no participants");
    }
    // Gather definitions before references. Participant/entry ordering must
    // not determine whether an import or multicast dependency is valid.
    for (participant_id, participant) in participants {
        for record in &participant.entries {
            match record {
                StateEntry::Allocation {
                    allocation,
                    content,
                    size,
                    handle_types,
                    logical_handle_count,
                    ..
                } => {
                    if allocation.creator == *participant_id {
                        ensure!(
                            (*handle_types
                                == CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
                                || (*handle_types
                                    == CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_NONE
                                    && *content))
                                && *size > 0,
                            "invalid allocation creator"
                        );
                        let std::collections::btree_map::Entry::Vacant(entry) =
                            allocations.entry(allocation.id)
                        else {
                            bail!("duplicate allocation creator");
                        };
                        entry.insert(Allocation {
                            reference: *allocation,
                            size: *size,
                            anchor: *logical_handle_count != 0,
                            preserve_content: *content,
                        });
                    } else if *content {
                        bail!("allocation content flag on importer");
                    }
                }
                StateEntry::Multicast {
                    allocation,
                    properties,
                    ..
                } => {
                    if properties.handleTypes
                        != u64::from(
                            CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0,
                        )
                        || properties.size == 0
                        || properties.numDevices == 0
                    {
                        bail!("invalid multicast properties");
                    }
                    let multicast = multicasts
                        .entry(allocation.id)
                        .or_insert_with(|| Multicast {
                            reference: *allocation,
                            size: properties.size as u64,
                            handle_types: properties.handleTypes,
                            flags: properties.flags,
                            num_devices: properties.numDevices,
                            creators: 0,
                            devices: BTreeMap::new(),
                        });
                    if multicast.reference != *allocation
                        || multicast.handle_types != properties.handleTypes
                        || multicast.flags != properties.flags
                        || multicast.num_devices != properties.numDevices
                    {
                        bail!("inconsistent multicast properties");
                    }
                    multicast.size = multicast.size.max(properties.size as u64);
                    if participant_id == &allocation.creator {
                        multicast.creators += 1;
                    }
                }
                _ => {}
            }
        }
    }
    for record in participants.values().flat_map(|p| &p.entries) {
        if let StateEntry::MulticastDevice { allocation, device } = record
            && multicasts
                .get_mut(&allocation.id)
                .context("missing multicast object")?
                .devices
                .insert(*device, false)
                .is_some()
        {
            bail!("duplicate multicast device");
        }
    }
    for (participant_id, participant) in participants {
        for record in &participant.entries {
            match record {
                StateEntry::Allocation { allocation, .. } => {
                    ensure!(
                        allocations
                            .get(&allocation.id)
                            .is_some_and(|known| known.reference == *allocation),
                        "missing creator"
                    );
                }
                StateEntry::Mapping {
                    allocation,
                    address,
                    size,
                    offset,
                    ..
                } => {
                    let known = allocations
                        .get_mut(&allocation.id)
                        .context("missing creator")?;
                    ensure!(
                        known.reference == *allocation,
                        "inconsistent allocation creator"
                    );
                    if *address == 0
                        || *size == 0
                        || offset.checked_add(*size).is_none_or(|end| end > known.size)
                    {
                        bail!("invalid mapping or mapping out of bounds");
                    }
                    known.anchor |= allocation.creator == *participant_id;
                }
                StateEntry::MulticastBinding {
                    allocation,
                    source,
                    size,
                    offset,
                    device,
                    ..
                } => {
                    let multicast = multicasts
                        .get_mut(&allocation.id)
                        .context("missing multicast object")?;
                    ensure!(
                        multicast.reference == *allocation,
                        "inconsistent multicast creator"
                    );
                    if *size == 0
                        || offset
                            .checked_add(*size)
                            .is_none_or(|end| end > multicast.size)
                    {
                        bail!("invalid multicast binding");
                    }
                    let member = match source {
                        BindingSource::Memory(range) => Some(*range),
                        BindingSource::Address {
                            address,
                            tracked_member,
                        } => {
                            ensure!(*address != 0, "invalid multicast member");
                            *tracked_member
                        }
                    };
                    if let Some(range) = member {
                        let allocation = allocations
                            .get(&range.allocation.id)
                            .context("invalid multicast member")?;
                        ensure!(
                            allocation.reference == range.allocation,
                            "inconsistent allocation creator"
                        );
                        if range
                            .offset
                            .checked_add(*size)
                            .is_none_or(|end| end > allocation.size)
                        {
                            bail!("multicast binding out of member bounds");
                        }
                    }
                    *multicast
                        .devices
                        .get_mut(device)
                        .context("multicast binding device is absent")? = true;
                }
                StateEntry::MulticastMapping {
                    allocation,
                    address,
                    size,
                    offset,
                    ..
                } => {
                    let multicast = multicasts
                        .get(&allocation.id)
                        .context("missing multicast object")?;
                    ensure!(
                        multicast.reference == *allocation,
                        "inconsistent multicast creator"
                    );
                    if *address == 0
                        || *size == 0
                        || offset
                            .checked_add(*size)
                            .is_none_or(|end| end > multicast.size)
                    {
                        bail!("invalid multicast mapping");
                    }
                }
                _ => {}
            }
        }
    }
    for allocation in allocations.values() {
        if !allocation.anchor {
            bail!("missing creator anchor");
        }
    }
    for multicast in multicasts.values() {
        if multicast.creators != 1 {
            bail!("multicast group must have exactly one creator");
        }
        if multicast.devices.len() != multicast.num_devices as usize {
            bail!("incomplete multicast device group");
        }
        if multicast.devices.values().any(|bound| !bound) {
            bail!("incomplete multicast binding group");
        }
    }
    Ok(allocations.into_values().collect())
}
