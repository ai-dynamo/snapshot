// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use serde::{Deserialize, Serialize};

pub type NamespacePid = u32;
pub type AllocationId = [u8; 16];

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct AllocationReference {
    #[serde(with = "serde_bytes")]
    pub id: AllocationId,
    pub creator_pid: NamespacePid,
}
