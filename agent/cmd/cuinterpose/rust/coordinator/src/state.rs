// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::{Participant, Result};
use cuinterpose_protocol::{self as protocol, MAX_BYTES};
use std::io::{Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

pub fn read(path: &Path) -> Result<Vec<Participant>> {
    let mut bytes = Vec::new();
    std::fs::File::open(path)?
        .take(MAX_BYTES as u64 + 1)
        .read_to_end(&mut bytes)?;
    let captured: Vec<protocol::Participant> = protocol::decode(&bytes)?;
    if captured.is_empty() {
        return Err("state has no participants".into());
    }
    Ok(captured
        .into_iter()
        .map(|p| Participant {
            id: p.id,
            records: p.records,
            ..Participant::default()
        })
        .collect())
}

pub fn write_atomic(path: &Path, participants: &mut [Participant]) -> Result<()> {
    participants.sort_by_key(|p| p.id);
    let captured: Vec<_> = participants
        .iter_mut()
        .map(|p| {
            p.records.sort();
            protocol::Participant {
                id: p.id,
                records: p.records.clone(),
            }
        })
        .collect();
    let bytes = protocol::encode(&captured)?;
    let directory = path.parent().ok_or("missing checkpoint directory")?;
    let temporary = directory.join(format!(".cuinterpose.state.{}.tmp", std::process::id()));
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&temporary)?;
    let result = (|| {
        file.write_all(&bytes)?;
        file.sync_all()?;
        std::fs::rename(&temporary, path)?;
        std::fs::File::open(directory)?.sync_all()
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&temporary);
    }
    Ok(result?)
}
