// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The v2 control contract. Encoding is explicit: Rust layout, enum
//! discriminants, and uninitialized struct padding never enter the wire.

use std::io::{self, Read, Write};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::net::UnixStream;
use std::time::Duration;

mod record;
mod ticket;
pub use record::{Access, Record, RecordFlags, RecordKind};
pub use ticket::{TICKET_SIZE, Ticket};

pub const HEADER_SIZE: usize = 256;
pub const RECORD_SIZE: usize = 688;
pub const MAX_RECORDS: usize = 4096;
pub const MAGIC: u32 = 0x44564d4d;
pub const VERSION: u16 = 2;
pub const STATE_HEADER: &str = "cuinterpose-state-v2";
pub type Identity = [u8; 33];
pub type AllocationId = [u8; 16];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(u16)]
pub enum Operation {
    Handshake = 1,
    Inspect = 2,
    PrepareMulticast = 3,
    SaveAllocations = 4,
    PrepareUnicast = 5,
    Export = 6,
    LoadAllocations = 7,
    RestoreUnicast = 8,
    RestoreMulticastCreators = 9,
    RestoreMulticastImporters = 10,
    RestoreMulticastDevices = 11,
    RestoreMulticastBindings = 12,
}

impl TryFrom<u16> for Operation {
    type Error = io::Error;

    fn try_from(value: u16) -> io::Result<Self> {
        Ok(match value {
            1 => Self::Handshake,
            2 => Self::Inspect,
            3 => Self::PrepareMulticast,
            4 => Self::SaveAllocations,
            5 => Self::PrepareUnicast,
            6 => Self::Export,
            7 => Self::LoadAllocations,
            8 => Self::RestoreUnicast,
            9 => Self::RestoreMulticastCreators,
            10 => Self::RestoreMulticastImporters,
            11 => Self::RestoreMulticastDevices,
            12 => Self::RestoreMulticastBindings,
            _ => {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "unknown operation",
                ));
            }
        })
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Header {
    pub operation: Operation,
    pub status: i32,
    pub count: u32,
    pub payload_size: u64,
    pub participant: Identity,
    pub message: [u8; 96],
    pub allocation: AllocationId,
    pub resource_kind: u32,
    pub live_raw_imports: u32,
    pub unsupported_creations: u32,
    pub phase: u8,
    pub copy_us: u32,
}

impl Header {
    pub fn new(operation: Operation, participant: Identity) -> Self {
        Self {
            operation,
            participant,
            status: 0,
            count: 0,
            payload_size: 0,
            message: [0; 96],
            allocation: [0; 16],
            resource_kind: 0,
            live_raw_imports: 0,
            unsupported_creations: 0,
            phase: 0,
            copy_us: 0,
        }
    }

    pub fn encode(&self) -> [u8; HEADER_SIZE] {
        let mut b = [0; HEADER_SIZE];
        b[0..4].copy_from_slice(&MAGIC.to_le_bytes());
        b[4..6].copy_from_slice(&VERSION.to_le_bytes());
        b[6..8].copy_from_slice(&(self.operation as u16).to_le_bytes());
        b[8..12].copy_from_slice(&self.status.to_le_bytes());
        b[12..16].copy_from_slice(&self.count.to_le_bytes());
        b[16..24].copy_from_slice(&self.payload_size.to_le_bytes());
        b[24..57].copy_from_slice(&self.participant);
        b[57..153].copy_from_slice(&self.message);
        b[153..169].copy_from_slice(&self.allocation);
        b[172..176].copy_from_slice(&self.resource_kind.to_le_bytes());
        b[176..180].copy_from_slice(&self.live_raw_imports.to_le_bytes());
        b[180..184].copy_from_slice(&self.unsupported_creations.to_le_bytes());
        b[184] = self.phase;
        b[188..192].copy_from_slice(&self.copy_us.to_le_bytes());
        b
    }

    pub fn decode(b: &[u8; HEADER_SIZE]) -> io::Result<Self> {
        if u32::from_le_bytes(b[0..4].try_into().unwrap()) != MAGIC
            || u16::from_le_bytes(b[4..6].try_into().unwrap()) != VERSION
            || !b[24..57].contains(&0)
            || !b[57..153].contains(&0)
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "invalid control header",
            ));
        }
        Ok(Self {
            operation: u16::from_le_bytes(b[6..8].try_into().unwrap()).try_into()?,
            status: i32::from_le_bytes(b[8..12].try_into().unwrap()),
            count: u32::from_le_bytes(b[12..16].try_into().unwrap()),
            payload_size: u64::from_le_bytes(b[16..24].try_into().unwrap()),
            participant: b[24..57].try_into().unwrap(),
            message: b[57..153].try_into().unwrap(),
            allocation: b[153..169].try_into().unwrap(),
            resource_kind: u32::from_le_bytes(b[172..176].try_into().unwrap()),
            live_raw_imports: u32::from_le_bytes(b[176..180].try_into().unwrap()),
            unsupported_creations: u32::from_le_bytes(b[180..184].try_into().unwrap()),
            phase: b[184],
            copy_us: u32::from_le_bytes(b[188..192].try_into().unwrap()),
        })
    }
}

pub fn parse_identity(text: &[u8]) -> io::Result<Identity> {
    if text.len() != 32
        || !text
            .iter()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(b))
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid participant identity",
        ));
    }
    let mut id = [0; 33];
    id[..32].copy_from_slice(text);
    Ok(id)
}

pub fn timeout(operation: Operation) -> Duration {
    let (variable, fallback) = match operation {
        Operation::SaveAllocations | Operation::LoadAllocations => {
            ("SNAPSHOT_CARRIER_TIMEOUT_SECONDS", 3600)
        }
        _ => ("SNAPSHOT_CONTROL_TIMEOUT_SECONDS", 10),
    };
    let seconds = std::env::var(variable)
        .ok()
        .and_then(|s| s.parse::<u32>().ok())
        .filter(|n| *n > 0 && *n <= i32::MAX as u32)
        .unwrap_or(fallback);
    Duration::from_secs(seconds.into())
}

/// Send the descriptor with the first header bytes. The remaining bytes use
/// ordinary stream I/O: short sendmsg writes must not resend SCM_RIGHTS.
pub fn send_header(
    stream: &UnixStream,
    header: &Header,
    descriptor: Option<&OwnedFd>,
) -> io::Result<()> {
    let bytes = header.encode();
    let mut iov = libc::iovec {
        iov_base: bytes.as_ptr().cast_mut().cast(),
        iov_len: bytes.len(),
    };
    // usize alignment is sufficient for cmsghdr on supported Linux/amd64.
    let mut control = [0usize; 4];
    // SAFETY: msghdr is a C aggregate whose all-zero representation is valid.
    let mut message: libc::msghdr = unsafe { std::mem::zeroed() };
    message.msg_iov = &mut iov;
    message.msg_iovlen = 1;
    if let Some(fd) = descriptor {
        message.msg_control = control.as_mut_ptr().cast();
        // SAFETY: a single int descriptor fits in control, which stays live
        // through sendmsg. CMSG_FIRSTHDR returns that aligned buffer's header.
        unsafe {
            // glibc uses size_t here; musl uses socklen_t. This fixed-size
            // ancillary buffer fits both ABIs.
            message.msg_controllen = libc::CMSG_SPACE(size_of::<i32>() as u32) as _;
            let cmsg = libc::CMSG_FIRSTHDR(&message);
            (*cmsg).cmsg_level = libc::SOL_SOCKET;
            (*cmsg).cmsg_type = libc::SCM_RIGHTS;
            (*cmsg).cmsg_len = libc::CMSG_LEN(size_of::<i32>() as u32) as _;
            libc::CMSG_DATA(cmsg).cast::<i32>().write(fd.as_raw_fd());
        }
    }
    let written = loop {
        // SAFETY: message references initialized buffers live for this call.
        let n = unsafe { libc::sendmsg(stream.as_raw_fd(), &message, libc::MSG_NOSIGNAL) };
        if n >= 0 {
            break n as usize;
        }
        let error = io::Error::last_os_error();
        if error.kind() != io::ErrorKind::Interrupted {
            return Err(error);
        }
    };
    if written == 0 {
        return Err(io::Error::new(
            io::ErrorKind::WriteZero,
            "control socket closed",
        ));
    }
    (&*stream).write_all(&bytes[written..])
}

/// Receive at most one descriptor, closing *all* received descriptors on
/// malformed/truncated ancillary data or a malformed header.
pub fn receive_header(stream: &UnixStream) -> io::Result<(Header, Option<OwnedFd>)> {
    let mut bytes = [0; HEADER_SIZE];
    let mut iov = libc::iovec {
        iov_base: bytes.as_mut_ptr().cast(),
        iov_len: bytes.len(),
    };
    let mut control = [0usize; 40];
    // SAFETY: the zeroed C message is filled with valid writable buffers.
    let mut message: libc::msghdr = unsafe { std::mem::zeroed() };
    message.msg_iov = &mut iov;
    message.msg_iovlen = 1;
    message.msg_control = control.as_mut_ptr().cast();
    message.msg_controllen = size_of_val(&control) as _;
    let received = loop {
        // SAFETY: recvmsg writes only within the provided buffers.
        let n = unsafe { libc::recvmsg(stream.as_raw_fd(), &mut message, libc::MSG_CMSG_CLOEXEC) };
        if n >= 0 {
            break n as usize;
        }
        let error = io::Error::last_os_error();
        if error.kind() != io::ErrorKind::Interrupted {
            return Err(error);
        }
    };
    let mut descriptors = Vec::new();
    let mut invalid = message.msg_flags & (libc::MSG_CTRUNC | libc::MSG_TRUNC) != 0;
    // SAFETY: the kernel constructs cmsg lengths within msg_controllen.
    // Every installed SCM_RIGHTS fd is immediately given an owning guard.
    unsafe {
        let mut cmsg = libc::CMSG_FIRSTHDR(&message);
        while !cmsg.is_null() {
            let base = libc::CMSG_LEN(0) as usize;
            if (*cmsg).cmsg_level == libc::SOL_SOCKET
                && (*cmsg).cmsg_type == libc::SCM_RIGHTS
                && (*cmsg).cmsg_len as usize >= base
            {
                let length = (*cmsg).cmsg_len as usize - base;
                invalid |= length % size_of::<i32>() != 0;
                for index in 0..length / size_of::<i32>() {
                    descriptors.push(OwnedFd::from_raw_fd(
                        libc::CMSG_DATA(cmsg).cast::<i32>().add(index).read(),
                    ));
                }
            } else {
                invalid = true;
            }
            cmsg = libc::CMSG_NXTHDR(&message, cmsg);
        }
    }
    if invalid || descriptors.len() > 1 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid control descriptors",
        ));
    }
    if received == 0 {
        return Err(io::Error::new(
            io::ErrorKind::UnexpectedEof,
            "control socket closed",
        ));
    }
    (&*stream).read_exact(&mut bytes[received..])?;
    Ok((Header::decode(&bytes)?, descriptors.pop()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn header_preserves_all_defined_fields() {
        let mut h = Header::new(
            Operation::Export,
            parse_identity(b"0123456789abcdef0123456789abcdef").unwrap(),
        );
        h.status = -1;
        h.count = 7;
        h.payload_size = u64::MAX;
        h.message[..5].copy_from_slice(b"error");
        h.allocation = [19; 16];
        h.resource_kind = 2;
        h.live_raw_imports = 3;
        h.unsupported_creations = 4;
        h.phase = 5;
        h.copy_us = 987654;
        assert_eq!(Header::decode(&h.encode()).unwrap(), h);
        assert_eq!(&h.encode()[169..172], &[0; 3]);
    }

    #[test]
    fn socket_transfers_an_owned_cloexec_descriptor() {
        let (left, right) = UnixStream::pair().unwrap();
        let file: OwnedFd = std::fs::File::open("/dev/null").unwrap().into();
        let h = Header::new(Operation::Export, [0; 33]);
        send_header(&left, &h, Some(&file)).unwrap();
        let (received, fd) = receive_header(&right).unwrap();
        assert_eq!(received, h);
        // SAFETY: fd is live and F_GETFD has no additional arguments.
        assert_ne!(
            unsafe { libc::fcntl(fd.unwrap().as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
            0
        );
    }

    #[test]
    fn rejects_invalid_version_and_unterminated_identity() {
        let h = Header::new(Operation::Handshake, [0; 33]);
        let mut bytes = h.encode();
        bytes[4] = 1;
        assert!(Header::decode(&bytes).is_err());
        bytes = h.encode();
        bytes[24..57].fill(b'a');
        assert!(Header::decode(&bytes).is_err());
    }

    #[test]
    fn header_matches_c_v2_known_bytes() {
        let mut header = Header::new(
            Operation::Export,
            parse_identity(b"0123456789abcdef0123456789abcdef").unwrap(),
        );
        header.status = -1;
        header.count = 2;
        header.payload_size = 0x0102030405060708;
        header.message[..3].copy_from_slice(b"bad");
        header.allocation = [0x12; 16];
        header.resource_kind = 2;
        header.live_raw_imports = 3;
        header.unsupported_creations = 4;
        header.phase = 5;
        header.copy_us = 0x12345678;
        let mut expected = [0; 256];
        expected[..8].copy_from_slice(&[0x4d, 0x4d, 0x56, 0x44, 2, 0, 6, 0]);
        expected[8..12].fill(0xff);
        expected[12] = 2;
        expected[16..24].copy_from_slice(&[8, 7, 6, 5, 4, 3, 2, 1]);
        expected[24..57].copy_from_slice(&header.participant);
        expected[57..60].copy_from_slice(b"bad");
        expected[153..169].fill(0x12);
        expected[172] = 2;
        expected[176] = 3;
        expected[180] = 4;
        expected[184] = 5;
        expected[188..192].copy_from_slice(&[0x78, 0x56, 0x34, 0x12]);
        assert_eq!(header.encode(), expected);
        assert_eq!(Header::decode(&expected).unwrap(), header);
        // Reserved/padding input is ignored, and freshly sent headers zero it.
        expected[169] = 99;
        expected[255] = 99;
        assert_eq!(Header::decode(&expected).unwrap(), header);
    }
}
