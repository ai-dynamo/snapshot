// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cuinterpose_protocol::{Header, Operation, TICKET_SIZE, Ticket};
use std::io;
use std::os::fd::{FromRawFd, OwnedFd};
use std::os::unix::net::UnixStream;

/// OS transport stays in the core; only the protocol crate knows ticket bytes.
pub fn export(ticket: &Ticket) -> io::Result<OwnedFd> {
    let bytes = ticket.encode()?;
    let raw = unsafe {
        libc::memfd_create(
            c"cuinterpose-ticket".as_ptr(),
            libc::MFD_CLOEXEC | libc::MFD_ALLOW_SEALING,
        )
    };
    if raw < 0 {
        return Err(io::Error::last_os_error());
    }
    let fd = unsafe { OwnedFd::from_raw_fd(raw) };
    let count = unsafe { libc::pwrite(raw, bytes.as_ptr().cast(), bytes.len(), 0) };
    if count != bytes.len() as isize {
        return Err(io::Error::other("cannot write ticket"));
    }
    let seals = libc::F_SEAL_SEAL | libc::F_SEAL_WRITE | libc::F_SEAL_GROW | libc::F_SEAL_SHRINK;
    if unsafe { libc::fcntl(raw, libc::F_ADD_SEALS, seals) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(fd)
}

pub fn read(fd: i32) -> io::Result<Ticket> {
    let seals = libc::F_SEAL_SEAL | libc::F_SEAL_WRITE | libc::F_SEAL_GROW | libc::F_SEAL_SHRINK;
    let actual = unsafe { libc::fcntl(fd, libc::F_GET_SEALS) };
    let mut stat: libc::stat = unsafe { std::mem::zeroed() };
    if actual < 0
        || actual & seals != seals
        || unsafe { libc::fstat(fd, &mut stat) } != 0
        || stat.st_size != TICKET_SIZE as i64
    {
        return Err(io::Error::other("not a sealed ticket"));
    }
    let mut bytes = [0u8; TICKET_SIZE];
    if unsafe { libc::pread(fd, bytes.as_mut_ptr().cast(), bytes.len(), 0) } != bytes.len() as isize
    {
        return Err(io::Error::other("cannot read ticket"));
    }
    Ticket::decode(&bytes)
}

pub fn request(ticket: &Ticket) -> io::Result<OwnedFd> {
    let socket = UnixStream::connect(&ticket.endpoint)?;
    let timeout = Some(cuinterpose_protocol::timeout(Operation::Export));
    socket.set_read_timeout(timeout)?;
    socket.set_write_timeout(timeout)?;
    let mut request = Header::new(Operation::Export, ticket.creator);
    request.resource_kind = ticket.resource;
    request.allocation = ticket.allocation;
    cuinterpose_protocol::send_header(&socket, &request, None)?;
    let (response, fd) = cuinterpose_protocol::receive_header(&socket)?;
    if response.operation != Operation::Export
        || response.participant != ticket.creator
        || response.resource_kind != ticket.resource
        || response.allocation != ticket.allocation
        || response.status != 0
        || response.count != 0
        || response.payload_size != 0
    {
        return Err(io::Error::other("creator rejected export"));
    }
    fd.ok_or_else(|| io::Error::other("creator sent no descriptor"))
}
#[cfg(test)]
mod tests {
    use super::*;
    use std::os::fd::AsRawFd;

    #[test]
    fn sealed_ticket_round_trip() {
        let ticket = Ticket {
            creator: cuinterpose_protocol::parse_identity(b"0123456789abcdef0123456789abcdef")
                .unwrap(),
            allocation: [4; 16],
            endpoint: "/tmp/cuinterpose-123.sock".into(),
            resource: 1,
            devices: 0,
            size: 0,
            handle_types: 0,
            flags: 0,
        };
        let fd = export(&ticket).unwrap();
        assert_eq!(read(fd.as_raw_fd()).unwrap(), ticket);
        assert_eq!(
            unsafe { libc::pwrite(fd.as_raw_fd(), b"x".as_ptr().cast(), 1, 0) },
            -1
        );
    }

    #[test]
    #[ignore = "run python3 core/tests/ticket_interop.py to compile the pinned C reader"]
    fn c_v2_sealed_ticket_interoperability() {
        use std::process::{Command, Stdio};

        let helper = std::env::var_os("CUINTERPOSE_C_TICKET_HELPER")
            .expect("the interoperability runner supplies the pinned C helper");
        let ticket = Ticket {
            creator: cuinterpose_protocol::parse_identity(b"0123456789abcdef0123456789abcdef")
                .unwrap(),
            allocation: [4; 16],
            endpoint: "/tmp/cuinterpose-interop.sock".into(),
            resource: 1,
            devices: 0,
            size: 0,
            handle_types: 0,
            flags: 0,
        };
        let rust_ticket = export(&ticket).unwrap();
        let (socket, child_socket) = UnixStream::pair().unwrap();
        socket
            .set_read_timeout(Some(std::time::Duration::from_secs(10)))
            .unwrap();
        // Stdio duplicates the sealed Rust ticket to fd 0 and the return socket
        // to fd 1. No pre_exec closure or inherited-CLOEXEC workaround is needed.
        let mut child = Command::new(helper)
            .stdin(Stdio::from(rust_ticket))
            .stdout(Stdio::from(OwnedFd::from(child_socket)))
            .spawn()
            .unwrap();
        let response = cuinterpose_protocol::receive_header(&socket);
        let status = child.wait().unwrap();
        assert!(status.success(), "pinned C reader/writer failed: {status}");
        let (header, c_ticket) = response.unwrap();
        assert_eq!(header.operation, Operation::Export);
        assert_eq!(header.participant, ticket.creator);
        assert_eq!(header.allocation, ticket.allocation);
        // The C helper has read the Rust-produced sealed ticket, then created a
        // new sealed ticket with the reference writer. Validate that new FD in
        // Rust, including its sealing and decoded fields.
        let c_ticket = c_ticket.unwrap();
        assert_eq!(read(c_ticket.as_raw_fd()).unwrap(), ticket);
    }
}
