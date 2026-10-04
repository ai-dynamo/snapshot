// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Synchronous malloc and memory IPC use tracked VMM allocations. IPC handles identify
//! these allocations so the adapter can reopen them through peer exports without using
//! native memory IPC.

use super::{Memblock, ProcessState, VirtualAllocationHandle};
use crate::error::{Error, Result};
use crate::{driver, runtime};
use cudarc::driver::sys::*;
use cuinterpose_protocol::{AllocationReference, VERSION as PROTOCOL_VERSION};

#[derive(Clone)]
pub struct MallocRegion {
    pub(crate) virtual_allocation_handle: VirtualAllocationHandle,
    pub(crate) requested: usize,
    // The VA reservation survives independently of its current mapping.
    extent: usize,
    pub(super) context: usize,
    opens: usize,
}

// CUDA fixes the public handle at 64 opaque bytes, so this adapter owns that
// representation while other peer messages use the typed codec.
#[repr(C)]
#[derive(Clone, Copy)]
struct VirtualIpcMemHandle {
    magic: [u8; 8],
    creator_pid: [u8; 4],
    allocation: [u8; 16],
    reserved: [u8; 20],
    requested: [u8; 8],
    extent: [u8; 8],
}
const VIRTUAL_IPC_MEM_HANDLE_MAGIC: [u8; 8] = {
    let mut magic = *b"CUIPC000";
    magic[7] += PROTOCOL_VERSION;
    magic
};
const _: () = assert!(size_of::<VirtualIpcMemHandle>() == size_of::<CUipcMemHandle>());

impl VirtualIpcMemHandle {
    fn decode(handle: CUipcMemHandle) -> Result<Self> {
        // Both representations contain only bytes and have the same size and alignment.
        let virtual_ipc_mem_handle: Self = unsafe { std::mem::transmute(handle) };
        if virtual_ipc_mem_handle.magic != VIRTUAL_IPC_MEM_HANDLE_MAGIC
            || u32::from_le_bytes(virtual_ipc_mem_handle.creator_pid) == 0
            || virtual_ipc_mem_handle.reserved != [0; 20]
            || u64::from_le_bytes(virtual_ipc_mem_handle.requested) == 0
            || u64::from_le_bytes(virtual_ipc_mem_handle.requested)
                > u64::from_le_bytes(virtual_ipc_mem_handle.extent)
        {
            return Err(Error::Cuda(CUresult::CUDA_ERROR_INVALID_HANDLE));
        }
        Ok(virtual_ipc_mem_handle)
    }
}

impl ProcessState {
    /// Attach a new VA to an existing virtual allocation handle while holding the state lock.
    pub(crate) fn map_malloc(
        &mut self,
        virtual_allocation_handle: CUmemGenericAllocationHandle,
        requested: usize,
        extent: usize,
        opens: usize,
        context: usize,
        device: CUdevice,
    ) -> Result<CUdeviceptr> {
        let handle = VirtualAllocationHandle::from_raw(virtual_allocation_handle)
            .ok_or(CUresult::CUDA_ERROR_INVALID_HANDLE)?;
        let id = handle.id(self)?;
        let mut reserved = None;
        let mut mapped = false;
        let result = (|| {
            let mut address = 0;
            let alignment = 0;
            let requested_address = 0;
            let flags = 0;
            unsafe {
                driver::cuMemAddressReserve(
                    &mut address,
                    extent,
                    alignment,
                    requested_address,
                    flags,
                )
            }?;
            reserved = Some(address);
            let offset = 0;
            self.map_unicast(id, handle, address, extent, offset, flags)?;
            mapped = true;
            let mut access = vec![peer_access(device)];
            if let Some(peers) = self.malloc_peers.get(&context) {
                for &device in peers.values() {
                    if !access.iter().any(|entry| entry.location.id == device) {
                        access.push(peer_access(device));
                    }
                }
            }
            unsafe { driver::cuMemSetAccess(address, extent, access.as_ptr(), access.len()) }?;
            self.mappings.get_mut(&address).unwrap().access = access;
            self.malloc_regions.insert(
                address,
                MallocRegion {
                    virtual_allocation_handle: handle,
                    requested,
                    extent,
                    context,
                    opens,
                },
            );
            if opens != 0 {
                self.imported_mallocs.insert(id, address);
            }
            Ok(address)
        })();
        if result.is_err() {
            // The mapping and handle have not been published to the application, so
            // rollback can undo this call's changes and return the original CUDA error.
            if let Some(address) = reserved {
                if mapped {
                    runtime::must_complete(unsafe { driver::cuMemUnmap(address, extent) });
                    self.remove_mapping(address);
                }
                runtime::must_complete(unsafe { driver::cuMemAddressFree(address, extent) });
            }
            runtime::must_complete(self.release_virtual_handle(handle));
        }
        result
    }

    pub(crate) fn unmap_malloc(&mut self, address: CUdeviceptr) -> Result<()> {
        let mapping = self
            .malloc_regions
            .get(&address)
            .ok_or(CUresult::CUDA_ERROR_INVALID_VALUE)?
            .clone();
        unsafe { driver::cuMemUnmap(address, mapping.extent) }?;
        let id = self.remove_mapping(address);
        self.malloc_regions.remove(&address);
        if mapping.opens != 0 {
            self.imported_mallocs.remove(&id);
        }
        runtime::must_complete(self.release_virtual_handle(mapping.virtual_allocation_handle));
        runtime::must_complete(unsafe { driver::cuMemAddressFree(address, mapping.extent) });
        Ok(())
    }
}

fn peer_access(device: CUdevice) -> CUmemAccessDesc {
    CUmemAccessDesc {
        location: CUmemLocation {
            type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
            id: device,
        },
        flags: CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE,
    }
}

struct PeerAccessUpdate {
    address: CUdeviceptr,
    size: usize,
    previous: CUmemAccessDesc,
    access: Vec<CUmemAccessDesc>,
}

pub fn cuCtxEnablePeerAccess(peer: CUcontext, flags: u32) -> Result<()> {
    // Serialize native peer changes and VMM grants with malloc creation and free.
    // An allocation must not miss a successful enable while being published.
    let mut state = runtime::active()?;
    let context = driver::context()?;
    let mut device = 0;
    unsafe { driver::cuCtxGetDevice(&mut device) }?;
    let access = peer_access(device);
    let updates: Vec<_> = state
        .malloc_regions
        .iter()
        .filter(|(_, region)| region.context == peer as usize)
        .map(|(&address, region)| {
            let mapping = &state.mappings[&address];
            let previous = mapping
                .access
                .iter()
                .find(|entry| {
                    entry.location.type_ == access.location.type_ && entry.location.id == device
                })
                .copied()
                .unwrap_or(CUmemAccessDesc {
                    flags: CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_NONE,
                    ..access
                });
            PeerAccessUpdate {
                address,
                size: region.extent,
                previous,
                access: mapping.merged_access(&[access]),
            }
        })
        .collect();
    unsafe { driver::cuCtxEnablePeerAccess(peer, flags) }?;
    for (index, update) in updates.iter().enumerate() {
        if let Err(error) =
            unsafe { driver::cuMemSetAccess(update.address, update.size, &access, 1) }
        {
            // CUDA does not promise that a failed access update changed no pages.
            // Restore the attempted range too, then undo this call's native enable.
            // No metadata has been committed and every previous permission survives.
            for applied in updates[..=index].iter().rev() {
                runtime::must_complete(unsafe {
                    driver::cuMemSetAccess(applied.address, applied.size, &applied.previous, 1)
                });
            }
            runtime::must_complete(unsafe { driver::cuCtxDisablePeerAccess(peer) });
            return Err(error);
        }
    }
    for update in updates {
        state.mappings.get_mut(&update.address).unwrap().access = update.access;
    }
    state
        .malloc_peers
        .entry(peer as usize)
        .or_default()
        .insert(context, device);
    Ok(())
}

pub fn cuCtxDisablePeerAccess(peer: CUcontext) -> Result<()> {
    let mut state = runtime::active()?;
    let context = driver::context()?;
    unsafe { driver::cuCtxDisablePeerAccess(peer) }?;
    if let Some(peers) = state.malloc_peers.get_mut(&(peer as usize)) {
        peers.remove(&context);
        if peers.is_empty() {
            state.malloc_peers.remove(&(peer as usize));
        }
    }
    // Existing VMM mappings keep their permissions. The adapter does not promise
    // context isolation, but future allocations no longer pay for this peer.
    Ok(())
}

#[derive(Clone, Copy, Eq, PartialEq)]
pub(crate) enum Ownership {
    Owned,
    Imported,
}

pub(crate) fn release(address: CUdeviceptr, ownership: Ownership) -> Result<()> {
    // Synchronization runs without the state lock because completing a kernel may
    // require another host thread to enter the shim or peer listener.
    let mut state = runtime::active()?;
    {
        let Some(mapping) = state.malloc_regions.get_mut(&address) else {
            if ownership == Ownership::Imported {
                return Err(Error::Cuda(CUresult::CUDA_ERROR_INVALID_VALUE));
            }
            return runtime::call_unlocked(state, || unsafe { driver::cuMemFree_v2(address) })
                .map(|_| ());
        };
        match (ownership, mapping.opens) {
            (Ownership::Owned, 0) | (Ownership::Imported, 1) => {}
            (Ownership::Imported, opens) if opens > 1 => {
                mapping.opens -= 1;
                return Ok(());
            }
            _ => return Err(CUresult::CUDA_ERROR_INVALID_VALUE.into()),
        }
        if mapping.context != crate::driver::context()? {
            return Err(Error::Cuda(CUresult::CUDA_ERROR_NOT_SUPPORTED));
        }
    }
    let (mut state, ()) = runtime::call_unlocked(state, || unsafe { driver::cuCtxSynchronize() })?;
    if ownership == Ownership::Imported {
        let mapping = state
            .malloc_regions
            .get_mut(&address)
            .ok_or(CUresult::CUDA_ERROR_INVALID_VALUE)?;
        // Another open can add a reference while synchronization runs without the lock.
        if mapping.opens > 1 {
            mapping.opens -= 1;
            return Ok(());
        }
    }
    state.unmap_malloc(address)
}

pub(crate) fn decode(handle: CUipcMemHandle) -> Result<(AllocationReference, usize, usize)> {
    let handle = VirtualIpcMemHandle::decode(handle)?;
    Ok((
        AllocationReference {
            creator_pid: u32::from_le_bytes(handle.creator_pid),
            id: handle.allocation,
        },
        u64::from_le_bytes(handle.requested) as usize,
        u64::from_le_bytes(handle.extent) as usize,
    ))
}

impl MallocRegion {
    pub(crate) fn export_handle(&self, reference: AllocationReference) -> Result<CUipcMemHandle> {
        if self.opens != 0 {
            return Err(CUresult::CUDA_ERROR_INVALID_VALUE.into());
        }
        let handle = VirtualIpcMemHandle {
            magic: VIRTUAL_IPC_MEM_HANDLE_MAGIC,
            creator_pid: reference.creator_pid.to_le_bytes(),
            allocation: reference.id,
            reserved: [0; 20],
            requested: (self.requested as u64).to_le_bytes(),
            extent: (self.extent as u64).to_le_bytes(),
        };
        Ok(unsafe { std::mem::transmute::<VirtualIpcMemHandle, CUipcMemHandle>(handle) })
    }
}

impl ProcessState {
    /// Repeated opens share a VA and one virtual handle in the same CUDA context.
    pub(crate) fn reopen_malloc(
        &mut self,
        reference: AllocationReference,
        requested: usize,
        extent: usize,
    ) -> Result<Option<CUdeviceptr>> {
        if let Some(allocation) = self
            .memblocks
            .get(&reference.id)
            .and_then(Memblock::unicast)
            && allocation.reference != reference
        {
            return Err(CUresult::CUDA_ERROR_INVALID_HANDLE.into());
        }
        if let Some(&address) = self.imported_mallocs.get(&reference.id) {
            let region = self.malloc_regions.get_mut(&address).unwrap();
            if region.requested != requested || region.extent != extent {
                return Err(CUresult::CUDA_ERROR_INVALID_HANDLE.into());
            }
            if region.context != driver::context()? {
                return Err(CUresult::CUDA_ERROR_NOT_SUPPORTED.into());
            }
            region.opens = region
                .opens
                .checked_add(1)
                .ok_or(CUresult::CUDA_ERROR_OUT_OF_MEMORY)?;
            return Ok(Some(address));
        }
        if reference.creator_pid == self.namespace_pid {
            return Err(CUresult::CUDA_ERROR_INVALID_HANDLE.into());
        }
        Ok(None)
    }
}

impl ProcessState {
    pub(crate) fn allocation_layout(
        &mut self,
        device: CUdevice,
        size: usize,
    ) -> Result<(CUmemAllocationProp, usize)> {
        let mut properties = CUmemAllocationProp {
            type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
            requestedHandleTypes:
                CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
            location: CUmemLocation {
                type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                id: device,
            },
            ..unsafe { std::mem::zeroed() }
        };
        // These layout properties are fixed for each device, but the current device can
        // change between calls, so the cache is keyed by device.
        let granularity = match self.malloc_layouts.entry(device) {
            std::collections::btree_map::Entry::Occupied(entry) => {
                let (granularity, rdma_capable) = *entry.get();
                properties.allocFlags.gpuDirectRDMACapable = rdma_capable;
                granularity
            }
            std::collections::btree_map::Entry::Vacant(entry) => {
                let mut rdma = 0;
                let mut vmm_rdma = 0;
                unsafe {
                    driver::cuDeviceGetAttribute(
                        &mut rdma,
                        CUdevice_attribute::CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_SUPPORTED,
                        device,
                    )?;
                    driver::cuDeviceGetAttribute(&mut vmm_rdma, CUdevice_attribute::CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_WITH_CUDA_VMM_SUPPORTED, device)?;
                }
                properties.allocFlags.gpuDirectRDMACapable = u8::from(rdma != 0 && vmm_rdma != 0);
                // Ordinary cuMemAlloc does not request compression or tile-pool usage,
                // so the converted allocation leaves both disabled.
                let mut granularity = 0;
                unsafe {
                    driver::cuMemGetAllocationGranularity(
                        &mut granularity,
                        &properties,
                        CUmemAllocationGranularity_flags::CU_MEM_ALLOC_GRANULARITY_MINIMUM,
                    )
                }?;
                if granularity == 0 {
                    return Err(CUresult::CUDA_ERROR_INVALID_VALUE.into());
                }
                entry.insert((granularity, properties.allocFlags.gpuDirectRDMACapable));
                granularity
            }
        };
        let extent = size
            .checked_next_multiple_of(granularity)
            .ok_or(CUresult::CUDA_ERROR_OUT_OF_MEMORY)?;
        Ok((properties, extent))
    }
}
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn virtual_ipc_mem_handle_rejects_foreign_handles_and_invalid_lengths() {
        assert!(VirtualIpcMemHandle::decode(unsafe { std::mem::zeroed() }).is_err());
        let mut virtual_ipc_mem_handle = VirtualIpcMemHandle {
            magic: VIRTUAL_IPC_MEM_HANDLE_MAGIC,
            creator_pid: 1u32.to_le_bytes(),
            allocation: [2; 16],
            reserved: [0; 20],
            requested: 17u64.to_le_bytes(),
            extent: 4096u64.to_le_bytes(),
        };
        let decoded = VirtualIpcMemHandle::decode(unsafe {
            std::mem::transmute::<VirtualIpcMemHandle, CUipcMemHandle>(virtual_ipc_mem_handle)
        })
        .expect("valid virtual IPC memory handle");
        assert_eq!(u32::from_le_bytes(decoded.creator_pid), 1);
        assert_eq!(u64::from_le_bytes(decoded.requested), 17);
        virtual_ipc_mem_handle.extent = 16u64.to_le_bytes();
        assert!(
            VirtualIpcMemHandle::decode(unsafe {
                std::mem::transmute::<VirtualIpcMemHandle, CUipcMemHandle>(virtual_ipc_mem_handle)
            })
            .is_err()
        );
    }
}
