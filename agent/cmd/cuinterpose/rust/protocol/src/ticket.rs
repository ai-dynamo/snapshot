// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::{AllocationId, Error, ParticipantId, Result};
use serde::{Deserialize, Serialize};

// Retain the recognizable signature, not the v2 layout. Recognizable obsolete
// tickets must fail decoding instead of reaching CUDA as raw export FDs.
pub const TICKET_MAGIC: &[u8; 4] = b"CMVD";

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ResourceKind {
    Unicast,
    Multicast,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Resource {
    Unicast,
    Multicast {
        devices: u32,
        size: u64,
        handle_types: u64,
        flags: u64,
    },
}

impl Resource {
    pub fn kind(&self) -> ResourceKind {
        match self {
            Self::Unicast => ResourceKind::Unicast,
            Self::Multicast { .. } => ResourceKind::Multicast,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Ticket {
    pub creator: ParticipantId,
    pub allocation: AllocationId,
    pub endpoint: String,
    pub resource: Resource,
}

impl Ticket {
    pub fn validate(&self) -> Result<()> {
        if self.allocation == AllocationId::default() || !self.endpoint.starts_with('/') {
            return Err(Error::Invalid("invalid ticket identity or endpoint"));
        }
        std::os::unix::net::SocketAddr::from_pathname(&self.endpoint)?;
        if let Resource::Multicast {
            devices,
            size,
            handle_types,
            ..
        } = self.resource
            && (devices == 0 || size == 0 || handle_types != 1)
        {
            return Err(Error::Invalid("invalid multicast ticket"));
        }
        Ok(())
    }
}
