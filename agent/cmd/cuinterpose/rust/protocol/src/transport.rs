// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use crate::{Error, MAX_MESSAGE_BYTES, Result, decode, encode};
use rustix::net::{
    RecvAncillaryBuffer, RecvAncillaryMessage, RecvFlags, ReturnFlags, SendAncillaryBuffer,
    SendAncillaryMessage, SendFlags, recvmsg, sendmsg,
};
use serde::{Serialize, de::DeserializeOwned};
use std::io::{self, IoSlice, IoSliceMut, Read, Write};
use std::mem::MaybeUninit;
use std::os::fd::{AsFd, OwnedFd};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::time::Duration;

/// Connect to a local peer with a bounded wait for space in its listen queue.
///
/// # Errors
/// Returns socket creation, timeout configuration, or connection errors, including
/// rustix rejecting a zero timeout before connecting.
pub fn connect(path: &Path, timeout: Duration) -> io::Result<UnixStream> {
    use rustix::net::sockopt::{Timeout, set_socket_timeout};
    use rustix::net::{AddressFamily, SocketAddrUnix, SocketFlags, SocketType, socket_with};

    let address = SocketAddrUnix::new(path)?;
    let socket = socket_with(
        AddressFamily::UNIX,
        SocketType::STREAM,
        SocketFlags::CLOEXEC,
        None,
    )?;
    // On Linux, SO_SNDTIMEO limits blocking AF_UNIX connect calls as well as writes, so
    // it must be set before connecting rather than with the later read and write
    // timeouts.
    set_socket_timeout(&socket, Timeout::Send, Some(timeout))?;
    rustix::net::connect(&socket, &address)?;
    Ok(UnixStream::from(socket))
}

/// Send one message within the size limit, optionally transferring a borrowed
/// descriptor.
///
/// # Errors
/// Returns serialization or socket errors. The caller must close the stream instead of
/// retrying because part of the message or its descriptor may already have been sent.
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

/// Receive one message within the size limit and take ownership of any transferred
/// descriptor.
///
/// # Errors
/// Rejects truncated or oversized frames, invalid encoding, and excess descriptors.
/// Received descriptors are closed on error, and the caller should close the stream.
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
    if received.bytes == 0 {
        return Err(Error::Invalid("control socket closed"));
    }
    if received.flags.contains(ReturnFlags::CTRUNC) {
        return Err(Error::Invalid("control socket ancillary data truncated"));
    }
    if received.flags.contains(ReturnFlags::TRUNC) {
        return Err(Error::Invalid("control socket message truncated"));
    }
    if descriptors.next().is_some() {
        return Err(Error::Invalid("control socket received excess descriptors"));
    }
    (&*socket).read_exact(&mut prefix[received.bytes..])?;
    let size = u32::from_le_bytes(prefix) as usize;
    if size > MAX_MESSAGE_BYTES {
        return Err(Error::Invalid("message exceeds size limit"));
    }
    let mut bytes = vec![0; size];
    (&*socket).read_exact(&mut bytes)?;
    // Taking ownership of every received FD before checking or decoding the frame
    // ensures that later errors close all received descriptors.
    Ok((decode(&bytes)?, descriptor))
}

/// The largest frame on a relay's session socket: a value of `MAX_MESSAGE_BYTES` with its
/// key and request around it.
pub const SESSION_MAX_MESSAGE_BYTES: usize = MAX_MESSAGE_BYTES + 1024 * 1024;

/// Send one message on the session socket that a relay inherits from its agent. The body
/// is a bare MessagePack map, without the version envelope of control messages.
///
/// # Errors
/// Returns serialization or socket errors, and rejects bodies over
/// `SESSION_MAX_MESSAGE_BYTES`.
pub fn send_session<T: Serialize>(mut socket: &UnixStream, message: &T) -> Result<()> {
    let body = rmp_serde::to_vec_named(message)?;
    if body.len() > SESSION_MAX_MESSAGE_BYTES {
        return Err(Error::Invalid("session message exceeds size limit"));
    }
    let mut bytes = Vec::with_capacity(4 + body.len());
    bytes.extend_from_slice(&(body.len() as u32).to_le_bytes());
    bytes.extend_from_slice(&body);
    socket.write_all(&bytes)?;
    Ok(())
}

/// Receive one message from a relay's session socket. Returns `None` when the agent closed
/// the socket between messages.
///
/// # Errors
/// Rejects truncated or oversized frames and invalid encoding.
pub fn receive_session<T: DeserializeOwned>(mut socket: &UnixStream) -> Result<Option<T>> {
    let mut prefix = [0; 4];
    let mut read = 0;
    while read < prefix.len() {
        match socket.read(&mut prefix[read..]) {
            Ok(0) if read == 0 => return Ok(None),
            Ok(0) => return Err(Error::Invalid("session message truncated")),
            Ok(count) => read += count,
            Err(error) if error.kind() == io::ErrorKind::Interrupted => {}
            Err(error) => return Err(error.into()),
        }
    }
    let size = u32::from_le_bytes(prefix) as usize;
    if size > SESSION_MAX_MESSAGE_BYTES {
        return Err(Error::Invalid("session message exceeds size limit"));
    }
    let mut body = vec![0; size];
    socket.read_exact(&mut body)?;
    let mut decoder = rmp_serde::Deserializer::new(io::Cursor::new(&body));
    decoder.set_max_depth(32);
    let message = T::deserialize(&mut decoder)?;
    if decoder.position() != body.len() as u64 {
        return Err(Error::Invalid("trailing session message data"));
    }
    Ok(Some(message))
}
