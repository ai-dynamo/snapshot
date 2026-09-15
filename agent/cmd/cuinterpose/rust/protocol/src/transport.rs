// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::{Error, MAX_BYTES, Result, decode, encode};
use rustix::net::{
    RecvAncillaryBuffer, RecvAncillaryMessage, RecvFlags, ReturnFlags, SendAncillaryBuffer,
    SendAncillaryMessage, SendFlags, recvmsg, sendmsg,
};
use serde::{Serialize, de::DeserializeOwned};
use std::io::{self, IoSlice, IoSliceMut, Read, Write};
use std::mem::MaybeUninit;
use std::os::fd::{AsFd, OwnedFd};
use std::os::unix::net::UnixStream;

/// Sends one bounded message, optionally transferring a borrowed descriptor.
///
/// # Errors
/// Returns serialization or socket errors; a failed send must not be retried
/// on this stream because a prefix or descriptor may already have been sent.
pub fn send<T: Serialize>(
    socket: &UnixStream,
    message: &T,
    descriptor: Option<&OwnedFd>,
) -> Result<()> {
    let body = encode(message)?;
    let mut bytes = Vec::with_capacity(4 + body.len());
    bytes.extend_from_slice(&(body.len() as u32).to_le_bytes());
    bytes.extend_from_slice(&body);
    let mut space = [MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(1))];
    let mut ancillary = SendAncillaryBuffer::new(&mut space);
    let descriptors = descriptor.map(|fd| [fd.as_fd()]);
    if let Some(descriptors) = &descriptors {
        ancillary.push(SendAncillaryMessage::ScmRights(descriptors));
    }
    let count = loop {
        match sendmsg(
            socket,
            &[IoSlice::new(&bytes)],
            &mut ancillary,
            SendFlags::NOSIGNAL,
        ) {
            Err(rustix::io::Errno::INTR) => continue,
            result => break result.map_err(io::Error::from)?,
        }
    };
    if count == 0 {
        return Err(Error::Invalid("control socket closed"));
    }
    // Never resend the ancillary FD after a partial sendmsg.
    (&*socket).write_all(&bytes[count..])?;
    Ok(())
}

/// Receives one bounded message and takes ownership of any transferred descriptor.
///
/// # Errors
/// Rejects truncated/oversized frames, invalid encoding, and excess descriptors.
/// Received descriptors are closed on error; the caller should close the stream.
pub fn receive<T: DeserializeOwned>(socket: &UnixStream) -> Result<(T, Option<OwnedFd>)> {
    let mut prefix = [0; 4];
    let mut space = [MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(2))];
    let mut ancillary = RecvAncillaryBuffer::new(&mut space);
    let received = loop {
        match recvmsg(
            socket,
            &mut [IoSliceMut::new(&mut prefix)],
            &mut ancillary,
            RecvFlags::CMSG_CLOEXEC,
        ) {
            Err(rustix::io::Errno::INTR) => continue,
            result => break result.map_err(io::Error::from)?,
        }
    };
    let mut descriptors = ancillary
        .drain()
        .filter_map(|message| match message {
            RecvAncillaryMessage::ScmRights(fds) => Some(fds),
            _ => None,
        })
        .flatten();
    let descriptor = descriptors.next();
    if received.bytes == 0
        || received
            .flags
            .intersects(ReturnFlags::CTRUNC | ReturnFlags::TRUNC)
        || descriptors.next().is_some()
    {
        return Err(Error::Invalid(
            "closed socket or invalid ancillary descriptors",
        ));
    }
    (&*socket).read_exact(&mut prefix[received.bytes..])?;
    let size = u32::from_le_bytes(prefix) as usize;
    if size > MAX_BYTES {
        return Err(Error::Invalid("message exceeds size limit"));
    }
    let mut bytes = vec![0; size];
    (&*socket).read_exact(&mut bytes)?;
    // Every received FD is owned before any fallible framing/decoding work.
    Ok((decode(&bytes)?, descriptor))
}
