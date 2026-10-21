// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Valid records for topology unit tests and participant integration tests.
#![allow(dead_code, reason = "each test crate uses a different subset")]

use cudarc::driver::sys::{CUmemAllocationHandleType, CUmemAllocationType, CUmemLocationType};
use cuinterpose_protocol::{
    AllocationReference, BindingSource, BindingVersion, MemberRange, MemoryLocation,
    MulticastProperties, Record,
};

/// Replace record fields, e.g. `with!(mapping(), Mapping.size = 0, Mapping.offset = 4096)`.
macro_rules! with {
    ($record:expr, $($variant:ident.$field:ident$(.$nested:ident)? = $value:expr),+) => {{
        let mut record = $record;
        // Evaluate each value before the field binding can shadow a variable it names.
        $({
            let value = $value;
            let ::cuinterpose_protocol::Record::$variant { $field, .. } = &mut record else {
                unreachable!()
            };
            (*$field)$(.$nested)? = value;
        })+
        record
    }};
}
pub(crate) use with;

const FD: u32 = CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0;
pub const A: AllocationReference = AllocationReference {
    id: [1; 16],
    creator_pid: 1,
};
pub const A2: AllocationReference = AllocationReference {
    creator_pid: 2,
    ..A
};
pub const M: AllocationReference = AllocationReference {
    id: [2; 16],
    creator_pid: 1,
};
pub const M2: AllocationReference = AllocationReference {
    creator_pid: 2,
    ..M
};
pub const DEVICE: MemoryLocation = MemoryLocation {
    location_type: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE as u32,
    id: 0,
};

pub fn numa(id: i32) -> MemoryLocation {
    MemoryLocation {
        location_type: CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA as u32,
        id,
    }
}

/// Unshared 4096-byte device allocation `A`, created by participant 1.
pub fn allocation() -> Record {
    Record::Allocation {
        allocation: A,
        shared: false,
        peer_access_granted: false,
        size: 4096,
        allocation_type: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED as u32,
        handle_types: FD,
        location: DEVICE,
        virtual_allocation_handle_count: 1,
    }
}

pub fn shared() -> Record {
    with!(allocation(), Allocation.shared = true)
}

pub fn mapping() -> Record {
    Record::Mapping {
        allocation: A,
        address: 0x10000,
        size: 4096,
        offset: 0,
        access: Vec::new(),
    }
}

/// 4096-byte multicast object `M` for one device, created by participant 1.
pub fn multicast() -> Record {
    Record::Multicast {
        allocation: M,
        properties: MulticastProperties {
            devices: 1,
            size: 4096,
            handle_types: u64::from(FD),
            flags: 0,
        },
        virtual_multicast_handle_count: 1,
    }
}

pub fn device() -> Record {
    Record::MulticastDevice {
        allocation: M,
        device: 0,
    }
}

pub fn member(allocation: AllocationReference, offset: u64) -> BindingSource {
    BindingSource::Memory(MemberRange { allocation, offset })
}

/// Binds all of `A` to `M` on device 0.
pub fn binding() -> Record {
    Record::MulticastBinding {
        allocation: M,
        source: member(A, 0),
        size: 4096,
        offset: 0,
        flags: 0,
        version: BindingVersion::V1,
        device: 0,
    }
}

pub fn multicast_mapping() -> Record {
    Record::MulticastMapping {
        allocation: M,
        address: 0x20000,
        size: 4096,
        offset: 0,
        flags: 0,
        access: Vec::new(),
    }
}
