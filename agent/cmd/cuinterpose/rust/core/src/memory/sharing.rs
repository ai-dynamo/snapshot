// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Shareable-handle codec, peer exports, and imported resource ownership.

use crate::driver::Result;
use crate::runtime;
use cudarc::driver::sys::CUmulticastObjectProp;
use cudarc::driver::sys::CUresult::*;
use cuinterpose_protocol::{
    self as protocol, AllocationId, AllocationReference, Error, NamespacePid, Reply, Request,
    Response, VIRTUAL_SHAREABLE_HANDLE_BYTES, VIRTUAL_SHAREABLE_HANDLE_MAGIC,
};
use rustix::fs::{MemfdFlags, memfd_create};
use std::collections::BTreeMap;
use std::fs::File;
use std::io::Write;
use std::os::fd::{AsRawFd, BorrowedFd, OwnedFd};
use std::os::unix::{fs::FileExt, net::UnixStream};
use std::sync::Mutex;

pub fn create(reference: AllocationReference) -> protocol::Result<OwnedFd> {
    let bytes = protocol::encode_virtual_shareable_handle(reference)?;
    let fd = memfd_create(c"cuinterpose-virtual-shareable-handle", MemfdFlags::CLOEXEC)
        .map_err(std::io::Error::from)?;
    let mut file = File::from(fd);
    file.write_all(&bytes)?;
    Ok(file.into())
}

/// A foreign FD is a native import. A recognizable but invalid or obsolete
/// virtual shareable handle must not be passed through to the CUDA driver.
pub fn decode(fd: i32) -> protocol::Result<Option<AllocationReference>> {
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
) -> protocol::Result<(OwnedFd, Option<CUmulticastObjectProp>)> {
    let control_dir =
        runtime::control_dir().map_err(|_| Error::Invalid("cuinterpose state is unavailable"))?;
    let socket = runtime::fork::Socket::open(|| {
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

// Multicast importers need the creation properties for checkpoint reconstruction.
// Cache the wire reply alongside its FD so sending never consults CUDA state.
pub(crate) type Exports = BTreeMap<AllocationId, (OwnedFd, Reply)>;

#[derive(Default)]
pub struct ExportCache {
    exports: Mutex<Exports>,
}

impl ExportCache {
    pub fn contains(&self, id: &AllocationId) -> Result<bool> {
        Ok(self
            .exports
            .lock()
            .map_err(|_| CUDA_ERROR_UNKNOWN)?
            .contains_key(id))
    }

    pub fn insert(
        &self,
        id: AllocationId,
        descriptor: OwnedFd,
        multicast: Option<CUmulticastObjectProp>,
    ) -> Result<()> {
        let reply = match multicast {
            Some(properties) => Reply::MulticastExport {
                devices: properties.numDevices,
                size: properties.size as u64,
                handle_types: properties.handleTypes,
                flags: properties.flags,
            },
            None => Reply::UnicastExport,
        };
        self.exports
            .lock()
            .map_err(|_| CUDA_ERROR_UNKNOWN)?
            .insert(id, (descriptor, reply));
        Ok(())
    }

    pub fn remove(&self, id: &AllocationId) -> Result<()> {
        self.exports
            .lock()
            .map_err(|_| CUDA_ERROR_UNKNOWN)?
            .remove(id);
        Ok(())
    }

    pub fn clear(&self) -> Result<()> {
        self.exports.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?.clear();
        Ok(())
    }

    pub fn send(
        &self,
        socket: &UnixStream,
        namespace_pid: NamespacePid,
        id: &AllocationId,
    ) -> protocol::Result<()> {
        let exports = self
            .exports
            .lock()
            .map_err(|_| protocol::Error::Invalid("export cache poisoned"))?;
        let (result, descriptor) = match exports.get(id) {
            Some((fd, reply)) => (Ok(reply.clone()), Some(fd)),
            None => (Err("creator resource is unavailable".into()), None),
        };
        protocol::send(
            socket,
            &Response {
                namespace_pid,
                result,
            },
            descriptor,
        )
    }
}

#[cfg(test)]
mod codec_tests {
    use super::*;

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

#[cfg(test)]
mod cache_tests {
    use super::*;
    use std::io::Read;
    use std::sync::{Arc, mpsc};
    use std::time::{Duration, Instant};

    #[test]
    fn exports_send_descriptors_and_multicast_metadata() {
        let cache = ExportCache::default();
        let id = [1; 16];
        let (socket, peer) = UnixStream::pair().unwrap();
        for multicast in [
            None,
            Some(CUmulticastObjectProp {
                numDevices: 2,
                size: 4096,
                handleTypes: 1,
                flags: 0,
            }),
        ] {
            cache
                .insert(id, File::open("/dev/zero").unwrap().into(), multicast)
                .unwrap();
            cache.send(&socket, 7, &id).unwrap();
            let (reply, fd): (Response, _) = protocol::receive(&peer).unwrap();
            assert_eq!(reply.namespace_pid, 7);
            match (reply.result.unwrap(), multicast) {
                (Reply::UnicastExport, None) => {}
                (
                    Reply::MulticastExport {
                        devices: 2,
                        size: 4096,
                        handle_types: 1,
                        flags: 0,
                    },
                    Some(_),
                ) => {}
                unexpected => panic!("wrong export reply: {unexpected:?}"),
            }
            let mut byte = [1];
            File::from(fd.unwrap()).read_exact(&mut byte).unwrap();
            assert_eq!(byte, [0]);
        }
        cache.remove(&id).unwrap();
        cache.send(&socket, 7, &id).unwrap();
        let (reply, fd): (Response, _) = protocol::receive(&peer).unwrap();
        assert!(reply.result.is_err());
        assert!(fd.is_none());
    }

    #[test]
    fn teardown_and_replacement_wait_for_socket_send() {
        for replace in [false, true] {
            let cache = Arc::new(ExportCache::default());
            let id = [2; 16];
            cache
                .insert(id, File::open("/dev/zero").unwrap().into(), None)
                .unwrap();
            let (mut socket, mut peer) = UnixStream::pair().unwrap();
            socket.set_nonblocking(true).unwrap();
            let mut filled = 0;
            loop {
                match socket.write(&[0; 4096]) {
                    Ok(bytes) => filled += bytes,
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => break,
                    result => panic!("filling socket: {result:?}"),
                }
            }
            socket.set_nonblocking(false).unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            peer.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
            let sender_cache = Arc::clone(&cache);
            let sender = std::thread::spawn(move || sender_cache.send(&socket, 7, &id).unwrap());
            let deadline = Instant::now() + Duration::from_secs(5);
            while cache.exports.try_lock().is_ok() {
                assert!(Instant::now() < deadline);
                std::thread::yield_now();
            }
            let mutation_cache = Arc::clone(&cache);
            let (done, completion) = mpsc::channel();
            let mutation = std::thread::spawn(move || {
                if replace {
                    mutation_cache
                        .insert(id, File::open("/dev/null").unwrap().into(), None)
                        .unwrap();
                } else {
                    mutation_cache.clear().unwrap();
                }
                done.send(()).unwrap();
            });
            assert!(completion.recv_timeout(Duration::from_millis(50)).is_err());
            peer.read_exact(&mut vec![0; filled]).unwrap();
            let (reply, fd): (Response, _) = protocol::receive(&peer).unwrap();
            assert!(matches!(reply.result, Ok(Reply::UnicastExport)));
            let mut byte = [1];
            File::from(fd.unwrap()).read_exact(&mut byte).unwrap();
            assert_eq!(byte, [0]);
            sender.join().unwrap();
            completion.recv_timeout(Duration::from_secs(5)).unwrap();
            mutation.join().unwrap();
            assert_eq!(cache.contains(&id).unwrap(), replace);
        }
    }

    #[test]
    fn failed_send_does_not_block_teardown() {
        let cache = ExportCache::default();
        let id = [3; 16];
        cache
            .insert(id, File::open("/dev/null").unwrap().into(), None)
            .unwrap();
        let (socket, peer) = UnixStream::pair().unwrap();
        drop(peer);
        assert!(cache.send(&socket, 7, &id).is_err());
        cache.clear().unwrap();
        assert!(!cache.contains(&id).unwrap());
    }
}
