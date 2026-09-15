// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Owned inspection metadata and versioned MessagePack messages. No CUDA
//! handles, process pointers, or Rust/driver object layouts enter this format.

mod identity;
mod record;
mod ticket;
mod transport;

#[doc(inline)]
pub use identity::{AllocationId, ParticipantId};
#[doc(inline)]
pub use record::{Access, BindingKind, BindingVersion, Record};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use std::{io, time::Duration};
#[doc(inline)]
pub use ticket::{Resource, ResourceKind, TICKET_MAGIC, Ticket};
#[doc(inline)]
pub use transport::{receive, send};

pub const VERSION: u16 = 3;
pub const MAX_RECORDS: usize = 4096;
pub const MAX_ACCESS: usize = 32;
// A maximal inspection (4096 mappings, 32 named access grants each) fits here.
// The same bound applies to a state document; tickets have a smaller bound.
pub const MAX_BYTES: usize = 32 * 1024 * 1024;
pub const MAX_TICKET_BYTES: usize = 4096;

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
}
pub type Result<T> = std::result::Result<T, Error>;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Operation {
    Handshake,
    Inspect,
    PrepareMulticast,
    SaveAllocations,
    PrepareUnicast,
    Export,
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
    Handshake,
    Inspect {
        participant: ParticipantId,
    },
    Execute {
        participant: ParticipantId,
        operation: Operation,
    },
    Export {
        participant: ParticipantId,
        resource: ResourceKind,
        allocation: AllocationId,
    },
}

#[derive(Debug, Serialize, Deserialize)]
pub struct Response {
    pub participant: ParticipantId,
    pub result: std::result::Result<Reply, String>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Reply {
    Handshake,
    Inspection {
        #[serde(deserialize_with = "bounded_vec::<_, _, MAX_RECORDS>")]
        records: Vec<Record>,
        live_raw_imports: u64,
        unsupported_creations: u64,
    },
    Completed {
        operation: Operation,
        bytes: u64,
        copy_us: u32,
    },
    Export {
        resource: ResourceKind,
        allocation: AllocationId,
    },
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Participant {
    pub id: ParticipantId,
    #[serde(deserialize_with = "bounded_vec::<_, _, MAX_RECORDS>")]
    pub records: Vec<Record>,
}

#[derive(Serialize, Deserialize)]
struct Envelope<T> {
    version: u16,
    body: T,
}

/// Encodes an owned metadata value inside the current version envelope.
///
/// # Errors
/// Returns serialization failures or an error when the encoded size exceeds `MAX_BYTES`.
pub fn encode<T: Serialize>(body: &T) -> Result<Vec<u8>> {
    let bytes = rmp_serde::to_vec_named(&Envelope {
        version: VERSION,
        body,
    })?;
    if bytes.len() > MAX_BYTES {
        return Err(Error::Invalid("message exceeds size limit"));
    }
    Ok(bytes)
}

/// Decodes one versioned metadata value without retaining references to the input.
///
/// # Errors
/// Rejects malformed, oversized, too deeply nested, obsolete, or trailing data.
pub fn decode<T: DeserializeOwned>(bytes: &[u8]) -> Result<T> {
    if bytes.len() > MAX_BYTES {
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

/// Reject oversized sequences before Serde allocates from their size hint.
/// This is shared only by the two bounded collections in inspection metadata.
fn bounded_vec<'de, D, T, const LIMIT: usize>(
    deserializer: D,
) -> std::result::Result<Vec<T>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    struct Sequence<T, const N: usize>(std::marker::PhantomData<T>);
    impl<'de, T: Deserialize<'de>, const N: usize> serde::de::Visitor<'de> for Sequence<T, N> {
        type Value = Vec<T>;
        fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
            write!(f, "at most {N} entries")
        }
        fn visit_seq<A: serde::de::SeqAccess<'de>>(
            self,
            mut seq: A,
        ) -> std::result::Result<Vec<T>, A::Error> {
            if seq.size_hint().is_some_and(|count| count > N) {
                return Err(serde::de::Error::custom("too many entries"));
            }
            let mut entries = Vec::with_capacity(seq.size_hint().unwrap_or(0));
            while let Some(entry) = seq.next_element()? {
                if entries.len() == N {
                    return Err(serde::de::Error::custom("too many entries"));
                }
                entries.push(entry);
            }
            Ok(entries)
        }
    }
    deserializer.deserialize_seq(Sequence::<T, LIMIT>(std::marker::PhantomData))
}

pub fn timeout(operation: Operation) -> Duration {
    let (variable, fallback) = match operation {
        Operation::SaveAllocations | Operation::LoadAllocations => {
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
