// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::AllocationReference;
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct MemberRange {
    pub allocation: AllocationReference,
    pub offset: u64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BindingSource {
    Memory(MemberRange),
    Address {
        address: u64,
        tracked_member: Option<MemberRange>,
    },
}

/// CUDA multicast binding API variant, not the cuinterpose protocol version.
/// V1 replays cuMulticastBindMem/BindAddr; V2 replays their _v2 entry points,
/// which take an explicit device argument.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BindingVersion {
    V1,
    V2,
}

/// One entry in a participant's persisted CUDA state.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StateEntry {
    Allocation {
        allocation: AllocationReference,
        content: bool,
        size: u64,
        /// `CUmemAllocationProp::type_`.
        allocation_type: u32,
        /// `CUmemAllocationProp::requestedHandleTypes` bits.
        handle_types: u32,
        /// `CUmemAllocationProp::location` as `(type_, id)`.
        location: (u32, i32),
        logical_handle_count: u64,
    },
    Mapping {
        allocation: AllocationReference,
        address: u64,
        size: u64,
        offset: u64,
        /// `CUmemAccessDesc` values as `(location.type_, location.id, flags)`.
        access: Vec<(u32, i32, u32)>,
    },
    Multicast {
        allocation: AllocationReference,
        /// Fields from `CUmulticastObjectProp`.
        devices: u32,
        size: u64,
        handle_types: u64,
        flags: u64,
        logical_handle_count: u64,
    },
    MulticastDevice {
        allocation: AllocationReference,
        device: i32,
    },
    MulticastBinding {
        allocation: AllocationReference,
        source: BindingSource,
        size: u64,
        offset: u64,
        flags: u64,
        version: BindingVersion,
        device: i32,
    },
    MulticastMapping {
        allocation: AllocationReference,
        address: u64,
        size: u64,
        offset: u64,
        flags: u64,
        /// `CUmemAccessDesc` values as `(location.type_, location.id, flags)`.
        access: Vec<(u32, i32, u32)>,
    },
}
