// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Unicast backing and mapping metadata.

use super::{Memblock, ProcessState};
use crate::driver::Result;
use cudarc::driver::sys::CUresult::*;
use cudarc::driver::sys::*;
use cuinterpose_protocol::{AllocationId, AllocationReference, NamespacePid};

#[derive(Clone)]
pub struct Allocation {
    pub reference: AllocationReference,
    pub driver: Option<u64>,
    pub size: usize,
    pub properties: CUmemAllocationProp,
    pub shared: bool,
    pub context: usize,
}

impl Allocation {
    /// Only the creator saves shared device memory; private memory stays native.
    pub(crate) fn needs_content_checkpoint(&self, namespace_pid: NamespacePid) -> bool {
        self.reference.creator_pid == namespace_pid
            && self.properties.type_ == CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED
            && self.properties.location.type_ == CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE
            && self.shared
    }
}

// CUmemAllocationProp's Win32 pointer is opaque and is never dereferenced on Linux.
// Driver access and allocation metadata are serialized under ProcessState's mutex.
unsafe impl Send for Allocation {}

#[derive(Clone)]
pub struct Mapping {
    pub id: AllocationId,
    pub address: u64,
    pub size: usize,
    pub offset: usize,
    pub access: Vec<CUmemAccessDesc>,
    pub flags: u64,
}

pub(crate) fn access_metadata(access: &[CUmemAccessDesc]) -> Vec<(u32, i32, u32)> {
    let mut metadata: Vec<_> = access
        .iter()
        .map(|entry| {
            (
                entry.location.type_ as u32,
                entry.location.id,
                entry.flags as u32,
            )
        })
        .collect();
    metadata.sort();
    metadata
}
impl ProcessState {
    pub(crate) fn adopt_unicast(
        &mut self,
        reference: AllocationReference,
        driver: u64,
        size: usize,
        properties: CUmemAllocationProp,
        shared: bool,
    ) -> Result<u64> {
        let handle = self.mint_virtual_allocation_handle(reference.id)?;
        self.memblocks.insert(
            reference.id,
            Memblock::Unicast(Allocation {
                reference,
                driver: Some(driver),
                size,
                properties,
                shared,
                context: crate::driver::context(),
            }),
        );
        Ok(handle)
    }

    /// A CUDA retain may recover the backing or return a redundant reference.
    pub(crate) fn retain_backing(&mut self, id: AllocationId, driver: u64) -> Result<u64> {
        let allocation = self
            .memblocks
            .get_mut(&id)
            .and_then(Memblock::unicast_mut)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        if allocation.driver.is_some() {
            let _ = unsafe { crate::driver::cuMemRelease(driver) };
        } else {
            allocation.driver = Some(driver);
        }
        self.mint_virtual_allocation_handle(id)
    }

    pub(crate) fn map_unicast(
        &mut self,
        id: AllocationId,
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
            allocation.context = crate::driver::context();
        }
        self.mappings.insert(
            address,
            Mapping {
                id,
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
        assert_eq!(access_metadata(&merged), vec![(1, 0, 3), (1, 1, 1)]);
        assert_eq!(access_metadata(&mapping.access), vec![(1, 0, 1)]);
        let more_locations: Vec<_> = (1..40)
            .map(|id| descriptor(id, CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READ))
            .collect();
        let expanded = mapping.merged_access(&more_locations);
        assert_eq!(expanded.len(), 40);
        assert_eq!(access_metadata(&expanded)[0], (1, 0, 1));
        mapping.access = merged;
        assert_eq!(
            access_metadata(&mapping.merged_access(&[descriptor(
                0,
                CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_NONE
            ),])),
            vec![(1, 1, 1)]
        );
    }
}
