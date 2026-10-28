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

pub const FABRIC_HANDLE_BYTES: usize = 64;

/// The bytes of a `CUmemFabricHandle`. Fabric handles stay real at runtime, so the
/// handle itself joins an importer to its creator in another process or Pod.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub struct FabricHandle(#[serde(with = "serde_bytes")] pub [u8; FABRIC_HANDLE_BYTES]);

impl FabricHandle {
    /// A stable digest that names the handle without revealing it, since any process in
    /// the fabric domain can import memory through the handle itself. Two FNV-1a lanes
    /// with different offsets make the digest 128 bits wide.
    pub fn digest(&self) -> [u8; 16] {
        let mut digest = [0; 16];
        for (lane, offset) in digest
            .chunks_exact_mut(8)
            .zip([0xcbf2_9ce4_8422_2325_u64, 0x8422_2325_cbf2_9ce4])
        {
            let hash = self.0.iter().fold(offset, |hash, byte| {
                (hash ^ u64::from(*byte)).wrapping_mul(0x0100_0000_01b3)
            });
            lane.copy_from_slice(&hash.to_le_bytes());
        }
        digest
    }
}

impl std::fmt::Debug for FabricHandle {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let digest = self.digest();
        write!(
            formatter,
            "FabricHandle({:016x})",
            u64::from_le_bytes(digest[..8].try_into().unwrap())
        )
    }
}
