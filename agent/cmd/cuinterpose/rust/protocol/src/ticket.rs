// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::{AllocationId, Identity, parse_identity};
use std::io;

pub const TICKET_SIZE: usize = 256;
const MAGIC: u32 = 0x44564d43;
const VERSION: u16 = 2;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Ticket {
    pub creator: Identity,
    pub allocation: AllocationId,
    pub endpoint: String,
    pub resource: u32,
    pub devices: u32,
    pub size: u64,
    pub handle_types: u64,
    pub flags: u64,
}

impl Ticket {
    pub fn encode(&self) -> io::Result<[u8; TICKET_SIZE]> {
        self.validate()?;
        let mut bytes = [0u8; TICKET_SIZE];
        bytes[..4].copy_from_slice(&MAGIC.to_le_bytes());
        bytes[4..6].copy_from_slice(&VERSION.to_le_bytes());
        bytes[41..74].copy_from_slice(&self.creator);
        bytes[74..90].copy_from_slice(&self.allocation);
        bytes[90..90 + self.endpoint.len()].copy_from_slice(self.endpoint.as_bytes());
        bytes[200..204].copy_from_slice(&self.resource.to_le_bytes());
        bytes[204..208].copy_from_slice(&self.devices.to_le_bytes());
        bytes[208..216].copy_from_slice(&self.size.to_le_bytes());
        bytes[216..224].copy_from_slice(&self.handle_types.to_le_bytes());
        bytes[224..232].copy_from_slice(&self.flags.to_le_bytes());
        // Reserved fields and alignment remain deliberately zero, as in C.
        Ok(bytes)
    }

    pub fn decode(bytes: &[u8; TICKET_SIZE]) -> io::Result<Self> {
        if bytes[..4] != MAGIC.to_le_bytes()
            || bytes[4..6] != VERSION.to_le_bytes()
            || bytes[6..41].iter().any(|byte| *byte != 0)
            || bytes[73] != 0
            || bytes[198..200].iter().any(|byte| *byte != 0)
            || bytes[232..256].iter().any(|byte| *byte != 0)
        {
            return Err(io::Error::other("invalid ticket"));
        }
        let creator = parse_identity(&bytes[41..73])?;
        let end = bytes[90..198]
            .iter()
            .position(|b| *b == 0)
            .ok_or_else(|| io::Error::other("unterminated endpoint"))?;
        let ticket = Self {
            creator,
            allocation: bytes[74..90].try_into().unwrap(),
            endpoint: std::str::from_utf8(&bytes[90..90 + end])
                .map_err(io::Error::other)?
                .into(),
            resource: u32::from_le_bytes(bytes[200..204].try_into().unwrap()),
            devices: u32::from_le_bytes(bytes[204..208].try_into().unwrap()),
            size: u64::from_le_bytes(bytes[208..216].try_into().unwrap()),
            handle_types: u64::from_le_bytes(bytes[216..224].try_into().unwrap()),
            flags: u64::from_le_bytes(bytes[224..232].try_into().unwrap()),
        };
        ticket.validate()?;
        Ok(ticket)
    }

    fn validate(&self) -> io::Result<()> {
        parse_identity(&self.creator[..32])?;
        if self.creator[32] != 0
            || self.allocation == [0; 16]
            || self.endpoint.len() >= 108
            || !self.endpoint.starts_with('/')
            || self.endpoint.as_bytes().contains(&0)
            || !matches!(self.resource, 1 | 2)
            || (self.resource == 1
                && (self.devices != 0
                    || self.size != 0
                    || self.handle_types != 0
                    || self.flags != 0))
            || (self.resource == 2
                && (self.devices == 0 || self.size == 0 || self.handle_types != 1))
        {
            return Err(io::Error::other("invalid ticket resource or endpoint"));
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn c_v2_known_bytes_and_roundtrip() {
        let ticket = Ticket {
            creator: parse_identity(b"0123456789abcdef0123456789abcdef").unwrap(),
            allocation: [0x67; 16],
            endpoint: "/tmp/control.sock".into(),
            resource: 2,
            devices: 3,
            size: 4096,
            handle_types: 1,
            flags: 7,
        };
        let mut expected = [0; 256];
        expected[..6].copy_from_slice(&[0x43, 0x4d, 0x56, 0x44, 2, 0]);
        expected[41..74].copy_from_slice(&ticket.creator);
        expected[74..90].fill(0x67);
        expected[90..107].copy_from_slice(b"/tmp/control.sock");
        expected[200] = 2;
        expected[204] = 3;
        expected[209] = 16;
        expected[216] = 1;
        expected[224] = 7;
        assert_eq!(ticket.encode().unwrap(), expected);
        assert_eq!(Ticket::decode(&expected).unwrap(), ticket);
    }

    #[test]
    fn reserved_fields_and_unicast_metadata_are_rejected() {
        let ticket = Ticket {
            creator: parse_identity(b"0123456789abcdef0123456789abcdef").unwrap(),
            allocation: [1; 16],
            endpoint: "/x".into(),
            resource: 1,
            devices: 0,
            size: 0,
            handle_types: 0,
            flags: 0,
        };
        let valid = ticket.encode().unwrap();
        assert_eq!(Ticket::decode(&valid).unwrap(), ticket);
        // Match C's three reserved arrays and four multicast-only fields.
        // Check every byte, including the high bytes of integer fields.
        for offset in (6..41).chain(198..200).chain(232..256).chain(204..232) {
            let mut bytes = valid;
            bytes[offset] = 1;
            assert!(Ticket::decode(&bytes).is_err(), "offset {offset}");
        }
        for field in 0..4 {
            let mut invalid = ticket.clone();
            match field {
                0 => invalid.devices = 1,
                1 => invalid.size = 1,
                2 => invalid.handle_types = 1,
                3 => invalid.flags = 1,
                _ => unreachable!(),
            }
            assert!(invalid.encode().is_err(), "field {field}");
        }
    }

    #[test]
    fn malformed_identity_endpoint_and_multicast_are_rejected() {
        let ticket = Ticket {
            creator: parse_identity(b"0123456789abcdef0123456789abcdef").unwrap(),
            allocation: [1; 16],
            endpoint: "/x".into(),
            resource: 2,
            devices: 2,
            size: 4096,
            handle_types: 1,
            flags: 0,
        };
        for offset in [73, 90, 200, 204, 216] {
            let mut bytes = ticket.encode().unwrap();
            bytes[offset] = if offset == 73 { b'x' } else { 0 };
            assert!(Ticket::decode(&bytes).is_err(), "offset {offset}");
        }
    }
}
