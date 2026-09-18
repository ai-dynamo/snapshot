// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::{Error, Result};
use serde::{Deserialize, Serialize};

pub type ParticipantId = [u8; 16];
pub type AllocationId = [u8; 16];

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct AllocationReference {
    #[serde(with = "serde_bytes")]
    pub id: AllocationId,
    #[serde(with = "serde_bytes")]
    pub creator: ParticipantId,
}

pub fn parse_participant_id(text: &str) -> Result<ParticipantId> {
    if text.len() != 32
        || !text
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
    {
        return Err(Error::Invalid(
            "participant identity must be 32 lowercase hex digits",
        ));
    }
    Ok(u128::from_str_radix(text, 16)
        .map_err(|_| Error::Invalid("invalid participant identity"))?
        .to_be_bytes())
}

pub fn format_id(id: &[u8; 16]) -> String {
    format!("{:032x}", u128::from_be_bytes(*id))
}
