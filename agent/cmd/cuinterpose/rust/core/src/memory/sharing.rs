// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Encode shareable handles, export peer allocations, and track imported allocation
//! ownership.

use super::checkpoint::Phase;
use super::vmm::{Allocation, location_metadata};
use super::{Memblock, ProcessState, VirtualAllocationHandle};
use crate::driver::context;
use crate::error::{Error as CoreError, Result};
use crate::runtime::{self, export_cache};
use cudarc::driver::sys::CUresult::*;
use cudarc::driver::sys::{
    CUmemAllocationHandleType, CUmemAllocationProp, CUmemLocationType, CUmulticastObjectProp,
};
use cuinterpose_protocol::{
    self as protocol, AllocationId, AllocationReference, Error, NamespacePid, Reply, Request,
    Response, VIRTUAL_SHAREABLE_HANDLE_BYTES, VIRTUAL_SHAREABLE_HANDLE_MAGIC,
};
use rustix::buffer::spare_capacity;
use rustix::event::{Timespec, epoll};
use rustix::fs::{FileType, fstat};
use rustix::net::{
    AddressFamily, RecvFlags, SendFlags, SocketFlags, SocketType, recv, send, socketpair, sockopt,
};
use std::collections::{BTreeMap, BTreeSet};
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, OwnedFd};
use std::os::unix::net::UnixStream;
use std::sync::{Mutex, MutexGuard};

/// Return None for foreign FDs so the caller imports them natively. Return
/// an invalid handle error for malformed virtual handles.
pub fn decode(fd: i32) -> protocol::Result<Option<AllocationReference>> {
    if fd < 0 {
        return Err(Error::Invalid("negative import descriptor"));
    }
    let ticket = unsafe { BorrowedFd::borrow_raw(fd) };
    // Virtual handles are Unix sequenced-packet sockets, so other descriptor types,
    // including CUDA export FDs, are foreign.
    let io = |error: rustix::io::Errno| Error::Io(error.into());
    if FileType::from_raw_mode(fstat(ticket).map_err(io)?.st_mode) != FileType::Socket
        || sockopt::socket_domain(ticket).map_err(io)? != AddressFamily::UNIX
        || sockopt::socket_type(ticket).map_err(io)? != SocketType::SEQPACKET
    {
        return Ok(None);
    }
    // Peeking leaves the packet queued, so every copy of the ticket can be imported.
    // With TRUNC, the length is the whole packet's, even past the buffer.
    let mut bytes = [0; VIRTUAL_SHAREABLE_HANDLE_BYTES];
    let flags = RecvFlags::PEEK | RecvFlags::DONTWAIT | RecvFlags::TRUNC;
    let length = match recv(ticket, &mut bytes, flags) {
        Ok((_, length)) => length,
        Err(rustix::io::Errno::AGAIN) => return Ok(None),
        Err(error) => return Err(io(error)),
    };
    if length < 3 || bytes[..3] != VIRTUAL_SHAREABLE_HANDLE_MAGIC[..3] {
        return Ok(None);
    }
    if bytes[3] != protocol::VERSION {
        return Err(Error::Invalid(
            "unsupported virtual shareable handle version",
        ));
    }
    if length != VIRTUAL_SHAREABLE_HANDLE_BYTES {
        return Err(Error::Invalid("invalid virtual shareable handle size"));
    }
    Ok(Some(protocol::decode_virtual_shareable_handle(&bytes)?))
}

#[derive(Clone, Copy, Debug)]
pub enum ExportMetadata {
    Unicast {
        size: usize,
        location: protocol::MemoryLocation,
    },
    Multicast(CUmulticastObjectProp),
}

pub fn request_export(
    allocation: AllocationReference,
    serving_pid: NamespacePid,
) -> protocol::Result<(OwnedFd, ExportMetadata)> {
    if serving_pid == 0 {
        return Err(Error::Invalid("invalid export namespace PID"));
    }
    let socket_dir =
        runtime::socket_dir().map_err(|_| Error::Invalid("cuinterpose state is unavailable"))?;
    let socket = protocol::connect(
        &protocol::socket_path(socket_dir, serving_pid),
        protocol::control_timeout(),
    )?;
    let timeout = Some(protocol::control_timeout());
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
        Reply::UnicastExport { size, location } if size != 0 => Ok(ExportMetadata::Unicast {
            size: size
                .try_into()
                .map_err(|_| Error::Invalid("unicast size exceeds host size"))?,
            location,
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
struct Export {
    reference: AllocationReference,
    descriptor: OwnedFd,
    reply: Reply,
    // The creator dropped every local reference while a ticket was open.
    released: bool,
}

#[derive(Default)]
pub(crate) struct Exports {
    cached: BTreeMap<AllocationId, Export>,
    // The cache's end of each ticket's socket pair, registered in the hangup monitor
    // under its descriptor number. It reports a hangup once the application has closed
    // every copy of the ticket.
    tickets: BTreeMap<AllocationId, Vec<OwnedFd>>,
}

pub struct ExportCache {
    exports: Mutex<Exports>,
    // Becomes readable when a ticket end reports a hangup, so the peer worker can reap
    // closed tickets without waiting for another CUDA call.
    hangups: OwnedFd,
}

// Ends beyond one batch stay ready, so the peer worker's next poll reaps them.
const REAP_BATCH: usize = 64;

impl ExportCache {
    pub fn new() -> Result<Self> {
        let hangups = epoll::create(epoll::CreateFlags::CLOEXEC)
            .map_err(|error| CoreError::io("create ticket hangup monitor", error))?;
        Ok(Self {
            exports: Mutex::default(),
            hangups,
        })
    }

    pub fn hangups(&self) -> Result<OwnedFd> {
        self.hangups
            .try_clone()
            .map_err(|error| CoreError::io("duplicate ticket hangup monitor", error))
    }

    /// Drop closed tickets and the released exports they kept alive.
    pub fn reap(&self) -> Result<()> {
        let mut exports = self.exports.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        // epoll reports a hangup or error on every registered end without being asked,
        // and either means the application closed the ticket.
        let mut ready = Vec::with_capacity(REAP_BATCH);
        match epoll::wait(
            &self.hangups,
            spare_capacity(&mut ready),
            Some(&Timespec::default()),
        ) {
            Ok(_) => {}
            Err(rustix::io::Errno::INTR) => return Ok(()),
            Err(error) => return Err(CoreError::io("wait for ticket hangups", error)),
        }
        let closed: BTreeSet<u64> = ready.iter().map(|event| event.data.u64()).collect();
        let Exports { cached, tickets } = &mut *exports;
        tickets.retain(|_, ends| {
            ends.retain(|end| {
                let open = !closed.contains(&(end.as_raw_fd() as u64));
                // A forked child can share the end's file description, which would
                // keep it registered after this process closes its descriptor.
                if !open {
                    let _ = epoll::delete(&self.hangups, end);
                }
                open
            });
            !ends.is_empty()
        });
        cached.retain(|id, export| !export.released || tickets.contains_key(id));
        Ok(())
    }

    /// The creator holds `reference` again, for example after importing its own ticket.
    /// Report whether the export is cached, and keep it past the tickets open now.
    pub fn reclaim(&self, reference: &AllocationReference) -> Result<bool> {
        let mut exports = self.exports.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        Ok(match exports.cached.get_mut(&reference.id) {
            Some(export) if export.reference == *reference => {
                export.released = false;
                true
            }
            _ => false,
        })
    }

    #[cfg(test)]
    fn contains(&self, reference: &AllocationReference) -> bool {
        self.exports
            .lock()
            .unwrap()
            .cached
            .get(&reference.id)
            .is_some_and(|export| export.reference == *reference)
    }

    /// Issue a shareable handle for `reference`. The application receives one end of a
    /// socket pair carrying the encoded reference, and the cache keeps the other end.
    pub fn issue(&self, reference: AllocationReference) -> Result<OwnedFd> {
        let pair = || -> protocol::Result<(OwnedFd, OwnedFd)> {
            let bytes = protocol::encode_virtual_shareable_handle(reference)?;
            let (ticket, end) = socketpair(
                AddressFamily::UNIX,
                SocketType::SEQPACKET,
                SocketFlags::CLOEXEC,
                None,
            )
            .map_err(std::io::Error::from)?;
            send(&end, &bytes, SendFlags::empty()).map_err(std::io::Error::from)?;
            Ok((ticket, end))
        };
        let (ticket, end) = pair().map_err(|error| {
            CoreError::io(
                "create shareable handle ticket",
                std::io::Error::other(error),
            )
        })?;
        epoll::add(
            &self.hangups,
            &end,
            epoll::EventData::new_u64(end.as_raw_fd() as u64),
            epoll::EventFlags::empty(),
        )
        .map_err(|error| CoreError::io("monitor shareable handle ticket", error))?;
        self.exports
            .lock()
            .map_err(|_| CUDA_ERROR_UNKNOWN)?
            .tickets
            .entry(reference.id)
            .or_default()
            .push(end);
        Ok(ticket)
    }

    /// The creator dropped its last local reference. An exported CUDA FD keeps its
    /// backing alive until every copy is closed, so keep the export while a ticket is
    /// open. Exports without tickets, such as legacy IPC handles, end here. A ticket
    /// that closed but is not reaped yet still counts, and the next reap drops it.
    pub fn release(&self, id: &AllocationId) -> Result<()> {
        let mut exports = self.exports.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        let Exports { cached, tickets } = &mut *exports;
        if !tickets.contains_key(id) {
            cached.remove(id);
        } else if let Some(export) = cached.get_mut(id) {
            export.released = true;
        }
        Ok(())
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
            ExportMetadata::Unicast { size, location } => {
                if size == 0 {
                    return Err(CUDA_ERROR_INVALID_VALUE.into());
                }
                Reply::UnicastExport {
                    size: size as u64,
                    location,
                }
            }
        };
        let mut exports = self.exports.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        if exports
            .cached
            .get(&reference.id)
            .is_some_and(|export| export.reference != reference)
        {
            return Err(CUDA_ERROR_INVALID_HANDLE.into());
        }
        exports.cached.insert(
            reference.id,
            Export {
                reference,
                descriptor,
                reply,
                released: false,
            },
        );
        Ok(())
    }

    pub fn remove(&self, id: &AllocationId) -> Result<()> {
        let mut exports = self.exports.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        exports.cached.remove(id);
        Ok(())
    }

    /// Drop every cached export before checkpoint. Ticket ends stay, so the checkpoint
    /// sees each ticket as a whole socket pair, and after restore `release()` still
    /// knows which tickets are open.
    pub fn clear(&self) -> Result<()> {
        let mut exports = self.exports.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        let dropped = exports
            .cached
            .values()
            .filter(|export| export.released)
            .count();
        exports.cached.clear();
        drop(exports);
        if dropped != 0 {
            // Only the creator re-exports after restore, so these tickets will fail to
            // import. Say why here, since the import error cannot.
            eprintln!(
                "cuinterpose: checkpoint dropped {dropped} export(s) kept only by open virtual shareable FDs; importing those FDs after restore fails"
            );
        }
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
        let (result, descriptor) = match exports.cached.get(&reference.id) {
            Some(export) if export.reference == *reference => {
                (Ok(export.reply.clone()), Some(&export.descriptor))
            }
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
        if reference.creator_pid == namespace_pid && !export_cache()?.reclaim(&reference)? {
            let fd = crate::driver::export_posix(self.driver_handle()?)?;
            let metadata = match self {
                Self::Unicast(allocation) => ExportMetadata::Unicast {
                    size: allocation.size,
                    location: location_metadata(allocation.properties.location),
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
                    if !matches!(metadata, ExportMetadata::Unicast { size, .. } if size == allocation.size)
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
    let (size, location) = match metadata {
        ExportMetadata::Unicast { size, location } => (size, location),
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
    let mut properties = unsafe { properties.assume_init() };
    // CUDA reports NUMA node 0 for every imported HOST_NUMA handle. Node IDs are
    // system-wide, unlike device ordinals, so keep the creator's node for reconstruction.
    if properties.location.type_ == CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA {
        properties.location.id = location.id;
    }
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
        peer_access_granted: false,
        // Imported backing already uses the host carrier.
        home_device: None,
        context,
    }));
    Ok((state, handle))
}
#[cfg(test)]
const DEVICE_LOCATION: protocol::MemoryLocation = protocol::MemoryLocation {
    location_type: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE as u32,
    id: 0,
};

#[cfg(test)]
mod codec_tests {
    use super::*;
    use rustix::fs::{MemfdFlags, memfd_create};
    use std::fs::File;
    use std::io::Write;
    use std::os::unix::fs::FileExt;

    fn handle_file(bytes: &[u8]) -> File {
        let fd = memfd_create(c"test-shareable-handle", MemfdFlags::CLOEXEC).unwrap();
        let mut file = File::from(fd);
        file.write_all(bytes).unwrap();
        file
    }

    fn pair() -> (OwnedFd, OwnedFd) {
        socketpair(
            AddressFamily::UNIX,
            SocketType::SEQPACKET,
            SocketFlags::CLOEXEC,
            None,
        )
        .unwrap()
    }

    /// A socket queued with one packet, shaped like a ticket.
    fn packet(bytes: &[u8]) -> OwnedFd {
        let (socket, end) = pair();
        send(&end, bytes, SendFlags::empty()).unwrap();
        socket
    }

    #[test]
    fn virtual_shareable_handle_round_trip() {
        let reference = AllocationReference {
            creator_pid: 1,
            id: [4; 16],
        };
        let ticket = ExportCache::new().unwrap().issue(reference).unwrap();
        // Decoding peeks, so a ticket imports more than once.
        for _ in 0..2 {
            assert_eq!(decode(ticket.as_raw_fd()).unwrap(), Some(reference));
        }
    }

    #[test]
    fn foreign_descriptors_are_untracked() {
        let foreign = File::open("/dev/null").unwrap();
        assert_eq!(decode(foreign.as_raw_fd()).unwrap(), None);
        let (socket, _peer) = UnixStream::pair().unwrap();
        assert_eq!(decode(socket.as_raw_fd()).unwrap(), None);
        let (empty, peer) = pair();
        assert_eq!(decode(empty.as_raw_fd()).unwrap(), None);
        // Once the peer closes, an empty socket reads end of file instead of EAGAIN.
        drop(peer);
        assert_eq!(decode(empty.as_raw_fd()).unwrap(), None);
        for bytes in [&b"foreign handle"[..], b"CU"] {
            assert_eq!(decode(packet(bytes).as_raw_fd()).unwrap(), None);
        }
        // A regular file is foreign even when it holds an encoded reference.
        let encoded = protocol::encode_virtual_shareable_handle(AllocationReference {
            creator_pid: 1,
            id: [4; 16],
        })
        .unwrap();
        assert_eq!(decode(handle_file(&encoded).as_raw_fd()).unwrap(), None);
    }

    #[test]
    fn truncated_and_oversized_virtual_handles_are_rejected() {
        let bytes = protocol::encode_virtual_shareable_handle(AllocationReference {
            creator_pid: 1,
            id: [4; 16],
        })
        .unwrap();
        let mut oversized = bytes.to_vec();
        oversized.push(0);
        // Three bytes end at the magic, so the version check rejects them first.
        let sizes = (4..bytes.len()).map(|size| &bytes[..size]);
        for ticket in sizes.chain([&oversized[..]]).map(packet) {
            assert!(matches!(
                decode(ticket.as_raw_fd()),
                Err(Error::Invalid("invalid virtual shareable handle size"))
            ));
        }
    }

    #[test]
    fn unsupported_virtual_handle_versions_are_rejected() {
        let mut bytes = protocol::encode_virtual_shareable_handle(AllocationReference {
            creator_pid: 1,
            id: [4; 16],
        })
        .unwrap();
        for version in [0, protocol::VERSION + 1] {
            bytes[3] = version;
            let ticket = packet(&bytes);
            assert!(matches!(decode(ticket.as_raw_fd()), Err(Error::Invalid(_))));
        }
    }

    #[test]
    fn export_cache_checks_identity_and_serves_full_size_from_another_pid() {
        let cache = ExportCache::new().unwrap();
        let reference = AllocationReference {
            creator_pid: 1,
            id: [4; 16],
        };
        cache
            .insert(
                reference,
                handle_file(b"backing").into(),
                ExportMetadata::Unicast {
                    size: 8192,
                    location: DEVICE_LOCATION,
                },
            )
            .unwrap();
        let other_creator = AllocationReference {
            creator_pid: 3,
            ..reference
        };
        assert!(cache.contains(&reference));
        assert!(!cache.contains(&other_creator));
        assert!(
            cache
                .insert(
                    other_creator,
                    handle_file(b"wrong backing").into(),
                    ExportMetadata::Unicast {
                        size: 4096,
                        location: DEVICE_LOCATION,
                    },
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
            Reply::UnicastExport { size: 8192, .. }
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
    fn export_replies_require_supported_metadata() {
        assert!(matches!(
            decode_export_metadata(Reply::UnicastExport {
                size: 8192,
                location: DEVICE_LOCATION,
            })
            .unwrap(),
            ExportMetadata::Unicast {
                size: 8192,
                location: DEVICE_LOCATION,
            }
        ));
        assert!(
            decode_export_metadata(Reply::UnicastExport {
                size: 0,
                location: DEVICE_LOCATION,
            })
            .is_err()
        );
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
    use rustix::event::{PollFd, PollFlags, poll};
    use std::fs::File;
    use std::io::{Read, Write};
    use std::sync::{Arc, mpsc};
    use std::time::{Duration, Instant};

    const REFERENCE: AllocationReference = AllocationReference {
        creator_pid: 7,
        id: [3; 16],
    };

    fn insert(cache: &ExportCache) {
        cache
            .insert(
                REFERENCE,
                File::open("/dev/zero").unwrap().into(),
                ExportMetadata::Unicast {
                    size: 8192,
                    location: DEVICE_LOCATION,
                },
            )
            .unwrap()
    }

    /// The peer worker reaps when the hangup monitor becomes readable.
    fn hangup(cache: &ExportCache) -> bool {
        let mut events = [PollFd::new(&cache.hangups, PollFlags::IN)];
        poll(&mut events, Some(&Timespec::default())).unwrap() == 1
    }

    /// Reap once the closed ticket hangs up, as the peer worker does. Another test's child
    /// process can hold every descriptor for a moment while it starts, which delays the
    /// hangup.
    fn reap_closed(cache: &ExportCache) {
        let deadline = Instant::now() + Duration::from_secs(5);
        while !hangup(cache) {
            assert!(Instant::now() < deadline, "no ticket hung up");
            std::thread::yield_now();
        }
        cache.reap().unwrap();
    }

    fn tickets(cache: &ExportCache) -> usize {
        cache
            .exports
            .lock()
            .unwrap()
            .tickets
            .values()
            .map(Vec::len)
            .sum()
    }

    #[test]
    fn open_tickets_keep_a_released_export() {
        let cache = ExportCache::new().unwrap();
        // Without a ticket, such as for a legacy IPC handle, release ends the export.
        insert(&cache);
        cache.release(&REFERENCE.id).unwrap();
        assert!(!cache.contains(&REFERENCE));

        // A creator may release before its peer imports the ticket.
        insert(&cache);
        let ticket = cache.issue(REFERENCE).unwrap();
        let copy = ticket.try_clone().unwrap();
        cache.release(&REFERENCE.id).unwrap();
        drop(ticket);
        assert!(!hangup(&cache));
        assert!(cache.contains(&REFERENCE));
        assert_eq!(decode(copy.as_raw_fd()).unwrap(), Some(REFERENCE));
        drop(copy);
        reap_closed(&cache);
        assert!(!hangup(&cache));
        assert!(!cache.contains(&REFERENCE));
        assert_eq!(tickets(&cache), 0);

        // Restoring the export makes it live again.
        insert(&cache);
        let ticket = cache.issue(REFERENCE).unwrap();
        cache.release(&REFERENCE.id).unwrap();
        insert(&cache);
        drop(ticket);
        reap_closed(&cache);
        assert!(cache.contains(&REFERENCE));
    }

    #[test]
    fn every_ticket_for_an_allocation_keeps_its_export() {
        let cache = ExportCache::new().unwrap();
        insert(&cache);
        let first = cache.issue(REFERENCE).unwrap();
        let second = cache.issue(REFERENCE).unwrap();
        cache.release(&REFERENCE.id).unwrap();
        drop(first);
        reap_closed(&cache);
        assert!(cache.contains(&REFERENCE));
        assert_eq!(tickets(&cache), 1);
        drop(second);
        reap_closed(&cache);
        assert!(!cache.contains(&REFERENCE));
        assert_eq!(tickets(&cache), 0);
    }

    #[test]
    fn release_leaves_a_closed_ticket_to_the_next_reap() {
        let cache = ExportCache::new().unwrap();
        insert(&cache);
        drop(cache.issue(REFERENCE).unwrap());
        cache.release(&REFERENCE.id).unwrap();
        assert!(cache.contains(&REFERENCE));
        reap_closed(&cache);
        assert!(!cache.contains(&REFERENCE));
    }

    #[test]
    fn a_creator_export_after_release_keeps_the_export() {
        let cache = ExportCache::new().unwrap();
        insert(&cache);
        let old = cache.issue(REFERENCE).unwrap();
        cache.release(&REFERENCE.id).unwrap();
        // The creator imports its own ticket and exports the allocation again.
        let other = AllocationReference {
            creator_pid: 8,
            ..REFERENCE
        };
        assert!(!cache.reclaim(&other).unwrap());
        assert!(cache.reclaim(&REFERENCE).unwrap());
        // The old ticket closing before the new one is issued must not end the export
        // the creator holds again.
        drop(old);
        reap_closed(&cache);
        assert!(cache.contains(&REFERENCE));
        let new = cache.issue(REFERENCE).unwrap();
        cache.release(&REFERENCE.id).unwrap();
        assert!(cache.contains(&REFERENCE));
        drop(new);
        reap_closed(&cache);
        assert!(!cache.contains(&REFERENCE));
    }

    #[test]
    fn checkpoint_drops_exports_held_only_by_tickets() {
        let cache = ExportCache::new().unwrap();
        insert(&cache);
        let ticket = cache.issue(REFERENCE).unwrap();
        cache.release(&REFERENCE.id).unwrap();
        cache.clear().unwrap();
        assert!(!cache.contains(&REFERENCE));
        assert_eq!(tickets(&cache), 1);
        // The ticket end outlives the checkpoint, so when an export is cached again, its
        // release still sees the open ticket.
        insert(&cache);
        cache.release(&REFERENCE.id).unwrap();
        assert!(cache.contains(&REFERENCE));
        drop(ticket);
        reap_closed(&cache);
        assert!(!cache.contains(&REFERENCE));
    }

    #[test]
    fn reap_unregisters_an_end_a_child_process_shares() {
        let cache = ExportCache::new().unwrap();
        insert(&cache);
        let ticket = cache.issue(REFERENCE).unwrap();
        // The child keeps only a copy of the cache's end, as its stdin. Exec closes the
        // other descriptors it inherits, including other tests' tickets.
        let end = cache.exports.lock().unwrap().tickets[&REFERENCE.id][0]
            .try_clone()
            .unwrap();
        struct Child(std::process::Child);
        impl Drop for Child {
            fn drop(&mut self) {
                let _ = self.0.kill();
                let _ = self.0.wait();
            }
        }
        let _child = Child(
            std::process::Command::new("sleep")
                .arg("30")
                .stdin(end)
                .spawn()
                .unwrap(),
        );
        drop(ticket);
        reap_closed(&cache);
        assert_eq!(tickets(&cache), 0);
        // The child's copy keeps the end's file description, and with it a registration
        // that reap did not remove, which would wake the peer worker forever.
        assert!(!hangup(&cache));
    }

    #[test]
    fn multicast_exports_send_descriptor_and_properties() {
        let cache = ExportCache::new().unwrap();
        let reference = AllocationReference {
            creator_pid: 7,
            id: [1; 16],
        };
        let (socket, peer) = UnixStream::pair().unwrap();
        let metadata = ExportMetadata::Multicast(CUmulticastObjectProp {
            numDevices: 2,
            size: 4096,
            handleTypes: 1,
            flags: 0,
        });
        cache
            .insert(reference, File::open("/dev/zero").unwrap().into(), metadata)
            .unwrap();
        cache.send(&socket, 7, &reference).unwrap();
        let (reply, fd): (Response, _) = protocol::receive(&peer).unwrap();
        assert_eq!(reply.namespace_pid, 7);
        assert!(matches!(
            reply.result.unwrap(),
            Reply::MulticastExport {
                properties: protocol::MulticastProperties {
                    devices: 2,
                    size: 4096,
                    handle_types: 1,
                    flags: 0,
                },
            }
        ));
        let mut byte = [1];
        File::from(fd.unwrap()).read_exact(&mut byte).unwrap();
        assert_eq!(byte, [0]);
    }

    #[test]
    fn teardown_and_replacement_wait_for_socket_send() {
        for replace in [false, true] {
            let cache = Arc::new(ExportCache::new().unwrap());
            let reference = AllocationReference {
                creator_pid: 7,
                id: [2; 16],
            };
            cache
                .insert(
                    reference,
                    File::open("/dev/zero").unwrap().into(),
                    ExportMetadata::Unicast {
                        size: 8192,
                        location: DEVICE_LOCATION,
                    },
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
                            ExportMetadata::Unicast {
                                size: 8192,
                                location: DEVICE_LOCATION,
                            },
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
                Ok(Reply::UnicastExport { size: 8192, .. })
            ));
            let mut byte = [1];
            File::from(fd.unwrap()).read_exact(&mut byte).unwrap();
            assert_eq!(byte, [0]);
            sender.join().unwrap();
            completion.recv_timeout(Duration::from_secs(5)).unwrap();
            mutation.join().unwrap();
            assert_eq!(cache.contains(&reference), replace);
        }
    }
}
