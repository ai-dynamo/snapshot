// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cuinterpose_protocol::{
    self as protocol, Error, MAX_TICKET_BYTES, Reply, Request, Response, Result, TICKET_MAGIC,
    Ticket,
};
use rustix::fs::{MemfdFlags, SealFlags, fcntl_add_seals, fcntl_get_seals, memfd_create};
use std::fs::File;
use std::io::Write;
use std::os::fd::{BorrowedFd, OwnedFd};
use std::os::unix::{fs::FileExt, net::UnixStream};

const SEALS: SealFlags = SealFlags::SEAL
    .union(SealFlags::WRITE)
    .union(SealFlags::GROW)
    .union(SealFlags::SHRINK);

pub fn export(ticket: &Ticket) -> Result<OwnedFd> {
    ticket.validate()?;
    let bytes = protocol::encode(ticket)?;
    if bytes.len() + TICKET_MAGIC.len() > MAX_TICKET_BYTES {
        return Err(Error::Invalid("ticket exceeds size limit"));
    }
    let fd = memfd_create(
        c"cuinterpose-ticket",
        MemfdFlags::CLOEXEC | MemfdFlags::ALLOW_SEALING,
    )
    .map_err(std::io::Error::from)?;
    let mut file = File::from(fd);
    file.write_all(TICKET_MAGIC)?;
    file.write_all(&bytes)?;
    fcntl_add_seals(&file, SEALS).map_err(std::io::Error::from)?;
    Ok(file.into())
}

/// A foreign FD is a native import. A recognizable but invalid/obsolete shim
/// ticket is an error, not an invitation to pass a memfd into the CUDA driver.
pub fn read(fd: i32) -> Result<Option<Ticket>> {
    if fd < 0 {
        return Err(Error::Invalid("negative import descriptor"));
    }
    // The caller lends the FD for this call; never close its application-owned
    // descriptor. Clone it so positional File reads are safe and RAII-owned.
    let borrowed = unsafe { BorrowedFd::borrow_raw(fd) };
    let file = File::from(borrowed.try_clone_to_owned()?);
    let mut magic = [0; 4];
    if file.read_exact_at(&mut magic, 0).is_err() || &magic != TICKET_MAGIC {
        return Ok(None);
    }
    let size = file.metadata()?.len() as usize;
    if !(TICKET_MAGIC.len()..=MAX_TICKET_BYTES).contains(&size)
        || !fcntl_get_seals(&file)
            .map_err(std::io::Error::from)?
            .contains(SEALS)
    {
        return Err(Error::Invalid("invalid ticket size or seals"));
    }
    let mut bytes = vec![0; size - magic.len()];
    file.read_exact_at(&mut bytes, magic.len() as u64)?;
    let ticket: Ticket = protocol::decode(&bytes)?;
    ticket.validate()?;
    Ok(Some(ticket))
}

pub fn request(ticket: &Ticket) -> Result<OwnedFd> {
    let socket = super::process::Socket::open(|| UnixStream::connect(&ticket.endpoint))?;
    let timeout = Some(protocol::timeout(None));
    socket.set_read_timeout(timeout)?;
    socket.set_write_timeout(timeout)?;
    let resource = ticket.resource.kind();
    protocol::send(
        &socket,
        &Request::Export {
            participant: ticket.creator,
            resource,
            allocation: ticket.allocation,
        },
        None,
    )?;
    let (response, fd): (Response, _) = protocol::receive(&socket)?;
    match response {
        Response {
            participant,
            result:
                Ok(Reply::Export {
                    resource: actual,
                    allocation,
                }),
        } if participant == ticket.creator
            && actual == resource
            && allocation == ticket.allocation =>
        {
            fd.ok_or(Error::Invalid("creator sent no descriptor"))
        }
        Response { participant, .. } if participant != ticket.creator => {
            Err(Error::Invalid("wrong creator participant"))
        }
        Response {
            result: Err(message),
            ..
        } => Err(Error::Remote(message)),
        _ => Err(Error::Invalid(
            "invalid export response identity or resource",
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::fd::AsRawFd;

    #[test]
    fn sealed_ticket_round_trip() {
        let ticket = Ticket {
            creator: "0123456789abcdef0123456789abcdef".parse().unwrap(),
            allocation: protocol::AllocationId([4; 16]),
            endpoint: "/tmp/cuinterpose-123.sock".into(),
            resource: protocol::Resource::Unicast,
        };
        let fd = export(&ticket).unwrap();
        assert_eq!(read(fd.as_raw_fd()).unwrap(), Some(ticket));
        assert!(File::from(fd).write_at(b"x", 0).is_err());
    }

    #[test]
    fn foreign_fd_is_not_a_ticket_but_obsolete_shim_ticket_is_rejected() {
        let foreign = File::open("/dev/null").unwrap();
        assert_eq!(read(foreign.as_raw_fd()).unwrap(), None);
        let fd = memfd_create(c"obsolete-ticket", MemfdFlags::ALLOW_SEALING).unwrap();
        let mut file = File::from(fd);
        file.write_all(TICKET_MAGIC).unwrap();
        file.write_all(&[0; 252]).unwrap();
        fcntl_add_seals(&file, SEALS).unwrap();
        assert!(read(file.as_raw_fd()).is_err());
    }
}
