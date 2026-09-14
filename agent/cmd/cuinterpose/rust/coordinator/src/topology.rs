// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::{Participant, Result};
use cuinterpose_protocol::{AllocationId, Identity, RecordFlags, RecordKind};
use std::collections::{BTreeMap, BTreeSet};

pub struct Allocation {
    pub creator: Identity,
    pub size: u64,
    pub preserve_content: bool,
    creator_handle: bool,
    creator_mapping: bool,
}

impl Default for Allocation {
    fn default() -> Self {
        Self {
            creator: [0; 33],
            size: 0,
            preserve_content: false,
            creator_handle: false,
            creator_mapping: false,
        }
    }
}
struct Multicast {
    creator: Identity,
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
    for participant in participants {
        if participant.id[32] != 0
            || cuinterpose_protocol::parse_identity(&participant.id[..32]).is_err()
        {
            return Err("invalid participant identity".into());
        }
        if !identities.insert(participant.id) {
            return Err("duplicate participant identity".into());
        }
        for record in &participant.records {
            let id = record.allocation_id;
            let creator = record.flags.0 & RecordFlags::CREATOR != 0;
            match record.kind {
                RecordKind::Allocation => {
                    let allocation = allocations.entry(id).or_default();
                    if creator {
                        if record.requested_handle_types != 1 {
                            return Err("non-POSIX requested handle type".into());
                        }
                        if record.allocation_size == 0 {
                            return Err("zero creator allocation size".into());
                        }
                        if allocation.creator != [0; 33] && allocation.creator != participant.id {
                            return Err("conflicting creators".into());
                        }
                        allocation.creator = participant.id;
                        allocation.size = record.allocation_size;
                        allocation.creator_handle =
                            record.flags.0 & RecordFlags::APPLICATION_HANDLE_LIVE != 0;
                        allocation.preserve_content =
                            record.flags.0 & RecordFlags::ALLOCATION_CONTENT != 0;
                    } else if record.flags.0 & RecordFlags::ALLOCATION_CONTENT != 0 {
                        return Err("allocation content flag on importer".into());
                    }
                }
                RecordKind::Mapping => {
                    if record.address == 0 || record.size == 0 || record.access_count > 32 {
                        return Err("invalid mapping".into());
                    }
                    allocations.entry(id).or_default().creator_mapping |= creator;
                }
                RecordKind::Multicast => {
                    let identity =
                        cuinterpose_protocol::parse_identity(&record.creator_participant[..32])?;
                    if record.creator_participant[32] != 0
                        || record.handle_types != 1
                        || record.allocation_size == 0
                        || record.num_devices == 0
                    {
                        return Err("invalid multicast properties".into());
                    }
                    let multicast = multicasts.entry(id).or_insert_with(|| Multicast {
                        creator: identity,
                        size: record.allocation_size,
                        handle_types: record.handle_types,
                        flags: record.object_flags,
                        num_devices: record.num_devices,
                        creators: 0,
                        devices: BTreeMap::new(),
                    });
                    if multicast.creator != identity
                        || multicast.handle_types != record.handle_types
                        || multicast.flags != record.object_flags
                        || multicast.num_devices != record.num_devices
                    {
                        return Err("inconsistent multicast properties".into());
                    }
                    multicast.size = multicast.size.max(record.allocation_size);
                    if creator {
                        if participant.id != identity {
                            return Err("invalid multicast creator".into());
                        }
                        multicast.creators += 1;
                    }
                }
                RecordKind::MulticastDevice => {
                    let multicast = multicasts
                        .get_mut(&id)
                        .ok_or("multicast device precedes object")?;
                    if multicast.devices.insert(record.device, false).is_some() {
                        return Err("duplicate multicast device".into());
                    }
                }
                RecordKind::MulticastBinding => {
                    let multicast = multicasts
                        .get_mut(&id)
                        .ok_or("multicast binding precedes object")?;
                    if record.size == 0
                        || record
                            .offset
                            .checked_add(record.size)
                            .is_none_or(|end| end > multicast.size)
                        || !matches!(record.binding_kind, 1 | 2)
                        || !matches!(record.api_version, 1 | 2)
                    {
                        return Err("invalid multicast binding".into());
                    }
                    if (record.binding_kind == 1
                        && (record.address != 0 || !allocations.contains_key(&record.member_id)))
                        || (record.binding_kind == 2 && record.address == 0)
                    {
                        return Err("invalid multicast member".into());
                    }
                    *multicast
                        .devices
                        .get_mut(&record.device)
                        .ok_or("multicast binding device is absent")? = true;
                }
                RecordKind::MulticastMapping => {
                    let multicast = multicasts
                        .get(&id)
                        .ok_or("multicast mapping precedes object")?;
                    if record.address == 0
                        || record.size == 0
                        || record.access_count > 32
                        || record
                            .offset
                            .checked_add(record.size)
                            .is_none_or(|end| end > multicast.size)
                    {
                        return Err("invalid multicast mapping".into());
                    }
                }
            }
        }
    }
    for allocation in allocations.values() {
        if allocation.creator == [0; 33] {
            return Err("missing creator".into());
        }
        if !allocation.creator_handle && !allocation.creator_mapping {
            return Err("missing creator anchor".into());
        }
    }
    for participant in participants {
        for record in &participant.records {
            let (allocation, offset) = match record.kind {
                RecordKind::Mapping => (&allocations[&record.allocation_id], record.offset),
                RecordKind::MulticastBinding if record.binding_kind == 1 => {
                    (&allocations[&record.member_id], record.member_offset)
                }
                _ => continue,
            };
            if offset
                .checked_add(record.size)
                .is_none_or(|end| end > allocation.size)
            {
                return Err(if record.kind == RecordKind::Mapping {
                    "mapping out of bounds"
                } else {
                    "multicast binding out of member bounds"
                }
                .into());
            }
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
