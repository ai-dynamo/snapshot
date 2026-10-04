// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use anyhow::{Context, Result, ensure};
use cuinterpose_protocol::{self as protocol, MAX_MESSAGE_BYTES, Manifest};
use std::io::{Read, Write};
use std::path::Path;

pub fn read(path: &Path) -> Result<Manifest> {
    let mut bytes = Vec::new();
    // Read one byte over the limit to distinguish an oversized file from one
    // exactly at the limit. The protocol decoder rejects the oversized buffer.
    std::fs::File::open(path)?
        .take(MAX_MESSAGE_BYTES as u64 + 1)
        .read_to_end(&mut bytes)?;
    let participants: Manifest = protocol::decode(&bytes)?;
    ensure!(!participants.is_empty(), "state has no participants");
    Ok(participants)
}

pub fn write_atomic(path: &Path, participants: &mut Manifest) -> Result<()> {
    for participant in participants.values_mut() {
        participant.sort();
    }
    let bytes = protocol::encode(&participants)?;
    let directory = path.parent().context("missing checkpoint directory")?;
    // Publish the private temporary file without replacing a name that appeared
    // after preflight. Both fsync calls are needed for durable publication.
    let mut file = tempfile::NamedTempFile::new_in(directory)?;
    file.write_all(&bytes)?;
    file.as_file().sync_all()?;
    file.persist_noclobber(path)?;
    std::fs::File::open(directory)?.sync_all()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{PermissionsExt, symlink};

    #[test]
    fn new_manifest_is_readable_and_private() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("cuinterpose.state");
        let mut manifest = Manifest::from([(1, Vec::new())]);
        write_atomic(&path, &mut manifest).unwrap();
        assert_eq!(read(&path).unwrap(), manifest);
        assert_eq!(
            std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
    }

    #[test]
    fn publication_preserves_existing_files_and_symlinks() {
        for link in [false, true] {
            let directory = tempfile::tempdir().unwrap();
            let path = directory.path().join("cuinterpose.state");
            let original = b"previous checkpoint";
            if link {
                std::fs::write(directory.path().join("previous"), original).unwrap();
                symlink("previous", &path).unwrap();
            } else {
                std::fs::write(&path, original).unwrap();
            }
            assert!(write_atomic(&path, &mut Manifest::from([(2, Vec::new())])).is_err());
            assert_eq!(std::fs::read(&path).unwrap(), original);
            assert_eq!(std::fs::symlink_metadata(&path).unwrap().is_symlink(), link);
            let entries = std::fs::read_dir(directory.path()).unwrap().count();
            assert_eq!(entries, if link { 2 } else { 1 });
        }
    }
}
