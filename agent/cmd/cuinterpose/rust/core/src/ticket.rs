// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cuinterpose_protocol::{
    self as protocol, AllocationReference, CUmulticastObjectProp, Error, Reply, Request, Response,
    Result, TICKET_BYTES, TICKET_MAGIC,
};
use rustix::fs::{MemfdFlags, SealFlags, fcntl_add_seals, fcntl_get_seals, memfd_create};
use std::fs::File;
use std::io::Write;
use std::os::fd::{BorrowedFd, OwnedFd};
use std::os::unix::{fs::FileExt, net::UnixStream};
use std::path::{Path, PathBuf};

const SEALS: SealFlags = SealFlags::SEAL
    .union(SealFlags::WRITE)
    .union(SealFlags::GROW)
    .union(SealFlags::SHRINK);

pub fn export(reference: AllocationReference) -> Result<OwnedFd> {
    if reference.id == [0; 16] {
        return Err(Error::Invalid("invalid allocation reference"));
    }
    let fd = memfd_create(
        c"cuinterpose-ticket",
        MemfdFlags::CLOEXEC | MemfdFlags::ALLOW_SEALING,
    )
    .map_err(std::io::Error::from)?;
    let mut file = File::from(fd);
    file.write_all(&TICKET_MAGIC)?;
    file.write_all(&reference.creator)?;
    file.write_all(&reference.id)?;
    fcntl_add_seals(&file, SEALS).map_err(std::io::Error::from)?;
    Ok(file.into())
}

/// A foreign FD is a native import. A recognizable but invalid/obsolete shim
/// ticket is an error, not an invitation to pass a memfd into the CUDA driver.
pub fn read(fd: i32) -> Result<Option<AllocationReference>> {
    if fd < 0 {
        return Err(Error::Invalid("negative import descriptor"));
    }
    // The caller lends the FD for this call; never close its application-owned
    // descriptor. Clone it so positional File reads are safe and RAII-owned.
    let borrowed = unsafe { BorrowedFd::borrow_raw(fd) };
    let file = File::from(borrowed.try_clone_to_owned()?);
    let mut magic = [0; 4];
    if file.read_exact_at(&mut magic, 0).is_err() || magic != TICKET_MAGIC {
        return Ok(None);
    }
    if file.metadata()?.len() as usize != TICKET_BYTES
        || !fcntl_get_seals(&file)
            .map_err(std::io::Error::from)?
            .contains(SEALS)
    {
        return Err(Error::Invalid("invalid ticket size or seals"));
    }
    let mut creator = [0; 16];
    let mut id = [0; 16];
    file.read_exact_at(&mut creator, TICKET_MAGIC.len() as u64)?;
    file.read_exact_at(&mut id, (TICKET_MAGIC.len() + creator.len()) as u64)?;
    if id == [0; 16] {
        return Err(Error::Invalid("invalid allocation reference"));
    }
    Ok(Some(AllocationReference { id, creator }))
}

pub fn request(
    allocation: AllocationReference,
) -> Result<(OwnedFd, Option<CUmulticastObjectProp>)> {
    let path = resolve(allocation.creator)?;
    match request_at(&path, allocation) {
        Ok(export) => Ok(export),
        Err(Error::Io(_)) | Err(Error::Invalid("wrong creator participant")) => {
            super::state::participant_directory()
                .map_err(|_| Error::Invalid("cuinterpose state is unavailable"))?
                .lock()
                .map_err(|_| Error::Invalid("participant directory is poisoned"))?
                .remove(&allocation.creator);
            request_at(&resolve(allocation.creator)?, allocation)
        }
        Err(error) => Err(error),
    }
}

fn request_at(
    path: &Path,
    allocation: AllocationReference,
) -> Result<(OwnedFd, Option<CUmulticastObjectProp>)> {
    let socket = super::process::Socket::open(|| UnixStream::connect(path))?;
    set_timeout(&socket)?;
    protocol::send(&socket, &Request::Export { allocation }, None)?;
    let (response, fd): (Response, _) = protocol::receive(&socket)?;
    if response.participant != allocation.creator {
        return Err(Error::Invalid("wrong creator participant"));
    }
    let descriptor = fd.ok_or(Error::Invalid("creator sent no descriptor"))?;
    match response.result.map_err(Error::Remote)? {
        Reply::UnicastExport => Ok((descriptor, None)),
        Reply::MulticastExport { properties } => {
            if properties.numDevices == 0 || properties.size == 0 {
                return Err(Error::Invalid("invalid multicast export properties"));
            }
            Ok((descriptor, Some(properties)))
        }
        _ => Err(Error::Invalid("invalid export response")),
    }
}

fn resolve(participant: [u8; 16]) -> Result<PathBuf> {
    if let Some(path) = super::state::participant_directory()
        .map_err(|_| Error::Invalid("cuinterpose state is unavailable"))?
        .lock()
        .map_err(|_| Error::Invalid("participant directory is poisoned"))?
        .get(&participant)
        .cloned()
    {
        return Ok(path);
    }
    rendezvous(participant)
}

fn rendezvous(target: [u8; 16]) -> Result<PathBuf> {
    let control_dir = super::state::control_dir()
        .map_err(|_| Error::Invalid("cuinterpose state is unavailable"))?;
    let mut paths: Vec<_> = std::fs::read_dir(control_dir)?
        .filter_map(|entry| entry.ok().map(|entry| entry.path()))
        .filter(|path| {
            path.file_name()
                .and_then(|name| name.to_str())
                .is_some_and(|name| name.starts_with("cuinterpose-") && name.ends_with(".sock"))
        })
        .collect();
    paths.sort();
    let mut discovered = Vec::new();
    for path in paths {
        let Ok(participant) = identify(&path) else {
            continue;
        };
        discovered.push((participant, path));
    }
    let mut directory = super::state::participant_directory()
        .map_err(|_| Error::Invalid("cuinterpose state is unavailable"))?
        .lock()
        .map_err(|_| Error::Invalid("participant directory is poisoned"))?;
    for (participant, path) in discovered {
        if directory
            .insert(participant, path.clone())
            .is_some_and(|previous| previous != path)
        {
            return Err(Error::Invalid("duplicate participant identity"));
        }
    }
    directory
        .get(&target)
        .cloned()
        .ok_or(Error::Invalid("creator participant is unavailable"))
}

fn identify(path: &Path) -> Result<[u8; 16]> {
    let socket = super::process::Socket::open(|| UnixStream::connect(path))?;
    set_timeout(&socket)?;
    protocol::send(&socket, &Request::Identify, None)?;
    let (response, fd): (Response, _) = protocol::receive(&socket)?;
    if fd.is_some() || !matches!(response.result.map_err(Error::Remote)?, Reply::Identified) {
        return Err(Error::Invalid("invalid identify response"));
    }
    Ok(response.participant)
}

fn set_timeout(socket: &UnixStream) -> Result<()> {
    let timeout = Some(protocol::timeout(None));
    socket.set_read_timeout(timeout)?;
    socket.set_write_timeout(timeout)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::fd::AsRawFd;

    #[test]
    fn sealed_ticket_round_trip() {
        let reference = AllocationReference {
            creator: [1; 16],
            id: [4; 16],
        };
        let fd = export(reference).unwrap();
        assert_eq!(read(fd.as_raw_fd()).unwrap(), Some(reference));
        assert_eq!(
            File::from(fd).metadata().unwrap().len(),
            TICKET_BYTES as u64
        );
    }

    #[test]
    fn foreign_fd_is_not_a_ticket_but_obsolete_shim_ticket_is_rejected() {
        let foreign = File::open("/dev/null").unwrap();
        assert_eq!(read(foreign.as_raw_fd()).unwrap(), None);
        let fd = memfd_create(c"obsolete-ticket", MemfdFlags::ALLOW_SEALING).unwrap();
        let mut file = File::from(fd);
        file.write_all(&TICKET_MAGIC).unwrap();
        file.write_all(&[0; 252]).unwrap();
        fcntl_add_seals(&file, SEALS).unwrap();
        assert!(read(file.as_raw_fd()).is_err());
    }
}
