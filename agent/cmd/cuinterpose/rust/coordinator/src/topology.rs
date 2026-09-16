// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::{Participant, Result};
use anyhow::{Context, bail, ensure};
use cuinterpose_protocol::{AllocationId, BindingSource, MAX_ACCESS, ParticipantId, Record};
use std::collections::{BTreeMap, BTreeSet};

pub struct Allocation {
    pub creator: ParticipantId,
    pub size: u64,
    pub preserve_content: bool,
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
        bail!("topology validate failed: no participants");
    }
    // Gather definitions before references. Participant/record ordering must
    // not determine whether an import or multicast dependency is valid.
    for participant in participants {
        if !identities.insert(participant.id) {
            bail!("duplicate participant identity");
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
                    if *creator {
                        ensure!(
                            *handle_types == 1 && *size > 0,
                            "invalid allocation creator"
                        );
                        let std::collections::btree_map::Entry::Vacant(entry) =
                            allocations.entry(*id)
                        else {
                            bail!("duplicate allocation creator");
                        };
                        entry.insert(Allocation {
                            creator: participant.id,
                            size: *size,
                            anchor: *handles != 0,
                            preserve_content: *content,
                        });
                    } else if *content {
                        bail!("allocation content flag on importer");
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
                        bail!("invalid multicast properties");
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
                        bail!("inconsistent multicast properties");
                    }
                    multicast.size = multicast.size.max(*size);
                    if *owned {
                        if participant.id != *creator {
                            bail!("invalid multicast creator");
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
                .context("missing multicast object")?
                .devices
                .insert(*device, false)
                .is_some()
        {
            bail!("duplicate multicast device");
        }
    }
    for participant in participants {
        for record in &participant.records {
            match record {
                Record::Allocation { id, .. } => {
                    ensure!(allocations.contains_key(id), "missing creator");
                }
                Record::Mapping {
                    id,
                    creator,
                    address,
                    size,
                    offset,
                    access,
                } => {
                    let allocation = allocations.get_mut(id).context("missing creator")?;
                    if *address == 0
                        || *size == 0
                        || access.len() > MAX_ACCESS
                        || offset
                            .checked_add(*size)
                            .is_none_or(|end| end > allocation.size)
                    {
                        bail!("invalid mapping or mapping out of bounds");
                    }
                    allocation.anchor |= *creator && allocation.creator == participant.id;
                }
                Record::MulticastBinding {
                    id,
                    source,
                    size,
                    offset,
                    device,
                    ..
                } => {
                    let multicast = multicasts.get_mut(id).context("missing multicast object")?;
                    if *size == 0
                        || offset
                            .checked_add(*size)
                            .is_none_or(|end| end > multicast.size)
                    {
                        bail!("invalid multicast binding");
                    }
                    let member = match source {
                        BindingSource::Memory(range) => Some(range),
                        BindingSource::Address {
                            address,
                            tracked_member,
                        } => {
                            ensure!(*address != 0, "invalid multicast member");
                            tracked_member.as_ref()
                        }
                    };
                    if let Some(range) = member {
                        let allocation = allocations
                            .get(&range.allocation)
                            .context("invalid multicast member")?;
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
                Record::MulticastMapping {
                    id,
                    address,
                    size,
                    offset,
                    access,
                    ..
                } => {
                    let multicast = multicasts.get(id).context("missing multicast object")?;
                    if *address == 0
                        || *size == 0
                        || access.len() > MAX_ACCESS
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
