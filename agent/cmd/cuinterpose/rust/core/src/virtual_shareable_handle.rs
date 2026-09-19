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
    let control_dir = super::state::control_dir()
        .map_err(|_| Error::Invalid("cuinterpose state is unavailable"))?;
    let socket = super::process::Socket::open(|| {
        UnixStream::connect(protocol::socket_path(control_dir, allocation.creator_pid))
    })?;
    let timeout = Some(protocol::timeout(None));
    socket.set_read_timeout(timeout)?;
    socket.set_write_timeout(timeout)?;
    protocol::send(&socket, &Request::Export { allocation }, None)?;
    let (response, fd): (Response, _) = protocol::receive(&socket)?;
    if response.namespace_pid != allocation.creator_pid {
        return Err(Error::Invalid("wrong creator namespace PID"));
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::fd::AsRawFd;

    #[test]
    fn virtual_shareable_handle_round_trip() {
        let reference = AllocationReference {
            creator_pid: 1,
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
