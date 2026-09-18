// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Stable MessagePack representations for the CUDA ABI values in the protocol.

use crate::MAX_ACCESS;
use cudarc::driver::sys::{
    CUmemAccess_flags, CUmemAccessDesc, CUmemAllocationHandleType, CUmemAllocationType,
    CUmemLocation, CUmemLocationType, CUmulticastObjectProp,
};
use serde::{Deserialize, Deserializer, Serialize, Serializer, de, ser};

pub mod allocation_type {
    use super::*;

    pub fn serialize<S>(value: &CUmemAllocationType, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        serializer.serialize_u32(*value as u32)
    }

    pub fn deserialize<'de, D>(deserializer: D) -> Result<CUmemAllocationType, D::Error>
    where
        D: Deserializer<'de>,
    {
        decode_allocation_type(u32::deserialize(deserializer)?)
    }
}

pub mod allocation_handle_type {
    use super::*;

    pub fn serialize<S>(value: &CUmemAllocationHandleType, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        serializer.serialize_u32(value.0)
    }

    pub fn deserialize<'de, D>(deserializer: D) -> Result<CUmemAllocationHandleType, D::Error>
    where
        D: Deserializer<'de>,
    {
        Ok(CUmemAllocationHandleType(u32::deserialize(deserializer)?))
    }
}

#[derive(Serialize, Deserialize)]
struct LocationWire {
    kind: u32,
    id: i32,
}

impl From<CUmemLocation> for LocationWire {
    fn from(value: CUmemLocation) -> Self {
        Self {
            kind: value.type_ as u32,
            id: value.id,
        }
    }
}

impl LocationWire {
    fn into_cuda<E: de::Error>(self) -> Result<CUmemLocation, E> {
        Ok(CUmemLocation {
            type_: decode_location_type(self.kind)?,
            id: self.id,
        })
    }
}

pub mod location {
    use super::*;

    pub fn serialize<S>(value: &CUmemLocation, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        LocationWire::from(*value).serialize(serializer)
    }

    pub fn deserialize<'de, D>(deserializer: D) -> Result<CUmemLocation, D::Error>
    where
        D: Deserializer<'de>,
    {
        LocationWire::deserialize(deserializer)?.into_cuda()
    }
}

#[derive(Serialize, Deserialize)]
struct AccessWire {
    location: LocationWire,
    flags: u32,
}

pub mod access {
    use super::*;

    pub fn serialize<S>(values: &[CUmemAccessDesc], serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        if values.len() > MAX_ACCESS {
            return Err(ser::Error::custom("too many access descriptors"));
        }
        values
            .iter()
            .map(|value| AccessWire {
                location: value.location.into(),
                flags: value.flags as u32,
            })
            .collect::<Vec<_>>()
            .serialize(serializer)
    }

    pub fn deserialize<'de, D>(deserializer: D) -> Result<Vec<CUmemAccessDesc>, D::Error>
    where
        D: Deserializer<'de>,
    {
        let values = Vec::<AccessWire>::deserialize(deserializer)?;
        if values.len() > MAX_ACCESS {
            return Err(de::Error::custom("too many access descriptors"));
        }
        values
            .into_iter()
            .map(|value| {
                Ok(CUmemAccessDesc {
                    location: value.location.into_cuda()?,
                    flags: decode_access_flags(value.flags)?,
                })
            })
            .collect()
    }
}

#[derive(Serialize, Deserialize)]
struct MulticastPropertiesWire {
    devices: u32,
    size: u64,
    handle_types: u64,
    flags: u64,
}

pub mod multicast_properties {
    use super::*;

    pub fn serialize<S>(value: &CUmulticastObjectProp, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        MulticastPropertiesWire {
            devices: value.numDevices,
            size: value
                .size
                .try_into()
                .map_err(|_| ser::Error::custom("multicast size exceeds wire format"))?,
            handle_types: value.handleTypes,
            flags: value.flags,
        }
        .serialize(serializer)
    }

    pub fn deserialize<'de, D>(deserializer: D) -> Result<CUmulticastObjectProp, D::Error>
    where
        D: Deserializer<'de>,
    {
        let value = MulticastPropertiesWire::deserialize(deserializer)?;
        Ok(CUmulticastObjectProp {
            numDevices: value.devices,
            size: value
                .size
                .try_into()
                .map_err(|_| de::Error::custom("multicast size exceeds host size"))?,
            handleTypes: value.handle_types,
            flags: value.flags,
        })
    }
}

fn decode_allocation_type<E: de::Error>(value: u32) -> Result<CUmemAllocationType, E> {
    use CUmemAllocationType::{
        CU_MEM_ALLOCATION_TYPE_INVALID, CU_MEM_ALLOCATION_TYPE_MANAGED, CU_MEM_ALLOCATION_TYPE_MAX,
        CU_MEM_ALLOCATION_TYPE_PINNED,
    };
    match value {
        value if value == CU_MEM_ALLOCATION_TYPE_INVALID as u32 => {
            Ok(CU_MEM_ALLOCATION_TYPE_INVALID)
        }
        value if value == CU_MEM_ALLOCATION_TYPE_PINNED as u32 => Ok(CU_MEM_ALLOCATION_TYPE_PINNED),
        value if value == CU_MEM_ALLOCATION_TYPE_MANAGED as u32 => {
            Ok(CU_MEM_ALLOCATION_TYPE_MANAGED)
        }
        value if value == CU_MEM_ALLOCATION_TYPE_MAX as u32 => Ok(CU_MEM_ALLOCATION_TYPE_MAX),
        _ => Err(E::custom("invalid CUDA allocation type")),
    }
}

fn decode_location_type<E: de::Error>(value: u32) -> Result<CUmemLocationType, E> {
    use CUmemLocationType::{
        CU_MEM_LOCATION_TYPE_DEVICE, CU_MEM_LOCATION_TYPE_HOST, CU_MEM_LOCATION_TYPE_HOST_NUMA,
        CU_MEM_LOCATION_TYPE_HOST_NUMA_CURRENT, CU_MEM_LOCATION_TYPE_INVALID,
        CU_MEM_LOCATION_TYPE_MAX,
    };
    match value {
        value if value == CU_MEM_LOCATION_TYPE_INVALID as u32 => Ok(CU_MEM_LOCATION_TYPE_INVALID),
        value if value == CU_MEM_LOCATION_TYPE_DEVICE as u32 => Ok(CU_MEM_LOCATION_TYPE_DEVICE),
        value if value == CU_MEM_LOCATION_TYPE_HOST as u32 => Ok(CU_MEM_LOCATION_TYPE_HOST),
        value if value == CU_MEM_LOCATION_TYPE_HOST_NUMA as u32 => {
            Ok(CU_MEM_LOCATION_TYPE_HOST_NUMA)
        }
        value if value == CU_MEM_LOCATION_TYPE_HOST_NUMA_CURRENT as u32 => {
            Ok(CU_MEM_LOCATION_TYPE_HOST_NUMA_CURRENT)
        }
        value if value == CU_MEM_LOCATION_TYPE_MAX as u32 => Ok(CU_MEM_LOCATION_TYPE_MAX),
        _ => Err(E::custom("invalid CUDA location type")),
    }
}

fn decode_access_flags<E: de::Error>(value: u32) -> Result<CUmemAccess_flags, E> {
    use CUmemAccess_flags::{
        CU_MEM_ACCESS_FLAGS_PROT_MAX, CU_MEM_ACCESS_FLAGS_PROT_NONE, CU_MEM_ACCESS_FLAGS_PROT_READ,
        CU_MEM_ACCESS_FLAGS_PROT_READWRITE,
    };
    match value {
        value if value == CU_MEM_ACCESS_FLAGS_PROT_NONE as u32 => Ok(CU_MEM_ACCESS_FLAGS_PROT_NONE),
        value if value == CU_MEM_ACCESS_FLAGS_PROT_READ as u32 => Ok(CU_MEM_ACCESS_FLAGS_PROT_READ),
        value if value == CU_MEM_ACCESS_FLAGS_PROT_READWRITE as u32 => {
            Ok(CU_MEM_ACCESS_FLAGS_PROT_READWRITE)
        }
        value if value == CU_MEM_ACCESS_FLAGS_PROT_MAX as u32 => Ok(CU_MEM_ACCESS_FLAGS_PROT_MAX),
        _ => Err(E::custom("invalid CUDA access flags")),
    }
}
