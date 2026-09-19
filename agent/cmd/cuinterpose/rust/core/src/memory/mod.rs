// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Resource identity, virtual-handle ownership, and tracked address ranges.

pub(crate) mod checkpoint;
mod host_carrier;
pub(crate) mod ipc;
pub(crate) mod sharing;
pub(crate) mod vmm;

use crate::driver::{CudaError, Result};
use crate::runtime;
use checkpoint::Phase;
use cudarc::driver::sys::CUresult::*;
use cuinterpose_protocol::{AllocationId, AllocationReference, NamespacePid};
use runtime::cache;
use std::collections::BTreeMap;
use vmm::{Allocation, Mapping};

// Distinguish cuinterpose virtual allocation handles from handles returned by CUDA.

pub const VIRTUAL_ALLOCATION_HANDLE_TAG: u64 = 0xd94d_0000_0000_0000;
pub const VIRTUAL_ALLOCATION_HANDLE_MASK: u64 = 0xffff_0000_0000_0000;
/// One ID and virtual-handle namespace covers unicast and multicast resources.
/// Keep their different CUDA properties and reconstruction state in the variants.
#[derive(Clone)]
pub enum Resource {
    Unicast(Allocation),
}

impl Resource {
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

    pub(crate) fn driver(&self) -> Result<u64> {
        match self {
            Self::Unicast(allocation) => allocation.driver,
        }
        .ok_or(CUDA_ERROR_INVALID_HANDLE.into())
    }

    pub(crate) fn busy(&self) -> bool {
        match self {
            Self::Unicast(allocation) => allocation.pins != 0,
        }
    }
}

pub struct ProcessState {
    pub namespace_pid: NamespacePid,
    pub malloc_regions: BTreeMap<u64, ipc::MallocRegion>,
    pub resources: BTreeMap<AllocationId, Resource>,
    pub virtual_allocation_handles: BTreeMap<u64, AllocationId>,
    pub mappings: BTreeMap<u64, Mapping>,
    pub raw: BTreeMap<u64, u32>,
    pub unsupported: u64,
    pub phase: Phase,
    pub arena: Option<host_carrier::Arena>,
    pub inflight: usize,
    next_virtual_allocation_handle: u64,
}

impl ProcessState {
    pub(crate) fn new(namespace_pid: NamespacePid) -> Self {
        Self {
            namespace_pid,
            malloc_regions: BTreeMap::new(),
            resources: BTreeMap::new(),
            virtual_allocation_handles: BTreeMap::new(),
            mappings: BTreeMap::new(),
            raw: BTreeMap::new(),
            unsupported: 0,
            next_virtual_allocation_handle: 1,
            phase: Phase::Active,
            arena: None,
            inflight: 0,
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
        if self.next_virtual_allocation_handle & VIRTUAL_ALLOCATION_HANDLE_MASK != 0 {
            return Err(CUDA_ERROR_OUT_OF_MEMORY.into());
        }
        Ok(())
    }

    pub(crate) fn mapped_resource(&self, address: u64) -> Option<AllocationId> {
        self.mappings
            .values()
            .find(|m| address >= m.address && address - m.address < m.size as u64)
            .map(|m| m.id)
    }

    pub(crate) fn mint_virtual_allocation_handle(&mut self, id: AllocationId) -> Result<u64> {
        if self.next_virtual_allocation_handle & VIRTUAL_ALLOCATION_HANDLE_MASK != 0 {
            return Err(CudaError::from(CUDA_ERROR_OUT_OF_MEMORY));
        }
        let handle = VIRTUAL_ALLOCATION_HANDLE_TAG | self.next_virtual_allocation_handle;
        self.next_virtual_allocation_handle += 1;
        self.virtual_allocation_handles.insert(handle, id);
        Ok(handle)
    }

    pub fn live_raw_imports(&self) -> u64 {
        self.raw.values().map(|count| u64::from(*count)).sum()
    }

    pub(crate) fn settle(&mut self, id: AllocationId) -> Result<()> {
        let handle_live = self
            .virtual_allocation_handles
            .values()
            .any(|value| *value == id);
        let mapped = self.mappings.values().any(|mapping| mapping.id == id);
        let resource = self
            .resources
            .get_mut(&id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        match resource {
            Resource::Unicast(allocation) => {
                // Unicast mappings retain the backing after its last handle is released.
                if !handle_live && let Some(driver) = allocation.driver {
                    unsafe { crate::driver::cuMemRelease(driver) }?;
                    allocation.driver = None;
                }
                if handle_live || mapped {
                    return Ok(());
                }
                cache()?.remove(&id)?;
            }
        }
        self.resources.remove(&id);
        Ok(())
    }
}

pub(crate) fn random<const N: usize>() -> Result<[u8; N]> {
    let mut bytes = [0; N];
    let mut offset = 0;
    while offset < N {
        let count = unsafe { libc::getrandom(bytes[offset..].as_mut_ptr().cast(), N - offset, 0) };
        if count > 0 {
            offset += count as usize;
        } else if count < 0
            && std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted
        {
            continue;
        } else {
            return Err(CudaError::from(CUDA_ERROR_NOT_INITIALIZED));
        }
    }
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
        state.next_virtual_allocation_handle = VIRTUAL_ALLOCATION_HANDLE_MASK;
        assert_eq!(state.new_reference(), Err(CUDA_ERROR_OUT_OF_MEMORY.into()));
        assert!(state.virtual_allocation_handles.is_empty());
    }
}
