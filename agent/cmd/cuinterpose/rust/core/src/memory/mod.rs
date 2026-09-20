// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Memblock identity, virtual-handle ownership, and tracked address ranges.

pub(crate) mod checkpoint;
mod host_carrier;
pub(crate) mod ipc;
pub(crate) mod multicast;
pub(crate) mod sharing;
pub(crate) mod vmm;

use crate::driver::{CudaError, Result};
use crate::runtime;
use checkpoint::Phase;
use cudarc::driver::sys::CUresult::*;
use cuinterpose_protocol::{AllocationId, AllocationReference, NamespacePid};
use runtime::export_cache;
use std::collections::BTreeMap;
use vmm::{Allocation, Mapping};

/// Application-visible allocation handle. High bits distinguish it from CUDA's.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Ord, PartialOrd)]
pub struct VirtualAllocationHandle(u64);

impl VirtualAllocationHandle {
    pub const TAG: u64 = 0xd94d_0000_0000_0000;
    pub const MASK: u64 = 0xffff_0000_0000_0000;

    pub(crate) fn from_raw(handle: u64) -> Option<Self> {
        (handle & Self::MASK == Self::TAG).then_some(Self(handle))
    }

    pub(crate) fn as_raw(self) -> u64 {
        self.0
    }

    /// CUDA-minted handle. Tagged values collide with the virtual namespace.
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
            .copied()
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

/// CUDA memblock: physical allocation behind a generic handle.
/// Unicast and multicast share one ID and virtual-handle namespace;
/// they keep different CUDA properties and reconstruction state in the variants.
#[derive(Clone)]
pub enum Memblock {
    Unicast(Allocation),
    Multicast(multicast::MulticastObject),
}

impl Memblock {
    pub fn unicast(&self) -> Option<&Allocation> {
        match self {
            Self::Unicast(allocation) => Some(allocation),
            _ => None,
        }
    }

    pub fn unicast_mut(&mut self) -> Option<&mut Allocation> {
        match self {
            Self::Unicast(allocation) => Some(allocation),
            _ => None,
        }
    }

    pub fn multicast(&self) -> Option<&multicast::MulticastObject> {
        match self {
            Self::Multicast(object) => Some(object),
            _ => None,
        }
    }

    pub fn multicast_mut(&mut self) -> Option<&mut multicast::MulticastObject> {
        match self {
            Self::Multicast(object) => Some(object),
            _ => None,
        }
    }

    pub(crate) fn reference(&self) -> AllocationReference {
        match self {
            Self::Unicast(allocation) => allocation.reference,
            Self::Multicast(object) => object.reference,
        }
    }

    /// CUDA generic allocation handle currently retained for this memblock.
    pub(crate) fn driver_handle(&self) -> Result<u64> {
        match self {
            Self::Unicast(allocation) => allocation.driver,
            Self::Multicast(object) => object.driver,
        }
        .ok_or(CUDA_ERROR_INVALID_HANDLE.into())
    }
}

pub struct ProcessState {
    pub namespace_pid: NamespacePid,
    pub malloc_regions: BTreeMap<u64, ipc::MallocRegion>,
    pub memblocks: BTreeMap<AllocationId, Memblock>,
    pub virtual_allocation_handles: BTreeMap<VirtualAllocationHandle, AllocationId>,
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
            memblocks: BTreeMap::new(),
            virtual_allocation_handles: BTreeMap::new(),
            mappings: BTreeMap::new(),
            next_virtual_allocation_handle: 1,
            phase: Phase::Active,
            arena: None,
            unlocked_driver_calls: 0,
        }
    }

    /// Check metadata capacity before acquiring CUDA backing.
    pub(crate) fn new_reference(&self) -> Result<AllocationReference> {
        if self.phase != Phase::Active {
            return Err(CUDA_ERROR_NOT_READY.into());
        }
        self.check_handle_capacity()?;
        Ok(AllocationReference {
            creator_pid: self.namespace_pid,
            id: random()?,
        })
    }

    pub(crate) fn check_handle_capacity(&self) -> Result<()> {
        if self.next_virtual_allocation_handle & VirtualAllocationHandle::MASK != 0 {
            return Err(CUDA_ERROR_OUT_OF_MEMORY.into());
        }
        Ok(())
    }

    pub(crate) fn mapped_memblock(&self, address: u64) -> Option<AllocationId> {
        self.mappings
            .values()
            .find(|m| address >= m.address && address - m.address < m.size as u64)
            .map(|m| m.id)
    }

    pub(crate) fn mint_virtual_allocation_handle(&mut self, id: AllocationId) -> Result<u64> {
        if self.next_virtual_allocation_handle & VirtualAllocationHandle::MASK != 0 {
            return Err(CudaError::from(CUDA_ERROR_OUT_OF_MEMORY));
        }
        let handle = VirtualAllocationHandle(
            VirtualAllocationHandle::TAG | self.next_virtual_allocation_handle,
        );
        self.next_virtual_allocation_handle += 1;
        self.virtual_allocation_handles.insert(handle, id);
        Ok(handle.as_raw())
    }

    /// Resolve a virtual handle to its allocation ID; native handles return `None`.
    /// A tagged handle absent from the registry is stale and returns `INVALID_HANDLE`.
    pub(crate) fn resolve_virtual_handle(&self, handle: u64) -> Result<Option<AllocationId>> {
        VirtualAllocationHandle::from_raw(handle)
            .map(|handle| handle.id(self))
            .transpose()
    }

    /// Release an unicast driver handle after its last virtual handle is dropped.
    /// Remove the memblock once neither virtual handles nor mappings refer to it.
    pub(crate) fn release_unused_memblock(&mut self, id: AllocationId) -> Result<()> {
        let handle_live = self
            .virtual_allocation_handles
            .values()
            .any(|value| *value == id);
        let mapped = self.mappings.values().any(|mapping| mapping.id == id);
        let memblock = self
            .memblocks
            .get_mut(&id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
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
            Memblock::Multicast(object) => {
                if handle_live || mapped {
                    return Ok(());
                }
                cache()?.remove(&id)?;
                if let Some(driver) = object.driver {
                    unsafe { crate::driver::cuMemRelease(driver) }?;
                }
            }
        }
        self.memblocks.remove(&id);
        Ok(())
    }
}

pub(crate) fn random<const N: usize>() -> Result<[u8; N]> {
    let mut bytes = [0; N];
    getrandom::fill(&mut bytes).map_err(|_| CUDA_ERROR_NOT_INITIALIZED)?;
    Ok(bytes)
}
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn identities_require_an_active_registry_and_handle_capacity() {
        let mut state = ProcessState::new(41);
        assert_eq!(state.new_reference().unwrap().creator_pid, 41);
        state.phase = Phase::UnicastPrepared;
        assert_eq!(state.new_reference(), Err(CUDA_ERROR_NOT_READY.into()));
        state.phase = Phase::Active;
        state.next_virtual_allocation_handle = VirtualAllocationHandle::MASK;
        assert_eq!(state.new_reference(), Err(CUDA_ERROR_OUT_OF_MEMORY.into()));
        assert!(state.virtual_allocation_handles.is_empty());
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
        assert_eq!(
            state.resolve_virtual_handle(VirtualAllocationHandle::TAG | 99),
            Err(CUDA_ERROR_INVALID_HANDLE.into())
        );
        assert_eq!(
            VirtualAllocationHandle::from_driver(0x1000).unwrap(),
            0x1000
        );
        assert_eq!(
            VirtualAllocationHandle::from_driver(VirtualAllocationHandle::TAG),
            Err(CUDA_ERROR_INVALID_HANDLE.into())
        );
    }
}
