// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Synchronous malloc and memory IPC over tracked VMM sharing.
//! Virtual IPC memory handles carry allocation identity; native memory IPC is never called.

use super::{Memblock, ProcessState, VirtualAllocationHandle};
use crate::{driver, runtime};
use cudarc::driver::sys::*;
use cuinterpose_protocol::{AllocationReference, VERSION as PROTOCOL_VERSION};
use driver::{CudaError, Result};

#[derive(Clone)]
pub struct MallocRegion {
    pub(crate) virtual_allocation_handle: VirtualAllocationHandle,
    pub(crate) requested: usize,
    // The VA reservation survives independently of its current mapping.
    extent: usize,
    context: usize,
    opens: usize,
}

// CUDA fixes the public handle at 64 opaque bytes. Keep this representation
// local to the adapter; ordinary peer messages use the existing typed codec.
#[repr(C)]
#[derive(Clone, Copy)]
struct VirtualIpcMemHandle {
    magic: [u8; 8],
    creator_pid: [u8; 4],
    allocation: [u8; 16],
    reserved: [u8; 20],
    pub(crate) requested: [u8; 8],
    extent: [u8; 8],
}
const VIRTUAL_IPC_MEM_HANDLE_MAGIC: [u8; 8] = [
    b'C',
    b'U',
    b'I',
    b'P',
    b'C',
    b'0',
    b'0',
    b'0' + PROTOCOL_VERSION,
];
const _: () = assert!(size_of::<VirtualIpcMemHandle>() == size_of::<CUipcMemHandle>());

impl VirtualIpcMemHandle {
    pub(crate) fn decode(handle: CUipcMemHandle) -> Result<Self> {
        // Both representations contain only bytes with identical size/alignment.
        let virtual_ipc_mem_handle: Self = unsafe { std::mem::transmute(handle) };
        if virtual_ipc_mem_handle.magic != VIRTUAL_IPC_MEM_HANDLE_MAGIC
            || u32::from_le_bytes(virtual_ipc_mem_handle.creator_pid) == 0
            || virtual_ipc_mem_handle.reserved != [0; 20]
            || u64::from_le_bytes(virtual_ipc_mem_handle.requested) == 0
            || u64::from_le_bytes(virtual_ipc_mem_handle.requested)
                > u64::from_le_bytes(virtual_ipc_mem_handle.extent)
        {
            return Err(CudaError(CUresult::CUDA_ERROR_INVALID_HANDLE));
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
    ) -> Result<CUdeviceptr> {
        let handle = VirtualAllocationHandle::from_raw(virtual_allocation_handle)
            .ok_or(CUresult::CUDA_ERROR_INVALID_HANDLE)?;
        let result = (|| {
            let id = handle.id(self)?;
            let mut device = 0;
            unsafe { driver::cuCtxGetDevice(&mut device) }?;
            let mut address = 0;
            unsafe { driver::cuMemAddressReserve(&mut address, extent, 0, 0, 0) }?;
            self.map_unicast(id, address, extent, 0, 0)?;
            let access = CUmemAccessDesc {
                location: CUmemLocation {
                    type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                    id: device,
                },
                flags: CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE,
            };
            unsafe { driver::cuMemSetAccess(address, extent, &access, 1) }?;
            self.mappings.get_mut(&address).unwrap().access = vec![access];
            self.malloc_regions.insert(
                address,
                MallocRegion {
                    virtual_allocation_handle: handle,
                    requested,
                    extent,
                    context: crate::driver::context(),
                    opens,
                },
            );
            Ok(address)
        })();
        Ok(runtime::must_complete(result))
    }

    pub(crate) fn unmap_malloc(&mut self, address: CUdeviceptr) -> Result<()> {
        let mapping = self
            .malloc_regions
            .get(&address)
            .ok_or(CUresult::CUDA_ERROR_INVALID_VALUE)?
            .clone();
        let id = mapping.virtual_allocation_handle.id(self)?;
        unsafe { driver::cuMemUnmap(address, mapping.extent) }?;
        self.mappings.remove(&address);
        self.virtual_allocation_handles
            .remove(&mapping.virtual_allocation_handle);
        self.malloc_regions.remove(&address);
        runtime::must_complete(self.settle(id));
        runtime::must_complete(unsafe { driver::cuMemAddressFree(address, mapping.extent) });
        Ok(())
    }
}

pub(crate) fn release(address: CUdeviceptr, imported: bool) -> Result<()> {
    // Synchronization must not hold STATE: another host thread may need the
    // shim or peer listener to complete the kernels being synchronized.
    let mut state = runtime::active()?;
    {
        let Some(mapping) = state.malloc_regions.get_mut(&address) else {
            if imported {
                return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
            }
            return runtime::call_unlocked(state, || unsafe { driver::cuMemFree_v2(address) })
                .map(|_| ());
        };
        if (mapping.opens != 0) != imported {
            return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
        }
        if imported && mapping.opens > 1 {
            mapping.opens -= 1;
            return Ok(());
        }
        if mapping.context != crate::driver::context() {
            return Err(CudaError(CUresult::CUDA_ERROR_NOT_SUPPORTED));
        }
    }
    let (mut state, ()) = runtime::call_unlocked(state, || unsafe { driver::cuCtxSynchronize() })?;
    if imported {
        let mapping = state
            .malloc_regions
            .get_mut(&address)
            .ok_or(CUresult::CUDA_ERROR_INVALID_VALUE)?;
        // An open may acquire a reference while synchronization runs unlocked.
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
        for (&address, region) in &mut self.malloc_regions {
            if region.opens != 0
                && self
                    .virtual_allocation_handles
                    .get(&region.virtual_allocation_handle)
                    == Some(&reference.id)
            {
                if region.requested != requested || region.extent != extent {
                    return Err(CUresult::CUDA_ERROR_INVALID_HANDLE.into());
                }
                if region.context != driver::context() {
                    return Err(CUresult::CUDA_ERROR_NOT_SUPPORTED.into());
                }
                region.opens = region
                    .opens
                    .checked_add(1)
                    .ok_or(CUresult::CUDA_ERROR_OUT_OF_MEMORY)?;
                return Ok(Some(address));
            }
        }
        if reference.creator_pid == self.namespace_pid {
            return Err(CUresult::CUDA_ERROR_INVALID_HANDLE.into());
        }
        Ok(None)
    }
}

pub(crate) fn allocation_layout(size: usize) -> Result<(CUmemAllocationProp, usize)> {
    let mut device = 0;
    unsafe { driver::cuCtxGetDevice(&mut device) }?;
    let properties = CUmemAllocationProp {
        type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
        requestedHandleTypes: CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
        location: CUmemLocation {
            type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
            id: device,
        },
        ..unsafe { std::mem::zeroed() }
    };
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
    let extent = size
        .checked_next_multiple_of(granularity)
        .ok_or(CUresult::CUDA_ERROR_OUT_OF_MEMORY)?;
    Ok((properties, extent))
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
