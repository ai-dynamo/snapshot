// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::{Participant, Result};
use cuinterpose_protocol::{AllocationId, BindingKind, MAX_ACCESS, ParticipantId, Record};
use std::collections::{BTreeMap, BTreeSet};

#[derive(Default)]
pub struct Allocation {
    pub creator: ParticipantId,
    pub size: u64,
    pub preserve_content: bool,
    creator_seen: bool,
    anchor: bool,
}
struct Multicast {
    creator: ParticipantId,
    size: u64,
    handle_types: u64,
    flags: u64,
    num_devices: u32,
    creators: u32,
    devices: BTreeMap<i32, bool>,
}

pub fn validate(participants: &[Participant]) -> Result<Vec<Allocation>> {
    let mut identities = BTreeSet::new();
    let mut allocations: BTreeMap<AllocationId, Allocation> = BTreeMap::new();
    let mut multicasts: BTreeMap<AllocationId, Multicast> = BTreeMap::new();
    if participants.is_empty() {
        return Err("topology validate failed: no participants".into());
    }
    // Gather definitions before references. Participant/record ordering must
    // not determine whether an import or multicast dependency is valid.
    for participant in participants {
        if !identities.insert(participant.id) {
            return Err("duplicate participant identity".into());
        }
        for record in &participant.records {
            match record {
                Record::Allocation {
                    id,
                    creator,
                    content,
                    size,
                    handle_types,
                    handles,
                    ..
                } => {
                    let allocation = allocations.entry(*id).or_default();
                    if *creator {
                        if *handle_types != 1 || *size == 0 || allocation.creator_seen {
                            return Err("invalid or duplicate allocation creator".into());
                        }
                        allocation.creator = participant.id;
                        allocation.creator_seen = true;
                        allocation.size = *size;
                        allocation.anchor |= *handles != 0;
                        allocation.preserve_content = *content;
                    } else if *content {
                        return Err("allocation content flag on importer".into());
                    }
                }
                Record::Multicast {
                    id,
                    creator,
                    owned,
                    size,
                    handle_types,
                    flags,
                    devices,
                    ..
                } => {
                    if *handle_types != 1 || *size == 0 || *devices == 0 {
                        return Err("invalid multicast properties".into());
                    }
                    let multicast = multicasts.entry(*id).or_insert_with(|| Multicast {
                        creator: *creator,
                        size: *size,
                        handle_types: *handle_types,
                        flags: *flags,
                        num_devices: *devices,
                        creators: 0,
                        devices: BTreeMap::new(),
                    });
                    if multicast.creator != *creator
                        || multicast.handle_types != *handle_types
                        || multicast.flags != *flags
                        || multicast.num_devices != *devices
                    {
                        return Err("inconsistent multicast properties".into());
                    }
                    multicast.size = multicast.size.max(*size);
                    if *owned {
                        if participant.id != *creator {
                            return Err("invalid multicast creator".into());
                        }
                        multicast.creators += 1;
                    }
                }
                _ => {}
            }
        }
    }
    for record in participants.iter().flat_map(|p| &p.records) {
        if let Record::MulticastDevice { id, device } = record
            && multicasts
                .get_mut(id)
                .ok_or("missing multicast object")?
                .devices
                .insert(*device, false)
                .is_some()
        {
            return Err("duplicate multicast device".into());
        }
    }
    for participant in participants {
        for record in &participant.records {
            match record {
                Record::Mapping {
                    id,
                    creator,
                    address,
                    size,
                    offset,
                    access,
                } => {
                    let allocation = allocations.get_mut(id).ok_or("missing creator")?;
                    if *address == 0
                        || *size == 0
                        || access.len() > MAX_ACCESS
                        || offset
                            .checked_add(*size)
                            .is_none_or(|end| end > allocation.size)
                    {
                        return Err("invalid mapping or mapping out of bounds".into());
                    }
                    allocation.anchor |= *creator && allocation.creator == participant.id;
                }
                Record::MulticastBinding {
                    id,
                    member,
                    address,
                    size,
                    offset,
                    member_offset,
                    binding,
                    device,
                    ..
                } => {
                    let multicast = multicasts.get_mut(id).ok_or("missing multicast object")?;
                    if *size == 0
                        || offset
                            .checked_add(*size)
                            .is_none_or(|end| end > multicast.size)
                    {
                        return Err("invalid multicast binding".into());
                    }
                    match binding {
                        BindingKind::Memory => {
                            let allocation =
                                allocations.get(member).ok_or("invalid multicast member")?;
                            if *address != 0
                                || member_offset
                                    .checked_add(*size)
                                    .is_none_or(|end| end > allocation.size)
                            {
                                return Err("multicast binding out of member bounds".into());
                            }
                        }
                        BindingKind::Address if *address == 0 => {
                            return Err("invalid multicast member".into());
                        }
                        BindingKind::Address => {}
                    }
                    *multicast
                        .devices
                        .get_mut(device)
                        .ok_or("multicast binding device is absent")? = true;
                }
                Record::MulticastMapping {
                    id,
                    address,
                    size,
                    offset,
                    access,
                    ..
                } => {
                    let multicast = multicasts.get(id).ok_or("missing multicast object")?;
                    if *address == 0
                        || *size == 0
                        || access.len() > MAX_ACCESS
                        || offset
                            .checked_add(*size)
                            .is_none_or(|end| end > multicast.size)
                    {
                        return Err("invalid multicast mapping".into());
                    }
                }
                _ => {}
            }
        }
    }
    for allocation in allocations.values() {
        if !allocation.creator_seen {
            return Err("missing creator".into());
        }
        if !allocation.anchor {
            return Err("missing creator anchor".into());
        }
    }
    for multicast in multicasts.values() {
        if multicast.creators != 1 {
            return Err("multicast group must have exactly one creator".into());
        }
        if multicast.devices.len() != multicast.num_devices as usize {
            return Err("incomplete multicast device group".into());
        }
        if multicast.devices.values().any(|bound| !bound) {
            return Err("incomplete multicast binding group".into());
        }
    }
    Ok(allocations.into_values().collect())
}
