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
    /// Another process exports or imports this backing.
    pub shared: bool,
    /// Another GPU received access to this backing. Native CUDA can fail to restore
    /// such backing even without an export, so this is never cleared.
    pub peer_access_granted: bool,
    /// Access from any other GPU is peer access. `None` treats every GPU as a peer.
    pub home_device: Option<i32>,
    pub context: usize,
}

impl Allocation {
    pub(crate) fn uses_host_carrier(&self) -> bool {
        self.shared || self.peer_access_granted
    }
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

/// Device memory belongs to its GPU, and host memory to the GPU whose context created
/// it. Host memory created without a current context has no home GPU.
pub(crate) fn home_device(properties: &CUmemAllocationProp, context: usize) -> Result<Option<i32>> {
    if properties.location.type_ == CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE {
        return Ok(Some(properties.location.id));
    }
    if context == 0 {
        return Ok(None);
    }
    let mut device = 0;
    unsafe { crate::driver::cuCtxGetDevice(&mut device) }?;
    Ok(Some(device))
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

pub(crate) fn location_metadata(location: CUmemLocation) -> cuinterpose_protocol::MemoryLocation {
    cuinterpose_protocol::MemoryLocation {
        location_type: location.type_ as u32,
        id: location.id,
    }
}

pub(crate) fn access_metadata(
    access: &[CUmemAccessDesc],
) -> Vec<cuinterpose_protocol::MemoryAccess> {
    let mut metadata: Vec<_> = access
        .iter()
        .map(|entry| cuinterpose_protocol::MemoryAccess {
            location: location_metadata(entry.location),
            flags: entry.flags as u32,
        })
        .collect();
    // Equivalent access sets must compare equally regardless of descriptor order.
    metadata.sort();
    metadata
}
impl ProcessState {
    /// Publish permissions only after the driver has accepted the access update.
    pub(crate) fn set_mapping_access(&mut self, address: u64, access: Vec<CUmemAccessDesc>) {
        let mapping = self.mappings.get_mut(&address).unwrap();
        if let Some(allocation) = self
            .memblocks
            .get_mut(&mapping.id)
            .and_then(Memblock::unicast_mut)
            && access.iter().any(|entry| {
                entry.location.type_ == CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE
                    && Some(entry.location.id) != allocation.home_device
                    && entry.flags != CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_NONE
            })
        {
            allocation.peer_access_granted = true;
        }
        mapping.access = access;
    }

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

    fn device_access(id: i32, flags: CUmemAccess_flags) -> CUmemAccessDesc {
        CUmemAccessDesc {
            location: CUmemLocation {
                type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                id,
            },
            flags,
        }
    }

    #[test]
    fn only_peer_access_selects_the_host_carrier() {
        use CUmemAccess_flags::*;
        use CUmemLocationType::*;
        let owner = device_access(0, CU_MEM_ACCESS_FLAGS_PROT_READWRITE);
        let peer = device_access(1, CU_MEM_ACCESS_FLAGS_PROT_READWRITE);
        let peer_none = device_access(1, CU_MEM_ACCESS_FLAGS_PROT_NONE);
        for (case, location, home_device, updates, peer_access_granted) in [
            (
                "owner only",
                CU_MEM_LOCATION_TYPE_DEVICE,
                Some(0),
                vec![vec![owner]],
                false,
            ),
            (
                "peer access",
                CU_MEM_LOCATION_TYPE_DEVICE,
                Some(0),
                vec![vec![owner, peer]],
                true,
            ),
            (
                "PROT_NONE only",
                CU_MEM_LOCATION_TYPE_DEVICE,
                Some(0),
                vec![vec![owner, peer_none]],
                false,
            ),
            // cuMemSetAccess drops revoked locations before publishing the mapping.
            (
                "grant then revoke",
                CU_MEM_LOCATION_TYPE_DEVICE,
                Some(0),
                vec![vec![owner, peer], vec![owner]],
                true,
            ),
            // Host NUMA node 1 is not GPU 1. Host memory belongs to its creating GPU.
            (
                "host, creating GPU only",
                CU_MEM_LOCATION_TYPE_HOST_NUMA,
                Some(0),
                vec![vec![owner]],
                false,
            ),
            (
                "host, another GPU",
                CU_MEM_LOCATION_TYPE_HOST_NUMA,
                Some(0),
                vec![vec![owner, peer]],
                true,
            ),
            (
                "host, no creating GPU",
                CU_MEM_LOCATION_TYPE_HOST_NUMA,
                None,
                vec![vec![owner]],
                true,
            ),
        ] {
            let mut state = ProcessState::new(41);
            let reference = state.new_reference().unwrap();
            let handle = state
                .adopt_unicast(Allocation {
                    reference,
                    refcounts: Default::default(),
                    driver: None,
                    size: 4096,
                    properties: CUmemAllocationProp {
                        type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
                        location: CUmemLocation {
                            type_: location,
                            id: if location == CU_MEM_LOCATION_TYPE_HOST_NUMA {
                                1
                            } else {
                                0
                            },
                        },
                        ..unsafe { std::mem::zeroed() }
                    },
                    shared: false,
                    peer_access_granted: false,
                    home_device,
                    context: 0,
                })
                .unwrap();
            state.mappings.insert(
                4096,
                Mapping {
                    id: reference.id,
                    handle: VirtualAllocationHandle::from_raw(handle).unwrap(),
                    address: 4096,
                    size: 4096,
                    offset: 0,
                    access: Vec::new(),
                    flags: 0,
                },
            );
            for access in &updates {
                state.set_mapping_access(4096, access.clone());
            }

            let allocation = state.memblocks[&reference.id].unicast().unwrap();
            assert!(!allocation.shared, "{case}");
            assert_eq!(
                allocation.peer_access_granted, peer_access_granted,
                "{case}"
            );
            assert_eq!(
                allocation.uses_host_carrier(),
                peer_access_granted,
                "{case}"
            );
            assert_eq!(
                access_metadata(&state.mappings[&4096].access),
                access_metadata(updates.last().unwrap()),
                "{case}"
            );
            let records = state.inspect().unwrap();
            assert!(
                records.iter().any(|record| matches!(
                    record,
                    cuinterpose_protocol::Record::Allocation { shared: false, peer_access_granted: granted, .. }
                        if *granted == peer_access_granted
                )),
                "{case}"
            );
        }
    }

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
