// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::{Participant, Result};
use cuinterpose_protocol::{MAX_RECORDS, RECORD_SIZE, Record, STATE_HEADER};
use std::io::{BufRead, BufReader, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

pub fn read(path: &Path) -> Result<Vec<Participant>> {
    let mut input = BufReader::new(
        std::fs::File::open(path)
            .map_err(|error| format!("{} is missing or unreadable: {error}", path.display()))?,
    );
    let mut line = String::new();
    input.read_line(&mut line)?;
    if line != format!("{STATE_HEADER}\n") {
        return Err("invalid state header".into());
    }
    let mut participants = Vec::new();
    loop {
        line.clear();
        if input.read_line(&mut line)? == 0 {
            break;
        }
        let fields: Vec<_> = line.split_whitespace().collect();
        if fields.len() != 3 || fields[0] != "participant" {
            return Err("invalid participant record".into());
        }
        let id = cuinterpose_protocol::parse_identity(fields[1].as_bytes())?;
        let count = fields[2].parse::<usize>()?;
        if count > MAX_RECORDS {
            return Err("too many records".into());
        }
        let mut participant = Participant {
            id,
            ..Participant::default()
        };
        for _ in 0..count {
            line.clear();
            input.read_line(&mut line)?;
            if line.len() != RECORD_SIZE * 2 + 1 || !line.ends_with('\n') {
                return Err("invalid encoded record size".into());
            }
            let mut bytes = [0; RECORD_SIZE];
            for (out, pair) in bytes
                .iter_mut()
                .zip(line.as_bytes()[..RECORD_SIZE * 2].chunks_exact(2))
            {
                let high = (pair[0] as char).to_digit(16).ok_or("invalid record hex")?;
                let low = (pair[1] as char).to_digit(16).ok_or("invalid record hex")?;
                *out = ((high << 4) | low) as u8;
            }
            participant.records.push(Record::decode(&bytes)?);
        }
        participants.push(participant);
    }
    if participants.is_empty() {
        return Err("state has no participants".into());
    }
    Ok(participants)
}

pub fn write_atomic(path: &Path, participants: &mut [Participant]) -> Result<()> {
    participants.sort_by_key(|p| p.id);
    let directory = path.parent().ok_or("missing checkpoint directory")?;
    let temporary = directory.join(format!(".cuinterpose.state.{}.tmp", std::process::id()));
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&temporary)?;
    let result = (|| -> Result<()> {
        writeln!(file, "{STATE_HEADER}")?;
        for participant in participants {
            participant.records.sort();
            writeln!(
                file,
                "participant {} {}",
                std::str::from_utf8(&participant.id[..32])?,
                participant.records.len()
            )?;
            for record in &participant.records {
                let mut line = [0u8; RECORD_SIZE * 2 + 1];
                for (index, byte) in record.encode().iter().enumerate() {
                    line[index * 2] = b"0123456789abcdef"[(byte >> 4) as usize];
                    line[index * 2 + 1] = b"0123456789abcdef"[(byte & 15) as usize];
                }
                line[RECORD_SIZE * 2] = b'\n';
                file.write_all(&line)?;
            }
        }
        file.sync_all()?;
        std::fs::rename(&temporary, path)?;
        std::fs::File::open(directory)?.sync_all()?;
        Ok(())
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&temporary);
    }
    result
}
