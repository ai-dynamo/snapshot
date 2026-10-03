// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Allocation identity, virtual handle references, and tracked address ranges.

pub(crate) mod checkpoint;
pub(crate) mod context;
mod host_carrier;
pub(crate) mod ipc;
pub(crate) mod sharing;
pub(crate) mod vmm;

use crate::error::{Error, Result};
use crate::runtime;
use checkpoint::Phase;
use cudarc::driver::sys::CUresult::*;
use cuinterpose_protocol::{AllocationId, AllocationReference, NamespacePid};
use runtime::export_cache;
use std::collections::BTreeMap;
use vmm::{Allocation, Mapping};

/// Allocation handle returned to the application with a private tag.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Ord, PartialOrd)]
pub struct VirtualAllocationHandle(u64);

impl VirtualAllocationHandle {
    // CUDA does not reserve this prefix, so callers must terminate if from_driver
    // detects a collision rather than publish an ambiguous handle.
    pub const TAG: u64 = 0xd94d_0000_0000_0000;
    pub const MASK: u64 = 0xffff_0000_0000_0000;

    pub(crate) fn from_raw(handle: u64) -> Option<Self> {
        (handle & Self::MASK == Self::TAG).then_some(Self(handle))
    }

    pub(crate) fn as_raw(self) -> u64 {
        self.0
    }

    /// Handle returned by CUDA. A tagged value would conflict with virtual handles.
    pub(crate) fn from_driver(handle: u64) -> Result<u64> {
        match Self::from_raw(handle) {
            Some(_) => Err(CUDA_ERROR_INVALID_HANDLE.into()),
            None => Ok(handle),
        }
    }

    pub(crate) fn id(self, state: &ProcessState) -> Result<AllocationId> {
        state
            .virtual_allocation_handles
            .get(&self)
            .map(|entry| entry.id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE.into())
    }

    /// CUDA generic allocation handle currently retained for this virtual handle.
    pub(crate) fn driver_handle(self, state: &ProcessState) -> Result<u64> {
        state
            .memblocks
            .get(&self.id(state)?)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?
            .driver_handle()
    }
}

/// Logical references that keep an allocation registered.
#[derive(Clone, Default)]
pub struct Refcounts {
    /// Distinct live virtual handles, excluding repeated retains of the same handle.
    pub handle_entries: usize,
    pub mappings: usize,
}

/// Physical allocation identified by a generic CUDA handle. Unicast and multicast share
/// ID and virtual handle namespaces while keeping their CUDA properties and restore
/// state in separate variants.
#[derive(Clone)]
pub enum Memblock {
    Unicast(Allocation),
}

impl Memblock {
    pub(crate) fn refcounts(&self) -> &Refcounts {
        match self {
            Self::Unicast(allocation) => &allocation.refcounts,
        }
    }

    pub(crate) fn refcounts_mut(&mut self) -> &mut Refcounts {
        match self {
            Self::Unicast(allocation) => &mut allocation.refcounts,
        }
    }

    pub fn unicast(&self) -> Option<&Allocation> {
        match self {
            Self::Unicast(allocation) => Some(allocation),
        }
    }

    pub fn unicast_mut(&mut self) -> Option<&mut Allocation> {
        match self {
            Self::Unicast(allocation) => Some(allocation),
            _ => None,
        }
    }

    pub(crate) fn reference(&self) -> AllocationReference {
        match self {
            Self::Unicast(allocation) => allocation.reference,
        }
    }

    /// CUDA generic allocation handle currently retained for this memblock.
    pub(crate) fn driver_handle(&self) -> Result<u64> {
        match self {
            Self::Unicast(allocation) => allocation.driver,
        }
        .ok_or(CUDA_ERROR_INVALID_HANDLE.into())
    }
}

pub struct HandleEntry {
    pub id: AllocationId,
    pub references: u64,
}

pub struct ProcessState {
    pub namespace_pid: NamespacePid,
    pub malloc_regions: BTreeMap<u64, ipc::MallocRegion>,
    imported_mallocs: BTreeMap<AllocationId, u64>,
    // Minimum granularity and GPUDirect RDMA flag for the fixed malloc properties.
    malloc_layouts: BTreeMap<i32, (usize, u8)>,
    pub memblocks: BTreeMap<AllocationId, Memblock>,
    pub virtual_allocation_handles: BTreeMap<VirtualAllocationHandle, HandleEntry>,
    pub mappings: BTreeMap<u64, Mapping>,
    pub phase: Phase,
    pub arena: Option<host_carrier::Arena>,
    pub unlocked_driver_calls: usize,
    next_virtual_allocation_handle: u64,
}

impl ProcessState {
    pub(crate) fn new(namespace_pid: NamespacePid) -> Self {
        Self {
            namespace_pid,
            malloc_regions: BTreeMap::new(),
            imported_mallocs: BTreeMap::new(),
            malloc_layouts: BTreeMap::new(),
            memblocks: BTreeMap::new(),
            virtual_allocation_handles: BTreeMap::new(),
            mappings: BTreeMap::new(),
            next_virtual_allocation_handle: 1,
            phase: Phase::Active,
            arena: None,
            unlocked_driver_calls: 0,
        }
    }

    /// Assign the identity used by the shim and its sharing peers.
    pub(crate) fn new_reference(&self) -> Result<AllocationReference> {
        if self.phase != Phase::Active {
            return Err(CUDA_ERROR_NOT_READY.into());
        }
        Ok(AllocationReference {
            creator_pid: self.namespace_pid,
            id: random()?,
        })
    }

    pub(crate) fn mint_virtual_allocation_handle(&mut self, id: AllocationId) -> Result<u64> {
        if self.next_virtual_allocation_handle & VirtualAllocationHandle::MASK != 0 {
            return Err(Error::from(CUDA_ERROR_OUT_OF_MEMORY));
        }
        let handle = VirtualAllocationHandle(
            VirtualAllocationHandle::TAG | self.next_virtual_allocation_handle,
        );
        self.next_virtual_allocation_handle += 1;
        self.virtual_allocation_handles
            .insert(handle, HandleEntry { id, references: 1 });
        Ok(handle.as_raw())
    }

    /// Remove one application reference without changing the handle value retained by
    /// existing mappings.
    pub(crate) fn release_virtual_handle(&mut self, handle: VirtualAllocationHandle) -> Result<()> {
        let entry = self
            .virtual_allocation_handles
            .get_mut(&handle)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        let id = entry.id;
        entry.references -= 1;
        if entry.references == 0 {
            self.virtual_allocation_handles.remove(&handle);
            self.memblocks
                .get_mut(&id)
                .unwrap()
                .refcounts_mut()
                .handle_entries -= 1;
            runtime::must_complete(self.release_unused_memblock(id));
        }
        Ok(())
    }

    /// Resolve a virtual handle to its allocation ID, returning `None` for native
    /// handles and `INVALID_HANDLE` for stale tagged handles missing from the registry.
    pub(crate) fn resolve_virtual_handle(&self, handle: u64) -> Result<Option<AllocationId>> {
        VirtualAllocationHandle::from_raw(handle)
            .map(|handle| handle.id(self))
            .transpose()
    }

    pub(crate) fn mappings_in_range(
        &self,
        address: u64,
        size: usize,
    ) -> impl Iterator<Item = &Mapping> {
        self.mappings
            .range(address..)
            .take_while(move |(start, _)| **start - address < size as u64)
            .map(|(_, mapping)| mapping)
    }

    /// Remove a mapping after CUDA unmaps it, before checking whether its backing can
    /// be released.
    pub(crate) fn remove_mapping(&mut self, address: u64) -> AllocationId {
        let mapping = self.mappings.remove(&address).unwrap();
        self.memblocks
            .get_mut(&mapping.id)
            .unwrap()
            .refcounts_mut()
            .mappings -= 1;
        mapping.id
    }

    /// Release a unicast driver handle after the last virtual handle is removed. Remove
    /// the memblock when no virtual handles or mappings refer to it.
    pub(crate) fn release_unused_memblock(&mut self, id: AllocationId) -> Result<()> {
        let memblock = self
            .memblocks
            .get_mut(&id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        let handle_live = memblock.refcounts().handle_entries != 0;
        let mapped = memblock.refcounts().mappings != 0;
        match memblock {
            Memblock::Unicast(allocation) => {
                // Unicast mappings retain the backing after its last handle is released.
                if !handle_live && let Some(driver) = allocation.driver {
                    unsafe { crate::driver::cuMemRelease(driver) }?;
                    allocation.driver = None;
                }
                if handle_live || mapped {
                    return Ok(());
                }
                export_cache()?.remove(&id)?;
            }
        }
        self.memblocks.remove(&id);
        Ok(())
    }
}

pub(crate) fn random<const N: usize>() -> Result<[u8; N]> {
    let mut bytes = [0; N];
    getrandom::fill(&mut bytes)
        .map_err(|error| Error::io("generate allocation identity", std::io::Error::other(error)))?;
    Ok(bytes)
}
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn identities_require_an_active_registry() {
        let mut state = ProcessState::new(41);
        assert_eq!(state.new_reference().unwrap().creator_pid, 41);
        state.phase = Phase::UnicastPrepared;
        assert!(matches!(
            state.new_reference(),
            Err(Error::Cuda(CUDA_ERROR_NOT_READY))
        ));
    }

    #[test]
    fn tagged_handles_resolve_or_reject_stale() {
        let mut state = ProcessState::new(41);
        let id = [7; 16];
        let handle = state.mint_virtual_allocation_handle(id).unwrap();
        assert_eq!(state.resolve_virtual_handle(handle).unwrap(), Some(id));
        assert_eq!(
            VirtualAllocationHandle::from_raw(handle)
                .unwrap()
                .id(&state)
                .unwrap(),
            id
        );
        assert_eq!(state.resolve_virtual_handle(0x1000).unwrap(), None);
        assert!(matches!(
            state.resolve_virtual_handle(VirtualAllocationHandle::TAG | 99),
            Err(Error::Cuda(CUDA_ERROR_INVALID_HANDLE))
        ));
        assert_eq!(
            VirtualAllocationHandle::from_driver(0x1000).unwrap(),
            0x1000
        );
        assert!(matches!(
            VirtualAllocationHandle::from_driver(VirtualAllocationHandle::TAG),
            Err(Error::Cuda(CUDA_ERROR_INVALID_HANDLE))
        ));
    }
}
