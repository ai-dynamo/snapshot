// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::{AllocationId, Identity, RECORD_SIZE};
use std::{cmp::Ordering, io};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
#[repr(u32)]
pub enum RecordKind {
    #[default]
    Allocation = 1,
    Mapping = 2,
    Multicast = 3,
    MulticastDevice = 4,
    MulticastBinding = 5,
    MulticastMapping = 6,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct RecordFlags(pub u32);

impl RecordFlags {
    pub const CREATOR: u32 = 1;
    pub const APPLICATION_HANDLE_LIVE: u32 = 2;
    pub const ALLOCATION_CONTENT: u32 = 4;
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Access {
    pub location_type: i32,
    pub location_id: i32,
    pub flags: u64,
}

/// Named representation of the C v2 record. Fields unused by a record kind
/// remain explicit so decode/encode and normalized artifact comparison retain
/// every byte, including the reserved tail before member_offset.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Record {
    pub kind: RecordKind,
    pub flags: RecordFlags,
    pub allocation_id: AllocationId,
    pub address: u64,
    pub size: u64,
    pub offset: u64,
    pub allocation_size: u64,
    pub allocation_type: i32,
    pub requested_handle_types: u32,
    pub allocation_location_type: i32,
    pub allocation_location_id: i32,
    pub access_count: u32,
    pub application_handle_count: u32,
    pub access: [Access; 32],
    pub member_id: AllocationId,
    pub creator_participant: Identity,
    pub binding_kind: u8,
    pub api_version: u8,
    pub reserved: [u8; 5],
    pub member_offset: u64,
    pub operation_flags: u64,
    pub handle_types: u64,
    pub object_flags: u64,
    pub num_devices: u32,
    pub device: i32,
}

impl Default for Record {
    fn default() -> Self {
        Self {
            kind: RecordKind::Allocation,
            flags: RecordFlags::default(),
            allocation_id: [0; 16],
            address: 0,
            size: 0,
            offset: 0,
            allocation_size: 0,
            allocation_type: 0,
            requested_handle_types: 0,
            allocation_location_type: 0,
            allocation_location_id: 0,
            access_count: 0,
            application_handle_count: 0,
            access: [Access::default(); 32],
            member_id: [0; 16],
            creator_participant: [0; 33],
            binding_kind: 0,
            api_version: 0,
            reserved: [0; 5],
            member_offset: 0,
            operation_flags: 0,
            handle_types: 0,
            object_flags: 0,
            num_devices: 0,
            device: 0,
        }
    }
}

impl Record {
    pub fn encode(&self) -> [u8; RECORD_SIZE] {
        let mut bytes = [0; RECORD_SIZE];
        let mut position = 0;
        let mut put = |value: &[u8]| {
            bytes[position..position + value.len()].copy_from_slice(value);
            position += value.len();
        };
        put(&(self.kind as u32).to_le_bytes());
        put(&self.flags.0.to_le_bytes());
        put(&self.allocation_id);
        for value in [self.address, self.size, self.offset, self.allocation_size] {
            put(&value.to_le_bytes());
        }
        put(&self.allocation_type.to_le_bytes());
        put(&self.requested_handle_types.to_le_bytes());
        put(&self.allocation_location_type.to_le_bytes());
        put(&self.allocation_location_id.to_le_bytes());
        put(&self.access_count.to_le_bytes());
        put(&self.application_handle_count.to_le_bytes());
        for access in self.access {
            put(&access.location_type.to_le_bytes());
            put(&access.location_id.to_le_bytes());
            put(&access.flags.to_le_bytes());
        }
        put(&self.member_id);
        put(&self.creator_participant);
        put(&[self.binding_kind, self.api_version]);
        put(&self.reserved);
        for value in [
            self.member_offset,
            self.operation_flags,
            self.handle_types,
            self.object_flags,
        ] {
            put(&value.to_le_bytes());
        }
        put(&self.num_devices.to_le_bytes());
        put(&self.device.to_le_bytes());
        debug_assert_eq!(position, RECORD_SIZE);
        bytes
    }

    pub fn decode(bytes: &[u8; RECORD_SIZE]) -> io::Result<Self> {
        let mut position = 0;
        // Only fixed-width fields are read, in wire order. This private macro
        // cannot be used by topology/state code to reach into binary layouts.
        macro_rules! take {
            ($n:expr) => {{
                let value: [u8; $n] = bytes[position..position + $n].try_into().unwrap();
                position += $n;
                value
            }};
        }
        let kind = match u32::from_le_bytes(take!(4)) {
            1 => RecordKind::Allocation,
            2 => RecordKind::Mapping,
            3 => RecordKind::Multicast,
            4 => RecordKind::MulticastDevice,
            5 => RecordKind::MulticastBinding,
            6 => RecordKind::MulticastMapping,
            _ => {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "unknown record kind",
                ));
            }
        };
        let mut record = Self {
            kind,
            flags: RecordFlags(u32::from_le_bytes(take!(4))),
            allocation_id: take!(16),
            address: u64::from_le_bytes(take!(8)),
            size: u64::from_le_bytes(take!(8)),
            offset: u64::from_le_bytes(take!(8)),
            allocation_size: u64::from_le_bytes(take!(8)),
            allocation_type: i32::from_le_bytes(take!(4)),
            requested_handle_types: u32::from_le_bytes(take!(4)),
            allocation_location_type: i32::from_le_bytes(take!(4)),
            allocation_location_id: i32::from_le_bytes(take!(4)),
            access_count: u32::from_le_bytes(take!(4)),
            application_handle_count: u32::from_le_bytes(take!(4)),
            ..Self::default()
        };
        if record.access_count > 32 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "mapping access count exceeds limit",
            ));
        }
        for access in &mut record.access {
            *access = Access {
                location_type: i32::from_le_bytes(take!(4)),
                location_id: i32::from_le_bytes(take!(4)),
                flags: u64::from_le_bytes(take!(8)),
            };
        }
        record.member_id = take!(16);
        record.creator_participant = take!(33);
        record.binding_kind = take!(1)[0];
        record.api_version = take!(1)[0];
        record.reserved = take!(5);
        record.member_offset = u64::from_le_bytes(take!(8));
        record.operation_flags = u64::from_le_bytes(take!(8));
        record.handle_types = u64::from_le_bytes(take!(8));
        record.object_flags = u64::from_le_bytes(take!(8));
        record.num_devices = u32::from_le_bytes(take!(4));
        record.device = i32::from_le_bytes(take!(4));
        debug_assert_eq!(position, RECORD_SIZE);
        Ok(record)
    }
}

// C state files sort records by memcmp, not by numeric field values. Keep that
// contract even though the Rust representation is no longer an opaque array.
impl Ord for Record {
    fn cmp(&self, other: &Self) -> Ordering {
        self.encode().cmp(&other.encode())
    }
}
impl PartialOrd for Record {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn c_v2_known_bytes_and_roundtrip() {
        let mut record = Record {
            kind: RecordKind::MulticastBinding,
            flags: RecordFlags(7),
            allocation_id: [0x12; 16],
            address: 0x0102030405060708,
            size: 4096,
            offset: 128,
            allocation_size: 8192,
            allocation_type: 1,
            requested_handle_types: 1,
            allocation_location_type: 1,
            allocation_location_id: -3,
            access_count: 1,
            application_handle_count: 2,
            member_id: [0x34; 16],
            creator_participant: crate::parse_identity(b"0123456789abcdef0123456789abcdef")
                .unwrap(),
            binding_kind: 1,
            api_version: 2,
            reserved: [0xa5; 5],
            member_offset: 256,
            operation_flags: 7,
            handle_types: 1,
            object_flags: 9,
            num_devices: 2,
            device: -4,
            ..Record::default()
        };
        record.access[0] = Access {
            location_type: 1,
            location_id: 3,
            flags: 3,
        };
        let mut expected = [0u8; 688];
        // These offsets are independent C v2 known-byte assertions, not calls
        // to the encoder under test.
        for (offset, value) in [
            (0, 5u32),
            (4, 7),
            (56, 1),
            (60, 1),
            (64, 1),
            (68, (-3i32) as u32),
            (72, 1),
            (76, 2),
            (80, 1),
            (84, 3),
            (680, 2),
            (684, (-4i32) as u32),
        ] {
            expected[offset..offset + 4].copy_from_slice(&value.to_le_bytes());
        }
        for (offset, value) in [
            (24, 0x0102030405060708u64),
            (32, 4096),
            (40, 128),
            (48, 8192),
            (88, 3),
            (648, 256),
            (656, 7),
            (664, 1),
            (672, 9),
        ] {
            expected[offset..offset + 8].copy_from_slice(&value.to_le_bytes());
        }
        expected[8..24].fill(0x12);
        expected[592..608].fill(0x34);
        expected[608..641].copy_from_slice(&record.creator_participant);
        expected[641] = 1;
        expected[642] = 2;
        expected[643..648].fill(0xa5);
        assert_eq!(record.encode(), expected);
        assert_eq!(Record::decode(&expected).unwrap(), record);
        for kind in [
            RecordKind::Allocation,
            RecordKind::Mapping,
            RecordKind::Multicast,
            RecordKind::MulticastDevice,
            RecordKind::MulticastBinding,
            RecordKind::MulticastMapping,
        ] {
            record.kind = kind;
            assert_eq!(Record::decode(&record.encode()).unwrap(), record);
        }
    }

    #[test]
    fn invalid_kind_and_count_are_rejected() {
        let mut bytes = Record::default().encode();
        bytes[0] = 99;
        assert!(Record::decode(&bytes).is_err());
        bytes = Record::default().encode();
        bytes[72] = 33;
        assert!(Record::decode(&bytes).is_err());
    }
}
