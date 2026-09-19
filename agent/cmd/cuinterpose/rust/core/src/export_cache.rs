// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Peer exports never take the CUDA state lock. Hold the cache lock through
//! each send so removal, checkpoint teardown, and fork wait for it to finish.
//! The peer listener is already serial; no per-export transfer state is needed.

use crate::state::Result;
use cudarc::driver::sys::CUmulticastObjectProp;
use cudarc::driver::sys::CUresult::CUDA_ERROR_UNKNOWN;
use cuinterpose_protocol::{self as protocol, AllocationId, NamespacePid, Reply, Response};
use std::collections::BTreeMap;
use std::os::fd::{AsRawFd, OwnedFd};
use std::os::unix::net::UnixStream;
use std::sync::{Mutex, MutexGuard};

// Multicast importers need the creation properties for checkpoint reconstruction.
// Cache the wire reply alongside its FD so sending never consults CUDA state.
pub(super) type Exports = BTreeMap<AllocationId, (OwnedFd, Reply)>;

#[derive(Default)]
pub struct ExportCache {
    exports: Mutex<Exports>,
}

impl ExportCache {
    pub fn fork_lock(&self, descriptors: &mut Vec<i32>) -> MutexGuard<'_, Exports> {
        let exports = self.exports.lock().unwrap_or_else(|e| e.into_inner());
        descriptors.extend(exports.values().map(|(fd, _)| fd.as_raw_fd()));
        exports
    }

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
mod tests {
    use super::*;
    use std::fs::File;
    use std::io::{Read, Write};
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
