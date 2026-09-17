// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::Error;
use serde::{Deserialize, Serialize};
use std::{fmt, str::FromStr};

// Distinct types prevent mixing process and allocation IDs in peer requests.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct ParticipantId(#[serde(with = "serde_bytes")] pub [u8; 16]);

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct AllocationId(#[serde(with = "serde_bytes")] pub [u8; 16]);

impl fmt::Display for ParticipantId {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(f, "{:032x}", u128::from_be_bytes(self.0))
    }
}

impl FromStr for ParticipantId {
    type Err = Error;
    fn from_str(text: &str) -> Result<Self, Error> {
        if text.len() != 32
            || !text
                .bytes()
                .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
        {
            return Err(Error::Invalid(
                "participant identity must be 32 lowercase hex digits",
            ));
        }
        let value = u128::from_str_radix(text, 16)
            .map_err(|_| Error::Invalid("invalid participant identity"))?;
        Ok(Self(value.to_be_bytes()))
    }
}
