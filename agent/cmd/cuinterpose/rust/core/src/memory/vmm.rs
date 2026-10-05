// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Unicast backing and mapping metadata.

use super::{HandleEntry, Memblock, ProcessState, Refcounts, VirtualAllocationHandle};
use crate::error::Result;
use cudarc::driver::sys::CUresult::*;
use cudarc::driver::sys::*;
use cuinterpose_protocol::{AllocationId, AllocationReference};
use std::collections::btree_map::Entry;

/// The host carrier must be able to reconstruct tracked unicast memory.
pub(crate) fn validate_properties(properties: &CUmemAllocationProp) -> Result<()> {
    if properties.type_ != CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED
        || !matches!(
            properties.location.type_,
            CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE
                | CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA
        )
    {
        return Err(CUDA_ERROR_NOT_SUPPORTED.into());
    }
    Ok(())
}

#[derive(Clone)]
pub struct Allocation {
    pub reference: AllocationReference,
    pub refcounts: Refcounts,
    pub driver: Option<u64>,
    pub size: usize,
    pub properties: CUmemAllocationProp,
    pub shared: bool,
    pub context: usize,
}

/// A host NUMA node ID describes memory placement and cannot serve as a CUDA device
/// ordinal. Context::run uses this fallback only when no recorded context remains.
pub(crate) fn context_device(properties: &CUmemAllocationProp) -> i32 {
    if properties.location.type_ == CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE {
        properties.location.id
    } else {
        0
    }
}

// This code never dereferences the opaque Win32 pointer on Linux, and driver access and
// allocation metadata updates are serialized by the ProcessState mutex.
unsafe impl Send for Allocation {}

#[derive(Clone)]
pub struct Mapping {
    pub id: AllocationId,
    pub handle: VirtualAllocationHandle,
    pub address: u64,
    pub size: usize,
    pub offset: usize,
    pub access: Vec<CUmemAccessDesc>,
    pub flags: u64,
}

pub(crate) fn access_metadata(
    access: &[CUmemAccessDesc],
) -> Vec<cuinterpose_protocol::MemoryAccess> {
    let mut metadata: Vec<_> = access
        .iter()
        .map(|entry| cuinterpose_protocol::MemoryAccess {
            location: cuinterpose_protocol::MemoryLocation {
                location_type: entry.location.type_ as u32,
                id: entry.location.id,
            },
            flags: entry.flags as u32,
        })
        .collect();
    // Equivalent access sets must compare equally regardless of descriptor order.
    metadata.sort();
    metadata
}
impl ProcessState {
    pub(crate) fn adopt_unicast(&mut self, mut allocation: Allocation) -> Result<u64> {
        let id = allocation.reference.id;
        let handle = self.mint_virtual_allocation_handle(id)?;
        allocation.refcounts.handle_entries = 1;
        self.memblocks.insert(id, Memblock::Unicast(allocation));
        Ok(handle)
    }

    /// Recover the driver reference if needed while retaining the original application handle.
    pub(crate) fn retain_backing(
        &mut self,
        id: AllocationId,
        handle: VirtualAllocationHandle,
        driver: u64,
    ) -> Result<u64> {
        let memblock = self
            .memblocks
            .get_mut(&id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        if let Memblock::Unicast(allocation) = memblock
            && allocation.driver.is_none()
        {
            allocation.driver = Some(driver);
        } else {
            unsafe { crate::driver::cuMemRelease(driver) }?;
        }
        let entry = match self.virtual_allocation_handles.entry(handle) {
            Entry::Occupied(entry) => entry.into_mut(),
            Entry::Vacant(entry) => {
                memblock.refcounts_mut().handle_entries += 1;
                entry.insert(HandleEntry { id, references: 0 })
            }
        };
        entry.references += 1;
        Ok(handle.as_raw())
    }

    pub(crate) fn map_unicast(
        &mut self,
        id: AllocationId,
        handle: VirtualAllocationHandle,
        address: u64,
        size: usize,
        offset: usize,
        flags: u64,
    ) -> Result<()> {
        let allocation = self
            .memblocks
            .get_mut(&id)
            .and_then(Memblock::unicast_mut)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        let context = if allocation.context == 0 {
            crate::driver::context()?
        } else {
            allocation.context
        };
        unsafe {
            crate::driver::cuMemMap(
                address,
                size,
                offset,
                allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                flags,
            )
        }?;
        if allocation.context == 0 {
            allocation.context = context;
        }
        allocation.refcounts.mappings += 1;
        self.mappings.insert(
            address,
            Mapping {
                id,
                handle,
                address,
                size,
                offset,
                access: Vec::new(),
                flags,
            },
        );
        Ok(())
    }
}

impl Mapping {
    pub(crate) fn merged_access(&self, descriptors: &[CUmemAccessDesc]) -> Vec<CUmemAccessDesc> {
        let mut entries = self.access.clone();
        for descriptor in descriptors {
            entries.retain(|entry| {
                entry.location.type_ != descriptor.location.type_
                    || entry.location.id != descriptor.location.id
            });
            if descriptor.flags != CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_NONE {
                entries.push(*descriptor);
            }
        }
        entries
    }
}
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn access_updates_replace_permissions_per_location() {
        let descriptor = |id, flags| CUmemAccessDesc {
            location: CUmemLocation {
                type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                id,
            },
            flags,
        };
        let mut mapping = Mapping {
            id: [1; 16],
            handle: VirtualAllocationHandle::from_raw(VirtualAllocationHandle::TAG | 1).unwrap(),
            address: 4096,
            size: 4096,
            offset: 0,
            access: vec![descriptor(
                0,
                CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READ,
            )],
            flags: 0,
        };
        let merged = mapping.merged_access(&[
            descriptor(0, CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE),
            descriptor(1, CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READ),
        ]);
        assert_eq!(
            access_metadata(&merged),
            vec![
                cuinterpose_protocol::MemoryAccess {
                    location: cuinterpose_protocol::MemoryLocation {
                        location_type: 1,
                        id: 0
                    },
                    flags: 3
                },
                cuinterpose_protocol::MemoryAccess {
                    location: cuinterpose_protocol::MemoryLocation {
                        location_type: 1,
                        id: 1
                    },
                    flags: 1
                }
            ]
        );
        assert_eq!(
            access_metadata(&mapping.access),
            vec![cuinterpose_protocol::MemoryAccess {
                location: cuinterpose_protocol::MemoryLocation {
                    location_type: 1,
                    id: 0
                },
                flags: 1
            }]
        );
        let more_locations: Vec<_> = (1..40)
            .map(|id| descriptor(id, CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READ))
            .collect();
        let expanded = mapping.merged_access(&more_locations);
        assert_eq!(expanded.len(), 40);
        assert_eq!(
            access_metadata(&expanded)[0],
            cuinterpose_protocol::MemoryAccess {
                location: cuinterpose_protocol::MemoryLocation {
                    location_type: 1,
                    id: 0
                },
                flags: 1
            }
        );
        mapping.access = merged;
        assert_eq!(
            access_metadata(&mapping.merged_access(&[descriptor(
                0,
                CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_NONE
            ),])),
            vec![cuinterpose_protocol::MemoryAccess {
                location: cuinterpose_protocol::MemoryLocation {
                    location_type: 1,
                    id: 1
                },
                flags: 1
            }]
        );
    }
}
