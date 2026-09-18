// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Owned inspection metadata and versioned MessagePack messages. No CUDA
//! handles or process pointers enter this format.

mod cuda_serde;
mod identity;
mod record;
mod transport;

#[doc(inline)]
pub use cudarc::driver::sys::{
    CUmemAccess_flags, CUmemAccessDesc, CUmemAllocationHandleType, CUmemAllocationType,
    CUmemLocation, CUmemLocationType, CUmulticastObjectProp,
};
#[doc(inline)]
pub use identity::{
    AllocationId, AllocationReference, ParticipantId, format_id, parse_participant_id,
};
#[doc(inline)]
pub use record::{BindingSource, BindingVersion, MemberRange, StateEntry};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use std::{collections::BTreeMap, io, path::PathBuf, time::Duration};
#[doc(inline)]
pub use transport::{receive, send};

pub const VERSION: u8 = 1;
// Bound allocations controlled by socket frame prefixes and checkpoint files.
// Protocol payloads contain metadata, never allocation contents.
pub const MAX_MESSAGE_BYTES: usize = 32 * 1024 * 1024;
pub const TICKET_MAGIC: [u8; 4] = [b'C', b'U', b'I', VERSION];
pub const TICKET_BYTES: usize =
    TICKET_MAGIC.len() + size_of::<ParticipantId>() + size_of::<AllocationId>();

pub type ParticipantDirectory = BTreeMap<ParticipantId, PathBuf>;
pub type Manifest = BTreeMap<ParticipantId, ParticipantState>;

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct ParticipantState {
    pub socket_path: PathBuf,
    pub entries: Vec<StateEntry>,
}

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error(transparent)]
    Io(#[from] io::Error),
    #[error(transparent)]
    Encode(#[from] rmp_serde::encode::Error),
    #[error(transparent)]
    Decode(#[from] rmp_serde::decode::Error),
    #[error("{0}")]
    Invalid(&'static str),
    #[error("creator rejected export: {0}")]
    Remote(String),
}
pub type Result<T> = std::result::Result<T, Error>;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Operation {
    PrepareMulticast,
    SaveAllocations,
    PrepareUnicast,
    LoadAllocations,
    RestoreUnicast,
    RestoreMulticastCreators,
    RestoreMulticastImporters,
    RestoreMulticastDevices,
    RestoreMulticastBindings,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Request {
    Identify,
    Rendezvous {
        #[serde(with = "serde_bytes")]
        participant: ParticipantId,
        participants: ParticipantDirectory,
    },
    Inspect {
        #[serde(with = "serde_bytes")]
        participant: ParticipantId,
    },
    Execute {
        #[serde(with = "serde_bytes")]
        participant: ParticipantId,
        operation: Operation,
    },
    Export {
        allocation: AllocationReference,
    },
}

#[derive(Debug, Serialize, Deserialize)]
pub struct Response {
    #[serde(with = "serde_bytes")]
    pub participant: ParticipantId,
    pub result: std::result::Result<Reply, String>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Reply {
    Identified,
    Ready,
    Inspection {
        entries: Vec<StateEntry>,
        live_raw_imports: u64,
        unsupported_creations: u64,
    },
    Completed {
        operation: Operation,
        bytes: u64,
        copy_us: u32,
    },
    UnicastExport,
    MulticastExport {
        #[serde(with = "cuda_serde::multicast_properties")]
        properties: CUmulticastObjectProp,
    },
}

#[derive(Serialize, Deserialize)]
struct Envelope<T> {
    version: u8,
    body: T,
}

/// Encodes an owned metadata value inside the current version envelope.
///
/// # Errors
/// Returns serialization failures or an error when the encoded size exceeds
/// `MAX_MESSAGE_BYTES`.
pub fn encode<T: Serialize>(body: &T) -> Result<Vec<u8>> {
    let bytes = rmp_serde::to_vec_named(&Envelope {
        version: VERSION,
        body,
    })?;
    if bytes.len() > MAX_MESSAGE_BYTES {
        return Err(Error::Invalid("message exceeds size limit"));
    }
    Ok(bytes)
}

/// Decodes one versioned metadata value without retaining references to the input.
///
/// # Errors
/// Rejects malformed, oversized, too deeply nested, obsolete, or trailing data.
pub fn decode<T: DeserializeOwned>(bytes: &[u8]) -> Result<T> {
    if bytes.len() > MAX_MESSAGE_BYTES {
        return Err(Error::Invalid("message exceeds size limit"));
    }
    let mut decoder = rmp_serde::Deserializer::new(io::Cursor::new(bytes));
    decoder.set_max_depth(32);
    let envelope = Envelope::<T>::deserialize(&mut decoder)?;
    if envelope.version != VERSION || decoder.position() != bytes.len() as u64 {
        return Err(Error::Invalid(
            "unsupported version or trailing message data",
        ));
    }
    Ok(envelope.body)
}

pub fn timeout(operation: Option<Operation>) -> Duration {
    let (variable, fallback) = match operation {
        Some(Operation::SaveAllocations | Operation::LoadAllocations) => {
            ("SNAPSHOT_CARRIER_TIMEOUT_SECONDS", 3600)
        }
        _ => ("SNAPSHOT_CONTROL_TIMEOUT_SECONDS", 10),
    };
    let seconds = std::env::var(variable)
        .ok()
        .and_then(|s| s.parse::<u32>().ok())
        .filter(|n| *n > 0 && *n <= i32::MAX as u32)
        .unwrap_or(fallback);
    Duration::from_secs(seconds.into())
}
