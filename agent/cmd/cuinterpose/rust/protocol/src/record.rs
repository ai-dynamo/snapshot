// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::{AllocationId, MAX_ACCESS, ParticipantId, bounded_vec};
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct Access {
    pub location_type: i32,
    pub location_id: i32,
    pub flags: u64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BindingKind {
    Memory,
    Address,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BindingVersion {
    V1,
    V2,
}

/// Each variant contains only metadata meaningful for that resource. Ordering
/// is semantic (including variant order), not an encoding-dependent memcmp.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Record {
    Allocation {
        id: AllocationId,
        creator: bool,
        content: bool,
        size: u64,
        allocation_type: i32,
        handle_types: u32,
        location_type: i32,
        location_id: i32,
        handles: u32,
    },
    Mapping {
        id: AllocationId,
        creator: bool,
        address: u64,
        size: u64,
        offset: u64,
        #[serde(deserialize_with = "bounded_vec::<_, _, MAX_ACCESS>")]
        access: Vec<Access>,
    },
    Multicast {
        id: AllocationId,
        creator: ParticipantId,
        owned: bool,
        size: u64,
        handles: u32,
        handle_types: u64,
        flags: u64,
        devices: u32,
    },
    MulticastDevice {
        id: AllocationId,
        device: i32,
    },
    MulticastBinding {
        id: AllocationId,
        member: AllocationId,
        address: u64,
        size: u64,
        offset: u64,
        member_offset: u64,
        flags: u64,
        binding: BindingKind,
        version: BindingVersion,
        device: i32,
    },
    MulticastMapping {
        id: AllocationId,
        address: u64,
        size: u64,
        offset: u64,
        flags: u64,
        #[serde(deserialize_with = "bounded_vec::<_, _, MAX_ACCESS>")]
        access: Vec<Access>,
    },
}
