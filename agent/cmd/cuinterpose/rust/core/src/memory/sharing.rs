// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Encode shareable handles, export peer allocations, and track imported allocation
//! ownership.

use super::checkpoint::Phase;
use super::vmm::Allocation;
use super::{Memblock, ProcessState, VirtualAllocationHandle};
use crate::driver::context;
use crate::error::{Error as CoreError, Result};
use crate::runtime::{self, export_cache};
use cudarc::driver::sys::CUresult::*;
use cudarc::driver::sys::{CUmemAllocationHandleType, CUmemAllocationProp, CUmulticastObjectProp};
use cuinterpose_protocol::{
    self as protocol, AllocationId, AllocationReference, Error, NamespacePid, Reply, Request,
    Response, VIRTUAL_SHAREABLE_HANDLE_BYTES, VIRTUAL_SHAREABLE_HANDLE_MAGIC,
};
use rustix::fs::{MemfdFlags, memfd_create};
use std::collections::BTreeMap;
use std::fs::File;
use std::io::Write;
use std::os::fd::{AsFd, BorrowedFd, OwnedFd};
use std::os::unix::{fs::FileExt, net::UnixStream};
use std::sync::{Mutex, MutexGuard};

pub fn create(reference: AllocationReference) -> protocol::Result<OwnedFd> {
    let bytes = protocol::encode_virtual_shareable_handle(reference)?;
    let fd = memfd_create(c"cuinterpose-virtual-shareable-handle", MemfdFlags::CLOEXEC)
        .map_err(std::io::Error::from)?;
    let mut file = File::from(fd);
    file.write_all(&bytes)?;
    Ok(file.into())
}

/// Return None for foreign FDs so the caller rejects them before calling CUDA. Return
/// an invalid handle error for malformed virtual handles.
pub fn decode(fd: i32) -> protocol::Result<Option<AllocationReference>> {
    if fd < 0 {
        return Err(Error::Invalid("negative import descriptor"));
    }
    // Duplicate the caller's borrowed FD into an owned File for positional reads, so
    // RAII closes only the duplicate and never the application's descriptor.
    let borrowed = unsafe { BorrowedFd::borrow_raw(fd) };
    let file = File::from(borrowed.try_clone_to_owned()?);
    // Virtual handles use regular memfds, so other descriptor types, including pipes
    // and sockets, are foreign. A regular file that is too short is an invalid virtual
    // handle.
    let metadata = file.metadata()?;
    if !metadata.is_file() {
        return Ok(None);
    }
    let mut magic = [0; 4];
    file.read_exact_at(&mut magic, 0)?;
    if magic[..3] != VIRTUAL_SHAREABLE_HANDLE_MAGIC[..3] {
        return Ok(None);
    }
    if magic[3] != protocol::VERSION {
        return Err(Error::Invalid(
            "unsupported virtual shareable handle version",
        ));
    }
    if metadata.len() != VIRTUAL_SHAREABLE_HANDLE_BYTES as u64 {
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

#[derive(Clone, Copy, Debug)]
pub enum ExportMetadata {
    Unicast { size: usize },
    Multicast(CUmulticastObjectProp),
}

pub fn request_export(
    allocation: AllocationReference,
    serving_pid: NamespacePid,
) -> protocol::Result<(OwnedFd, ExportMetadata)> {
    if serving_pid == 0 {
        return Err(Error::Invalid("invalid export namespace PID"));
    }
    let control_dir =
        runtime::control_dir().map_err(|_| Error::Invalid("cuinterpose state is unavailable"))?;
    let socket = protocol::connect(
        &protocol::socket_path(control_dir, serving_pid),
        protocol::timeout(None),
    )?;
    let timeout = Some(protocol::timeout(None));
    socket.set_read_timeout(timeout)?;
    socket.set_write_timeout(timeout)?;
    protocol::send(
        &socket,
        &Request::Export {
            namespace_pid: serving_pid,
            allocation,
        },
        None,
    )?;
    let (response, fd): (Response, _) = protocol::receive(&socket)?;
    if response.namespace_pid != serving_pid {
        return Err(Error::Invalid("wrong export namespace PID"));
    }
    let reply = response.result.map_err(Error::Remote)?;
    let descriptor = fd.ok_or(Error::Invalid("exporter sent no descriptor"))?;
    Ok((descriptor, decode_export_metadata(reply)?))
}

fn decode_export_metadata(reply: Reply) -> protocol::Result<ExportMetadata> {
    match reply {
        Reply::UnicastExport { size } if size != 0 => Ok(ExportMetadata::Unicast {
            size: size
                .try_into()
                .map_err(|_| Error::Invalid("unicast size exceeds host size"))?,
        }),
        Reply::MulticastExport { properties } => Ok(ExportMetadata::Multicast(
            decode_multicast_properties(properties)?,
        )),
        _ => Err(Error::Invalid("invalid export response")),
    }
}

fn decode_multicast_properties(
    properties: protocol::MulticastProperties,
) -> protocol::Result<CUmulticastObjectProp> {
    let protocol::MulticastProperties {
        devices,
        size,
        handle_types,
        flags,
    } = properties;
    if devices == 0
        || size == 0
        || handle_types
            != u64::from(CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0)
        || flags != 0
    {
        return Err(Error::Invalid("invalid multicast export properties"));
    }
    Ok(CUmulticastObjectProp {
        numDevices: devices,
        size: size
            .try_into()
            .map_err(|_| Error::Invalid("multicast size exceeds host size"))?,
        handleTypes: handle_types,
        flags,
    })
}

// Cache complete backing metadata with the FD so an importer can preserve it after
// the creator drops its local references. Serving exports never locks CUDA state.
pub(crate) type Exports = BTreeMap<AllocationId, (AllocationReference, OwnedFd, Reply)>;

#[derive(Default)]
pub struct ExportCache {
    exports: Mutex<Exports>,
}

impl ExportCache {
    pub fn contains(&self, reference: &AllocationReference) -> Result<bool> {
        Ok(self
            .exports
            .lock()
            .map_err(|_| CUDA_ERROR_UNKNOWN)?
            .get(&reference.id)
            .is_some_and(|(known, _, _)| known == reference))
    }

    pub fn insert(
        &self,
        reference: AllocationReference,
        descriptor: OwnedFd,
        metadata: ExportMetadata,
    ) -> Result<()> {
        let reply = match metadata {
            ExportMetadata::Multicast(properties) => Reply::MulticastExport {
                properties: protocol::MulticastProperties {
                    devices: properties.numDevices,
                    size: properties.size as u64,
                    handle_types: properties.handleTypes,
                    flags: properties.flags,
                },
            },
            ExportMetadata::Unicast { size } => {
                if size == 0 {
                    return Err(CUDA_ERROR_INVALID_VALUE.into());
                }
                Reply::UnicastExport { size: size as u64 }
            }
        };
        let mut exports = self.exports.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        if exports
            .get(&reference.id)
            .is_some_and(|(known, _, _)| *known != reference)
        {
            return Err(CUDA_ERROR_INVALID_HANDLE.into());
        }
        exports.insert(reference.id, (reference, descriptor, reply));
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
        reference: &AllocationReference,
    ) -> protocol::Result<()> {
        let exports = self
            .exports
            .lock()
            .map_err(|_| protocol::Error::Invalid("export cache poisoned"))?;
        let (result, descriptor) = match exports.get(&reference.id) {
            Some((known, fd, reply)) if known == reference => (Ok(reply.clone()), Some(fd)),
            _ => (Err("allocation resource is unavailable".into()), None),
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

impl Memblock {
    /// Publish the creator's export without requiring the peer service to lock
    /// ProcessState.
    pub fn export(&mut self, namespace_pid: NamespacePid) -> Result<AllocationReference> {
        if let Self::Unicast(allocation) = self
            && allocation.context == 0
        {
            allocation.context = context()?;
        }
        let reference = self.reference();
        if reference.creator_pid == namespace_pid && !export_cache()?.contains(&reference)? {
            let fd = crate::driver::export_posix(self.driver_handle()?)?;
            let metadata = match self {
                Self::Unicast(allocation) => ExportMetadata::Unicast {
                    size: allocation.size,
                },
                Self::Multicast(object) => ExportMetadata::Multicast(object.properties),
            };
            export_cache()?.insert(reference, fd, metadata)?;
        }
        match self {
            Self::Unicast(allocation) => {
                allocation.shared = true;
            }
            Self::Multicast(object) => object.shared = true,
        }
        Ok(reference)
    }
}

pub(crate) fn import_reference(
    mut state: MutexGuard<'static, ProcessState>,
    reference: AllocationReference,
) -> Result<(MutexGuard<'static, ProcessState>, u64)> {
    if state.phase != Phase::Active {
        return Err(CoreError::from(CUDA_ERROR_NOT_READY));
    }
    let id = reference.id;
    if let Some(memblock) = state.memblocks.get_mut(&id) {
        if memblock.reference() != reference {
            return Err(CoreError::from(CUDA_ERROR_INVALID_VALUE));
        }
        match memblock {
            Memblock::Unicast(allocation) => {
                if allocation.driver.is_none() {
                    let (raw, metadata) = request_export(reference, reference.creator_pid)
                        .map_err(CoreError::PeerExport)?;
                    if !matches!(metadata, ExportMetadata::Unicast { size } if size == allocation.size)
                    {
                        return Err(CoreError::from(CUDA_ERROR_INVALID_HANDLE));
                    }
                    allocation.driver = Some(crate::driver::import_posix(raw.as_fd())?);
                }
                allocation.shared = true;
            }
            Memblock::Multicast(object) => object.shared = true,
        }
        let handle = state.mint_virtual_allocation_handle(id)?;
        state
            .memblocks
            .get_mut(&id)
            .unwrap()
            .refcounts_mut()
            .handle_entries += 1;
        return Ok((state, handle));
    }
    // The EXPORT service uses the export cache without locking ProcessState, so a
    // request within this process can complete while its caller holds the allocation
    // metadata lock.
    let (raw, metadata) =
        request_export(reference, reference.creator_pid).map_err(CoreError::PeerExport)?;
    let size = match metadata {
        ExportMetadata::Unicast { size } => size,
        ExportMetadata::Multicast(properties) => {
            return super::multicast::import(state, reference, raw, properties);
        }
    };
    let context = context()?;
    let driver = crate::driver::import_posix(raw.as_fd())?;
    let driver = runtime::must_complete(VirtualAllocationHandle::from_driver(driver));
    let mut properties = std::mem::MaybeUninit::<CUmemAllocationProp>::zeroed();
    if let Err(error) = unsafe {
        crate::driver::cuMemGetAllocationPropertiesFromHandle(properties.as_mut_ptr(), driver)
    } {
        runtime::must_complete(unsafe { crate::driver::cuMemRelease(driver) });
        return Err(error);
    }
    let properties = unsafe { properties.assume_init() };
    if let Err(error) = super::vmm::validate_properties(&properties) {
        // A peer version may allow backing that this version does not support. Import
        // it to inspect its properties, then release the handle before publishing it.
        runtime::must_complete(unsafe { crate::driver::cuMemRelease(driver) });
        return Err(error);
    }
    let handle = runtime::must_complete(state.adopt_unicast(Allocation {
        reference,
        refcounts: Default::default(),
        driver: Some(driver),
        size,
        properties,
        shared: true,
        context,
    }));
    Ok((state, handle))
}
#[cfg(test)]
mod codec_tests {
    use super::*;
    use std::os::fd::AsRawFd;

    fn handle_file(bytes: &[u8]) -> File {
        let fd = memfd_create(c"test-shareable-handle", MemfdFlags::CLOEXEC).unwrap();
        let mut file = File::from(fd);
        file.write_all(bytes).unwrap();
        file
    }

    #[test]
    fn virtual_shareable_handle_round_trip() {
        let reference = AllocationReference {
            creator_pid: 1,
            id: [4; 16],
        };
        let file = File::from(create(reference).unwrap());
        assert_eq!(decode(file.as_raw_fd()).unwrap(), Some(reference));
        assert_eq!(
            file.metadata().unwrap().len(),
            VIRTUAL_SHAREABLE_HANDLE_BYTES as u64
        );
    }

    #[test]
    fn foreign_descriptors_are_untracked() {
        let foreign = File::open("/dev/null").unwrap();
        assert_eq!(decode(foreign.as_raw_fd()).unwrap(), None);
        let (socket, _peer) = UnixStream::pair().unwrap();
        assert_eq!(decode(socket.as_raw_fd()).unwrap(), None);
        let file = handle_file(b"foreign handle");
        assert_eq!(decode(file.as_raw_fd()).unwrap(), None);
    }

    #[test]
    fn truncated_and_oversized_virtual_handles_are_rejected() {
        let bytes = protocol::encode_virtual_shareable_handle(AllocationReference {
            creator_pid: 1,
            id: [4; 16],
        })
        .unwrap();
        for size in 0..bytes.len() {
            let file = handle_file(&bytes[..size]);
            assert!(decode(file.as_raw_fd()).is_err(), "accepted size {size}");
        }
        let mut oversized = bytes.to_vec();
        oversized.push(0);
        let file = handle_file(&oversized);
        assert!(decode(file.as_raw_fd()).is_err());
    }

    #[test]
    fn unsupported_virtual_handle_versions_are_rejected() {
        let mut bytes = protocol::encode_virtual_shareable_handle(AllocationReference {
            creator_pid: 1,
            id: [4; 16],
        })
        .unwrap();
        for version in [protocol::VERSION - 1, protocol::VERSION + 1] {
            bytes[3] = version;
            let file = handle_file(&bytes);
            assert!(matches!(decode(file.as_raw_fd()), Err(Error::Invalid(_))));
        }
    }

    #[test]
    fn unicast_exports_require_a_complete_nonzero_extent() {
        assert!(matches!(
            decode_export_metadata(Reply::UnicastExport { size: 8192 }).unwrap(),
            ExportMetadata::Unicast { size: 8192 }
        ));
        assert!(decode_export_metadata(Reply::UnicastExport { size: 0 }).is_err());
    }

    #[test]
    fn export_cache_checks_identity_and_serves_full_size_from_another_pid() {
        let cache = ExportCache::default();
        let reference = AllocationReference {
            creator_pid: 1,
            id: [4; 16],
        };
        cache
            .insert(
                reference,
                handle_file(b"backing").into(),
                ExportMetadata::Unicast { size: 8192 },
            )
            .unwrap();
        let other_creator = AllocationReference {
            creator_pid: 3,
            ..reference
        };
        assert!(cache.contains(&reference).unwrap());
        assert!(!cache.contains(&other_creator).unwrap());
        assert!(
            cache
                .insert(
                    other_creator,
                    handle_file(b"wrong backing").into(),
                    ExportMetadata::Unicast { size: 4096 },
                )
                .is_err()
        );
        let (sender, receiver) = UnixStream::pair().unwrap();
        cache.send(&sender, 2, &other_creator).unwrap();
        let (response, fd): (Response, _) = protocol::receive(&receiver).unwrap();
        assert!(response.result.is_err());
        assert!(fd.is_none());

        cache.send(&sender, 2, &reference).unwrap();
        let (response, fd): (Response, _) = protocol::receive(&receiver).unwrap();
        assert_eq!(response.namespace_pid, 2);
        assert!(matches!(
            response.result.unwrap(),
            Reply::UnicastExport { size: 8192 }
        ));
        let file = File::from(fd.unwrap());
        let mut contents = [0; 7];
        file.read_exact_at(&mut contents, 0).unwrap();
        assert_eq!(&contents, b"backing");

        cache.remove(&reference.id).unwrap();
        cache.send(&sender, 2, &reference).unwrap();
        let (response, fd): (Response, _) = protocol::receive(&receiver).unwrap();
        assert!(response.result.is_err());
        assert!(fd.is_none());
    }

    #[test]
    fn multicast_exports_require_supported_properties() {
        for (devices, size, handle_types, flags, accepted) in [
            (2, 4096, 1, 0, true),
            (0, 4096, 1, 0, false),
            (2, 0, 1, 0, false),
            (2, 4096, 0, 0, false),
            (2, 4096, 8, 0, false),
            (2, 4096, 9, 0, false),
            (2, 4096, 1, 1, false),
        ] {
            let properties = protocol::MulticastProperties {
                devices,
                size,
                handle_types,
                flags,
            };
            assert_eq!(
                decode_multicast_properties(properties).is_ok(),
                accepted,
                "{properties:?}"
            );
        }
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
        let reference = AllocationReference {
            creator_pid: 7,
            id: [1; 16],
        };
        let (socket, peer) = UnixStream::pair().unwrap();
        for metadata in [
            ExportMetadata::Unicast { size: 8192 },
            ExportMetadata::Multicast(CUmulticastObjectProp {
                numDevices: 2,
                size: 4096,
                handleTypes: 1,
                flags: 0,
            }),
        ] {
            cache
                .insert(reference, File::open("/dev/zero").unwrap().into(), metadata)
                .unwrap();
            cache.send(&socket, 7, &reference).unwrap();
            let (reply, fd): (Response, _) = protocol::receive(&peer).unwrap();
            assert_eq!(reply.namespace_pid, 7);
            match (reply.result.unwrap(), metadata) {
                (Reply::UnicastExport { size: 8192 }, ExportMetadata::Unicast { size: 8192 }) => {}
                (
                    Reply::MulticastExport {
                        properties:
                            protocol::MulticastProperties {
                                devices: 2,
                                size: 4096,
                                handle_types: 1,
                                flags: 0,
                            },
                    },
                    ExportMetadata::Multicast(_),
                ) => {}
                unexpected => panic!("wrong export reply: {unexpected:?}"),
            }
            let mut byte = [1];
            File::from(fd.unwrap()).read_exact(&mut byte).unwrap();
            assert_eq!(byte, [0]);
        }
        cache.remove(&reference.id).unwrap();
        cache.send(&socket, 7, &reference).unwrap();
        let (reply, fd): (Response, _) = protocol::receive(&peer).unwrap();
        assert!(reply.result.is_err());
        assert!(fd.is_none());
    }

    #[test]
    fn teardown_and_replacement_wait_for_socket_send() {
        for replace in [false, true] {
            let cache = Arc::new(ExportCache::default());
            let reference = AllocationReference {
                creator_pid: 7,
                id: [2; 16],
            };
            cache
                .insert(
                    reference,
                    File::open("/dev/zero").unwrap().into(),
                    ExportMetadata::Unicast { size: 8192 },
                )
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
            let sender =
                std::thread::spawn(move || sender_cache.send(&socket, 7, &reference).unwrap());
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
                        .insert(
                            reference,
                            File::open("/dev/null").unwrap().into(),
                            ExportMetadata::Unicast { size: 8192 },
                        )
                        .unwrap();
                } else {
                    mutation_cache.clear().unwrap();
                }
                done.send(()).unwrap();
            });
            assert!(completion.recv_timeout(Duration::from_millis(50)).is_err());
            peer.read_exact(&mut vec![0; filled]).unwrap();
            let (reply, fd): (Response, _) = protocol::receive(&peer).unwrap();
            assert!(matches!(
                reply.result,
                Ok(Reply::UnicastExport { size: 8192 })
            ));
            let mut byte = [1];
            File::from(fd.unwrap()).read_exact(&mut byte).unwrap();
            assert_eq!(byte, [0]);
            sender.join().unwrap();
            completion.recv_timeout(Duration::from_secs(5)).unwrap();
            mutation.join().unwrap();
            assert_eq!(cache.contains(&reference).unwrap(), replace);
        }
    }

    #[test]
    fn failed_send_does_not_block_teardown() {
        let cache = ExportCache::default();
        let reference = AllocationReference {
            creator_pid: 7,
            id: [3; 16],
        };
        cache
            .insert(
                reference,
                File::open("/dev/null").unwrap().into(),
                ExportMetadata::Unicast { size: 8192 },
            )
            .unwrap();
        let (socket, peer) = UnixStream::pair().unwrap();
        drop(peer);
        assert!(cache.send(&socket, 7, &reference).is_err());
        cache.clear().unwrap();
        assert!(!cache.contains(&reference).unwrap());
    }
}
