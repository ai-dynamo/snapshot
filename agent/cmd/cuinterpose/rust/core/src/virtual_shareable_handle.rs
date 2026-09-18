// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cudarc::driver::sys::CUmulticastObjectProp;
use cuinterpose_protocol::{
    self as protocol, AllocationReference, Error, Reply, Request, Response, Result,
    VIRTUAL_SHAREABLE_HANDLE_BYTES, VIRTUAL_SHAREABLE_HANDLE_MAGIC,
};
use rustix::fs::{MemfdFlags, memfd_create};
use std::fs::File;
use std::io::Write;
use std::os::fd::{BorrowedFd, OwnedFd};
use std::os::unix::{fs::FileExt, net::UnixStream};
use std::path::{Path, PathBuf};

pub fn create(reference: AllocationReference) -> Result<OwnedFd> {
    let bytes = protocol::encode_virtual_shareable_handle(reference)?;
    let fd = memfd_create(c"cuinterpose-virtual-shareable-handle", MemfdFlags::CLOEXEC)
        .map_err(std::io::Error::from)?;
    let mut file = File::from(fd);
    file.write_all(&bytes)?;
    Ok(file.into())
}

/// A foreign FD is a native import. A recognizable but invalid or obsolete
/// virtual shareable handle must not be passed through to the CUDA driver.
pub fn decode(fd: i32) -> Result<Option<AllocationReference>> {
    if fd < 0 {
        return Err(Error::Invalid("negative import descriptor"));
    }
    // The caller lends the FD for this call; never close its application-owned
    // descriptor. Clone it so positional File reads are safe and RAII-owned.
    let borrowed = unsafe { BorrowedFd::borrow_raw(fd) };
    let file = File::from(borrowed.try_clone_to_owned()?);
    let mut magic = [0; 4];
    if file.read_exact_at(&mut magic, 0).is_err() || magic != VIRTUAL_SHAREABLE_HANDLE_MAGIC {
        return Ok(None);
    }
    if file.metadata()?.len() as usize != VIRTUAL_SHAREABLE_HANDLE_BYTES {
        return Err(Error::Invalid("invalid virtual shareable handle size"));
    }
    let mut bytes = [0; VIRTUAL_SHAREABLE_HANDLE_BYTES];
    bytes[..VIRTUAL_SHAREABLE_HANDLE_MAGIC.len()].copy_from_slice(&magic);
    file.read_exact_at(
        &mut bytes[VIRTUAL_SHAREABLE_HANDLE_MAGIC.len()..],
        VIRTUAL_SHAREABLE_HANDLE_MAGIC.len() as u64,
    )?;
    Ok(Some(protocol::decode_virtual_shareable_handle(&bytes)?))
}

pub fn request_export(
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
        Reply::MulticastExport {
            devices,
            size,
            handle_types,
            flags,
        } => {
            if devices == 0 || size == 0 {
                return Err(Error::Invalid("invalid multicast export properties"));
            }
            Ok((
                descriptor,
                Some(CUmulticastObjectProp {
                    numDevices: devices,
                    size: size
                        .try_into()
                        .map_err(|_| Error::Invalid("multicast size exceeds host size"))?,
                    handleTypes: handle_types,
                    flags,
                }),
            ))
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
    fn virtual_shareable_handle_round_trip() {
        let reference = AllocationReference {
            creator: [1; 16],
            id: [4; 16],
        };
        let fd = create(reference).unwrap();
        assert_eq!(decode(fd.as_raw_fd()).unwrap(), Some(reference));
        assert_eq!(
            File::from(fd).metadata().unwrap().len(),
            VIRTUAL_SHAREABLE_HANDLE_BYTES as u64
        );
    }

    #[test]
    fn foreign_fd_is_native_but_obsolete_virtual_shareable_handle_is_rejected() {
        let foreign = File::open("/dev/null").unwrap();
        assert_eq!(decode(foreign.as_raw_fd()).unwrap(), None);
        let fd = memfd_create(c"obsolete-virtual-handle", MemfdFlags::empty()).unwrap();
        let mut file = File::from(fd);
        file.write_all(&VIRTUAL_SHAREABLE_HANDLE_MAGIC).unwrap();
        file.write_all(&[0; 252]).unwrap();
        assert!(decode(file.as_raw_fd()).is_err());
    }
}
