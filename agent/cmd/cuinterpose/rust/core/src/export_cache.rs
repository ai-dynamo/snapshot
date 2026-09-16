// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The listener never takes the CUDA state lock. A lease pins a duplicated
//! descriptor through sendmsg; teardown stops admission and drains those leases
//! before dropping the driver's cached exports.

use crate::state::Result;
use cuinterpose_abi::{INVALID_HANDLE, UNKNOWN};
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
        let entries = self.entries.lock().map_err(|_| UNKNOWN)?;
        Ok(entries.descriptors.contains_key(id))
    }

    pub fn len(&self) -> Result<usize> {
        let entries = self.entries.lock().map_err(|_| UNKNOWN)?;
        Ok(entries.descriptors.len())
    }

    pub fn acquire(&self, id: &Key) -> Result<Lease<'_>> {
        let mut entries = self.entries.lock().map_err(|_| UNKNOWN)?;
        if entries.draining {
            return Err(INVALID_HANDLE.into());
        }
        let total = entries.transfers.checked_add(1).ok_or(UNKNOWN)?;
        let entry = entries.descriptors.get_mut(id).ok_or(INVALID_HANDLE)?;
        if entry.retiring {
            return Err(INVALID_HANDLE.into());
        }
        let descriptor = entry.descriptor.try_clone().map_err(|_| UNKNOWN)?;
        entry.transfers = entry.transfers.checked_add(1).ok_or(UNKNOWN)?;
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
        let _mutation = self.mutations.lock().map_err(|_| UNKNOWN)?;
        let mut entries = self.entries.lock().map_err(|_| UNKNOWN)?;
        if let Some(entry) = entries.descriptors.get_mut(&id) {
            entry.retiring = true;
            while entries.descriptors[&id].transfers != 0 {
                entries = self.drained.wait(entries).map_err(|_| UNKNOWN)?;
            }
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
        let _mutation = self.mutations.lock().map_err(|_| UNKNOWN)?;
        let mut entries = self.entries.lock().map_err(|_| UNKNOWN)?;
        entries.draining = true;
        while entries.transfers != 0 {
            entries = self.drained.wait(entries).map_err(|_| UNKNOWN)?;
        }
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
