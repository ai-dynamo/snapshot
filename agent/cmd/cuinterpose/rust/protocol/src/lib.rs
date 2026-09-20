// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Owned inspection metadata and versioned MessagePack messages. No CUDA
//! handles or process pointers enter this format.

mod identity;
mod record;
mod transport;

#[doc(inline)]
pub use identity::{AllocationId, AllocationReference, NamespacePid};
#[doc(inline)]
pub use record::{BindingSource, BindingVersion, MemberRange, Record};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use std::{
    collections::BTreeMap,
    io,
    path::{Path, PathBuf},
    time::Duration,
};
#[doc(inline)]
pub use transport::{receive, send};

pub const VERSION: u8 = 2;
// Bound allocations controlled by socket frame prefixes and checkpoint files.
// Protocol payloads contain metadata, never allocation contents.
pub const MAX_MESSAGE_BYTES: usize = 32 * 1024 * 1024;
pub const VIRTUAL_SHAREABLE_HANDLE_MAGIC: [u8; 4] = [b'C', b'U', b'I', VERSION];
pub const VIRTUAL_SHAREABLE_HANDLE_BYTES: usize =
    VIRTUAL_SHAREABLE_HANDLE_MAGIC.len() + size_of::<NamespacePid>() + size_of::<AllocationId>();

pub type Manifest = BTreeMap<NamespacePid, Vec<Record>>;

pub fn socket_path(control_dir: &Path, namespace_pid: NamespacePid) -> PathBuf {
    control_dir.join(format!("cuinterpose-{namespace_pid}.sock"))
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

/// Encode `magic | creator namespace PID | allocation ID`.
pub fn encode_virtual_shareable_handle(
    reference: AllocationReference,
) -> Result<[u8; VIRTUAL_SHAREABLE_HANDLE_BYTES]> {
    if reference.id == [0; size_of::<AllocationId>()] {
        return Err(Error::Invalid("invalid allocation reference"));
    }
    let mut bytes = [0; VIRTUAL_SHAREABLE_HANDLE_BYTES];
    let (magic, fields) = bytes.split_at_mut(VIRTUAL_SHAREABLE_HANDLE_MAGIC.len());
    let (creator_pid, id) = fields.split_at_mut(size_of::<NamespacePid>());
    magic.copy_from_slice(&VIRTUAL_SHAREABLE_HANDLE_MAGIC);
    if reference.creator_pid == 0 {
        return Err(Error::Invalid("invalid creator namespace PID"));
    }
    creator_pid.copy_from_slice(&reference.creator_pid.to_le_bytes());
    id.copy_from_slice(&reference.id);
    Ok(bytes)
}

/// Decode `magic | creator namespace PID | allocation ID`.
pub fn decode_virtual_shareable_handle(
    bytes: &[u8; VIRTUAL_SHAREABLE_HANDLE_BYTES],
) -> Result<AllocationReference> {
    let (magic, fields) = bytes.split_at(VIRTUAL_SHAREABLE_HANDLE_MAGIC.len());
    if magic != VIRTUAL_SHAREABLE_HANDLE_MAGIC {
        return Err(Error::Invalid("invalid virtual shareable handle magic"));
    }
    let (creator_pid_bytes, id_bytes) = fields.split_at(size_of::<NamespacePid>());
    let mut creator_pid = [0; size_of::<NamespacePid>()];
    let mut id = [0; size_of::<AllocationId>()];
    creator_pid.copy_from_slice(creator_pid_bytes);
    id.copy_from_slice(id_bytes);
    let creator_pid = NamespacePid::from_le_bytes(creator_pid);
    if creator_pid == 0 {
        return Err(Error::Invalid("invalid creator namespace PID"));
    }
    if id == [0; size_of::<AllocationId>()] {
        return Err(Error::Invalid("invalid allocation reference"));
    }
    Ok(AllocationReference { id, creator_pid })
}

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
    /// Enter checkpoint mode and return stable records. The application must
    /// already be parked; this request does not synchronize GPU work.
    BeginCheckpoint {
        namespace_pid: NamespacePid,
    },
    Inspect {
        namespace_pid: NamespacePid,
    },
    Execute {
        namespace_pid: NamespacePid,
        operation: Operation,
    },
    Export {
        allocation: AllocationReference,
    },
}

#[derive(Debug, Serialize, Deserialize)]
pub struct Response {
    pub namespace_pid: NamespacePid,
    pub result: std::result::Result<Reply, String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Reply {
    Inspection {
        records: Vec<Record>,
    },
    Completed {
        operation: Operation,
        bytes: u64,
        copy_us: u32,
    },
    UnicastExport,
    MulticastExport {
        devices: u32,
        size: u64,
        handle_types: u64,
        flags: u64,
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
