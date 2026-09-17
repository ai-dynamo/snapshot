// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The listener never takes the CUDA state lock. A lease pins a duplicated
//! descriptor through sendmsg; teardown stops admission and drains those leases
//! before dropping the driver's cached exports.

use crate::state::Result;
use cudarc::driver::sys::CUresult::{CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_UNKNOWN};
use cuinterpose_protocol::{AllocationId, ResourceKind};
use std::collections::BTreeMap;
use std::os::fd::OwnedFd;
use std::sync::{Condvar, Mutex, MutexGuard};

/// Resource kind is part of the lookup key, not an authorization token.
pub type Key = (ResourceKind, AllocationId);

#[derive(Default)]
pub(super) struct Entries {
    descriptors: BTreeMap<Key, Entry>,
    transfers: usize,
    draining: bool,
}

struct Entry {
    descriptor: OwnedFd,
    transfers: usize,
    retiring: bool,
}

#[derive(Default)]
pub struct ExportCache {
    entries: Mutex<Entries>,
    // Mutations may wait with the entries lock released. Serialize them without
    // blocking peer admission for other IDs or letting a second mutation replace
    // the entry whose leases the first mutation is waiting to drain.
    mutations: Mutex<()>,
    drained: Condvar,
}

pub struct Lease<'a> {
    cache: &'a ExportCache,
    id: Key,
    descriptor: Option<OwnedFd>,
}

impl ExportCache {
    pub fn fork_lock(&self, descriptors: &mut Vec<i32>) -> MutexGuard<'_, Entries> {
        use std::os::fd::AsRawFd;
        let mut entries = self.entries.lock().unwrap_or_else(|e| e.into_inner());
        while entries.transfers != 0 {
            entries = self
                .drained
                .wait(entries)
                .unwrap_or_else(|e| e.into_inner());
        }
        descriptors.extend(
            entries
                .descriptors
                .values()
                .map(|entry| entry.descriptor.as_raw_fd()),
        );
        entries
    }
    pub fn contains(&self, id: &Key) -> Result<bool> {
        let entries = self.entries.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        Ok(entries.descriptors.contains_key(id))
    }

    #[cfg(test)]
    pub fn len(&self) -> Result<usize> {
        let entries = self.entries.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        Ok(entries.descriptors.len())
    }

    pub fn acquire(&self, id: &Key) -> Result<Lease<'_>> {
        let mut entries = self.entries.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        if entries.draining {
            return Err(CUDA_ERROR_INVALID_HANDLE.into());
        }
        let total = entries.transfers.checked_add(1).ok_or(CUDA_ERROR_UNKNOWN)?;
        let entry = entries
            .descriptors
            .get_mut(id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        if entry.retiring {
            return Err(CUDA_ERROR_INVALID_HANDLE.into());
        }
        let descriptor = entry
            .descriptor
            .try_clone()
            .map_err(|_| CUDA_ERROR_UNKNOWN)?;
        entry.transfers = entry.transfers.checked_add(1).ok_or(CUDA_ERROR_UNKNOWN)?;
        entries.transfers = total;
        Ok(Lease {
            cache: self,
            id: *id,
            descriptor: Some(descriptor),
        })
    }

    /// Retire only this ID. A new insert or missing removal does not affect
    /// admission or itself drain unrelated transfers. Mutations are serialized,
    /// so replacing B can still wait behind an already-running retirement of A.
    pub fn replace(&self, id: Key, descriptor: Option<OwnedFd>) -> Result<()> {
        let _mutation = self.mutations.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        let mut entries = self.entries.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        if let Some(entry) = entries.descriptors.get_mut(&id) {
            entry.retiring = true;
            entries = self
                .drained
                .wait_while(entries, |entries| entries.descriptors[&id].transfers != 0)
                .map_err(|_| CUDA_ERROR_UNKNOWN)?;
        }
        entries.descriptors.remove(&id);
        if let Some(descriptor) = descriptor {
            entries.descriptors.insert(
                id,
                Entry {
                    descriptor,
                    transfers: 0,
                    retiring: false,
                },
            );
        }
        Ok(())
    }

    pub fn clear(&self) -> Result<()> {
        let _mutation = self.mutations.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        let mut entries = self.entries.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
        entries.draining = true;
        let mut entries = self
            .drained
            .wait_while(entries, |entries| entries.transfers != 0)
            .map_err(|_| CUDA_ERROR_UNKNOWN)?;
        entries.descriptors.clear();
        entries.draining = false;
        Ok(())
    }
}

impl Lease<'_> {
    pub fn descriptor(&self) -> &OwnedFd {
        // Only Drop removes the descriptor, after this borrow has ended.
        self.descriptor.as_ref().expect("live export lease")
    }
}

impl Drop for Lease<'_> {
    fn drop(&mut self) {
        // Close before waking teardown: otherwise an export can remain live
        // after the last CUDA driver reference has been released.
        drop(self.descriptor.take());
        let mut entries = self.cache.entries.lock().unwrap_or_else(|e| e.into_inner());
        entries.transfers -= 1;
        let entry = entries
            .descriptors
            .get_mut(&self.id)
            .expect("leased entry stays until drained");
        entry.transfers -= 1;
        self.cache.drained.notify_all();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::File;
    use std::sync::{Arc, mpsc};
    use std::time::Duration;

    #[test]
    fn resource_kind_is_part_of_descriptor_identity() {
        let cache = ExportCache::default();
        let id = AllocationId([8; 16]);
        cache
            .replace(
                (ResourceKind::Multicast, id),
                Some(File::open("/dev/null").unwrap().into()),
            )
            .unwrap();
        assert!(cache.acquire(&(ResourceKind::Unicast, id)).is_err());
        assert!(cache.acquire(&(ResourceKind::Multicast, id)).is_ok());
        cache.replace((ResourceKind::Unicast, id), None).unwrap();
        assert!(cache.acquire(&(ResourceKind::Multicast, id)).is_ok());
    }

    #[test]
    fn teardown_drains_transfers_and_rejects_new_requests() {
        let cache = Arc::new(ExportCache::default());
        let id = (ResourceKind::Unicast, AllocationId([1; 16]));
        cache
            .replace(id, Some(File::open("/dev/null").unwrap().into()))
            .unwrap();
        let lease = cache.acquire(&id).unwrap();
        let (done, completion) = mpsc::channel();
        let copy = Arc::clone(&cache);
        let worker = std::thread::spawn(move || {
            copy.clear().unwrap();
            done.send(()).unwrap();
        });
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while !cache.entries.lock().unwrap().draining {
            assert!(std::time::Instant::now() < deadline);
            std::thread::yield_now();
        }
        assert!(cache.acquire(&id).is_err());
        assert!(completion.try_recv().is_err());
        drop(lease);
        completion.recv_timeout(Duration::from_secs(5)).unwrap();
        worker.join().unwrap();
        assert_eq!(cache.len().unwrap(), 0);
        assert!(cache.acquire(&id).is_err());
        cache
            .replace(id, Some(File::open("/dev/zero").unwrap().into()))
            .unwrap();
        assert!(cache.acquire(&id).is_ok());
    }

    #[test]
    fn replacement_waits_until_the_old_descriptor_is_sent() {
        use std::io::Read;
        let cache = Arc::new(ExportCache::default());
        let id = (ResourceKind::Multicast, AllocationId([2; 16]));
        let unrelated = (ResourceKind::Unicast, AllocationId([4; 16]));
        cache
            .replace(unrelated, Some(File::open("/dev/null").unwrap().into()))
            .unwrap();
        let unrelated_lease = cache.acquire(&unrelated).unwrap();
        cache
            .replace(id, Some(File::open("/dev/null").unwrap().into()))
            .unwrap();
        let lease = cache.acquire(&id).unwrap();
        let copy = Arc::clone(&cache);
        let worker = std::thread::spawn(move || {
            copy.replace(id, Some(File::open("/dev/zero").unwrap().into()))
                .unwrap();
        });
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while !cache.entries.lock().unwrap().descriptors[&id].retiring {
            assert!(std::time::Instant::now() < deadline);
            std::thread::yield_now();
        }
        assert!(cache.acquire(&id).is_err());
        let second_unrelated = cache.acquire(&unrelated);
        let mut original = File::from(lease.descriptor().try_clone().unwrap());
        assert_eq!(original.read(&mut [0; 1]).unwrap(), 0);
        drop(original);
        drop(lease);
        drop(unrelated_lease);
        worker.join().unwrap();
        assert!(second_unrelated.is_ok(), "retiring B rejected unrelated A");
        let fresh = cache.acquire(&id).unwrap();
        let mut fresh = File::from(fresh.descriptor().try_clone().unwrap());
        let mut byte = [1];
        assert_eq!(fresh.read(&mut byte).unwrap(), 1);
        assert_eq!(byte, [0]);
    }

    #[test]
    fn unrelated_mutations_do_not_drain_or_reject_active_exports() {
        let cache = ExportCache::default();
        let a = (ResourceKind::Unicast, AllocationId([1; 16]));
        let b = (ResourceKind::Multicast, AllocationId([2; 16]));
        cache
            .replace(a, Some(File::open("/dev/null").unwrap().into()))
            .unwrap();
        let first = cache.acquire(&a).unwrap();
        std::thread::scope(|scope| {
            let (done, completion) = mpsc::channel();
            let shared = &cache;
            let worker = scope.spawn(move || {
                shared
                    .replace(b, Some(File::open("/dev/zero").unwrap().into()))
                    .unwrap();
                shared.replace(b, None).unwrap();
                shared.replace(b, None).unwrap(); // Missing removal is a no-op.
                done.send(()).unwrap();
            });
            let finished = completion.recv_timeout(Duration::from_secs(5));
            let second = cache.acquire(&a);
            // Drop even on regression so a blocked mutation can finish before
            // the test reports failure rather than stranding its scoped thread.
            drop(first);
            worker.join().unwrap();
            assert!(finished.is_ok(), "unrelated mutation waited for A");
            assert!(second.is_ok(), "unrelated mutation rejected A");
        });
    }

    #[test]
    fn failed_socket_send_releases_lease_and_allows_clear() {
        use cuinterpose_protocol::{Request, send};
        use std::os::unix::net::UnixStream;
        let cache = Arc::new(ExportCache::default());
        let id = (ResourceKind::Unicast, AllocationId([3; 16]));
        cache
            .replace(id, Some(File::open("/dev/null").unwrap().into()))
            .unwrap();
        let lease = cache.acquire(&id).unwrap();
        let (socket, peer) = UnixStream::pair().unwrap();
        drop(peer);
        assert!(send(&socket, &Request::Handshake, Some(lease.descriptor())).is_err());
        let (done, completion) = mpsc::channel();
        let copy = Arc::clone(&cache);
        let worker = std::thread::spawn(move || {
            copy.clear().unwrap();
            done.send(()).unwrap();
        });
        drop(lease);
        completion.recv_timeout(Duration::from_secs(5)).unwrap();
        worker.join().unwrap();
        assert_eq!(cache.len().unwrap(), 0);
    }
}
