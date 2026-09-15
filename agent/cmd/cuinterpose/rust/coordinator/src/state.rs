// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use anyhow::{Context, Result, ensure};
use cuinterpose_protocol::{self as protocol, MAX_BYTES, Participant};
use std::io::{Read, Write};
use std::path::Path;

pub fn read(path: &Path) -> Result<Vec<Participant>> {
    let mut bytes = Vec::new();
    std::fs::File::open(path)?
        .take(MAX_BYTES as u64 + 1)
        .read_to_end(&mut bytes)?;
    let participants: Vec<Participant> = protocol::decode(&bytes)?;
    ensure!(!participants.is_empty(), "state has no participants");
    Ok(participants)
}

pub fn write_atomic(path: &Path, participants: &mut [Participant]) -> Result<()> {
    participants.sort_by_key(|p| p.id);
    for participant in participants.iter_mut() {
        participant.records.sort();
    }
    let bytes = protocol::encode(&participants)?;
    let directory = path.parent().context("missing checkpoint directory")?;
    // NamedTempFile starts mode 0600 and removes incomplete files on error.
    // persist is atomic replacement, not durability: retain both fsyncs.
    let mut file = tempfile::NamedTempFile::new_in(directory)?;
    file.write_all(&bytes)?;
    file.as_file().sync_all()?;
    file.persist(path)?;
    std::fs::File::open(directory)?.sync_all()?;
    Ok(())
}
