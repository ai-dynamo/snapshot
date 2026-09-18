// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use anyhow::{Context, Result, ensure};
use cuinterpose_protocol::{self as protocol, MAX_ENTRIES, MAX_MESSAGE_BYTES, Manifest};
use std::io::{Read, Write};
use std::path::Path;

pub fn read(path: &Path) -> Result<Manifest> {
    let mut bytes = Vec::new();
    std::fs::File::open(path)?
        .take(MAX_MESSAGE_BYTES as u64 + 1)
        .read_to_end(&mut bytes)?;
    let participants: Manifest = protocol::decode(&bytes)?;
    ensure!(!participants.is_empty(), "state has no participants");
    ensure!(
        participants
            .values()
            .all(|participant| participant.entries.len() <= MAX_ENTRIES),
        "state participant has too many entries"
    );
    Ok(participants)
}

pub fn write_atomic(path: &Path, participants: &mut Manifest) -> Result<()> {
    ensure!(
        participants
            .values()
            .all(|participant| participant.entries.len() <= MAX_ENTRIES),
        "state participant has too many entries"
    );
    for participant in participants.values_mut() {
        participant.entries.sort();
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
