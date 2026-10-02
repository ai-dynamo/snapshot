// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::AllocationReference;
use serde::{Deserialize, Serialize};

/// CUDA memory location (`CUmemLocation`).
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct MemoryLocation {
    pub location_type: u32,
    pub id: i32,
}

/// Access permissions at a CUDA memory location (`CUmemAccessDesc`).
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct MemoryAccess {
    pub location: MemoryLocation,
    pub flags: u32,
}

/// Creation properties shared by multicast records and export replies.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct MulticastProperties {
    /// `CUmulticastObjectProp::numDevices`.
    pub devices: u32,
    pub size: u64,
    pub handle_types: u64,
    pub flags: u64,
}

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

/// CUDA multicast binding API variant. This is separate from the cuinterpose protocol
/// version. V1 replays cuMulticastBindMem and cuMulticastBindAddr. V2 replays their _v2
/// entry points, which take an explicit device argument.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BindingVersion {
    V1,
    V2,
}

/// One entry in a participant's persisted CUDA state.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Record {
    Allocation {
        allocation: AllocationReference,
        /// This participant saves allocation bytes in the host carrier captured by CRIU
        /// and restores them from it. True only for shared allocations created by this
        /// participant. Importers do not copy the same bytes again. Private memory uses
        /// native CUDA checkpointing.
        checkpoint_via_host_carrier: bool,
        size: u64,
        /// `CUmemAllocationProp::type_`.
        allocation_type: u32,
        /// `CUmemAllocationProp::requestedHandleTypes` bits.
        handle_types: u32,
        /// `CUmemAllocationProp::location`.
        location: MemoryLocation,
        virtual_allocation_handle_count: u64,
    },
    Mapping {
        allocation: AllocationReference,
        address: u64,
        size: u64,
        offset: u64,
        /// `CUmemAccessDesc` values.
        access: Vec<MemoryAccess>,
    },
    Multicast {
        allocation: AllocationReference,
        properties: MulticastProperties,
        virtual_multicast_handle_count: u64,
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
        /// `CUmemAccessDesc` values.
        access: Vec<MemoryAccess>,
    },
}
