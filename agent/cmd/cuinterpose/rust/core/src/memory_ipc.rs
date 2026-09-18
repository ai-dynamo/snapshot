// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Synchronous malloc and memory IPC over tracked VMM sharing.
//! Virtual IPC memory handles carry allocation identity; native memory IPC is never called.

use crate::{driver, state};
use cudarc::driver::sys::*;
use cuinterpose_protocol::{AllocationReference, VERSION as PROTOCOL_VERSION};
use driver::{CudaError, Result};

#[derive(Clone)]
pub struct Mapping {
    virtual_allocation_handle: CUmemGenericAllocationHandle,
    requested: usize,
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
    requested: [u8; 8],
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
    fn decode(handle: CUipcMemHandle) -> Result<Self> {
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

/// Attach a new VA to an existing virtual allocation handle while holding the state lock.
fn map(
    state: &mut state::State,
    virtual_allocation_handle: CUmemGenericAllocationHandle,
    requested: usize,
    extent: usize,
    opens: usize,
) -> Result<CUdeviceptr> {
    let id = state.virtual_allocation_handles[&virtual_allocation_handle];
    let allocation = &state.allocations[&id];
    let backing = allocation
        .driver
        .ok_or(CUresult::CUDA_ERROR_INVALID_HANDLE)?;
    let mut device = 0;
    unsafe { driver::cuCtxGetDevice(&mut device) }?;
    let mut address = 0;
    unsafe { driver::cuMemAddressReserve(&mut address, extent, 0, 0, 0) }?;
    if let Err(error) = unsafe { driver::cuMemMap(address, extent, 0, backing, 0) } {
        unsafe { driver::cuMemAddressFree(address, extent) }?;
        return Err(error);
    }
    let access = CUmemAccessDesc {
        location: CUmemLocation {
            type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
            id: device,
        },
        flags: CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE,
    };
    if let Err(error) = unsafe { driver::cuMemSetAccess(address, extent, &access, 1) } {
        unsafe { driver::cuMemUnmap(address, extent) }?;
        unsafe { driver::cuMemAddressFree(address, extent) }?;
        return Err(error);
    }
    state.mappings.insert(
        address,
        state::Mapping {
            id,
            address,
            size: extent,
            offset: 0,
            access: vec![access],
            unknown: false,
            flags: 0,
            checkpointed: false,
        },
    );
    state.mallocs.insert(
        address,
        Mapping {
            virtual_allocation_handle,
            requested,
            extent,
            context: state::context(),
            opens,
        },
    );
    Ok(address)
}

pub fn cuMemAlloc_v2(out: *mut CUdeviceptr, size: usize) -> Result<()> {
    if out.is_null() || size == 0 {
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
    }
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
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
    }
    let extent = size
        .checked_next_multiple_of(granularity)
        .ok_or(CUresult::CUDA_ERROR_OUT_OF_MEMORY)?;
    let mut virtual_allocation_handle = 0;
    state::cuMemCreate(&mut virtual_allocation_handle, extent, &properties, 0)?;
    let result = {
        let mut state = state::active()?;
        map(&mut state, virtual_allocation_handle, size, extent, 0)
    };
    match result {
        Ok(address) => {
            unsafe { out.write(address) };
            Ok(())
        }
        Err(error) => {
            state::cuMemRelease(virtual_allocation_handle)?;
            Err(error)
        }
    }
}

pub fn cuIpcGetMemHandle(out: *mut CUipcMemHandle, address: CUdeviceptr) -> Result<()> {
    if out.is_null() {
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
    }
    let mut state = state::active()?;
    let mapping = state
        .mallocs
        .get(&address)
        .ok_or(CUresult::CUDA_ERROR_INVALID_VALUE)?
        .clone();
    if mapping.opens != 0 {
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
    }
    let id = state.virtual_allocation_handles[&mapping.virtual_allocation_handle];
    let allocation = state
        .allocations
        .get_mut(&id)
        .ok_or(CUresult::CUDA_ERROR_INVALID_HANDLE)?;
    if !state::cache()?.contains(&id)? {
        let fd = driver::export_posix(
            allocation
                .driver
                .ok_or(CUresult::CUDA_ERROR_INVALID_HANDLE)?,
        )?;
        state::cache()?.replace(id, Some((fd, None)))?;
    }
    let virtual_ipc_mem_handle = VirtualIpcMemHandle {
        magic: VIRTUAL_IPC_MEM_HANDLE_MAGIC,
        creator_pid: allocation.reference.creator_pid.to_le_bytes(),
        allocation: id,
        reserved: [0; 20],
        requested: (mapping.requested as u64).to_le_bytes(),
        extent: (mapping.extent as u64).to_le_bytes(),
    };
    allocation.shared = true;
    unsafe {
        out.write(std::mem::transmute::<VirtualIpcMemHandle, CUipcMemHandle>(
            virtual_ipc_mem_handle,
        ))
    };
    Ok(())
}

pub fn cuIpcOpenMemHandle(out: *mut CUdeviceptr, handle: CUipcMemHandle, flags: u32) -> Result<()> {
    if out.is_null() || flags != CUipcMem_flags::CU_IPC_MEM_LAZY_ENABLE_PEER_ACCESS as u32 {
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
    }
    let virtual_ipc_mem_handle = VirtualIpcMemHandle::decode(handle)?;
    let mut state = state::active()?;
    let id = virtual_ipc_mem_handle.allocation;
    let context = state::context();
    if let Some(allocation) = state.allocations.get(&id)
        && allocation.reference.creator_pid
            != u32::from_le_bytes(virtual_ipc_mem_handle.creator_pid)
    {
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_HANDLE));
    }
    let state_ref = &mut *state;
    for (&address, mapping) in &mut state_ref.mallocs {
        if mapping.opens != 0
            && state_ref
                .virtual_allocation_handles
                .get(&mapping.virtual_allocation_handle)
                == Some(&id)
        {
            if mapping.requested != u64::from_le_bytes(virtual_ipc_mem_handle.requested) as usize
                || mapping.extent != u64::from_le_bytes(virtual_ipc_mem_handle.extent) as usize
            {
                return Err(CudaError(CUresult::CUDA_ERROR_INVALID_HANDLE));
            }
            if mapping.context != context {
                return Err(CudaError(CUresult::CUDA_ERROR_NOT_SUPPORTED));
            }
            mapping.opens = mapping
                .opens
                .checked_add(1)
                .ok_or(CUresult::CUDA_ERROR_OUT_OF_MEMORY)?;
            unsafe { out.write(address) };
            return Ok(());
        }
    }
    let creator_pid = u32::from_le_bytes(virtual_ipc_mem_handle.creator_pid);
    if creator_pid == state.namespace_pid {
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_HANDLE));
    }
    let reference = AllocationReference { creator_pid, id };
    let mut virtual_allocation_handle = 0;
    state::import_reference(state, &mut virtual_allocation_handle, reference)?;
    let mut state = state::active()?;
    let result = map(
        &mut state,
        virtual_allocation_handle,
        u64::from_le_bytes(virtual_ipc_mem_handle.requested) as usize,
        u64::from_le_bytes(virtual_ipc_mem_handle.extent) as usize,
        1,
    );
    match result {
        Ok(address) => {
            unsafe { out.write(address) };
            Ok(())
        }
        Err(error) => {
            state
                .virtual_allocation_handles
                .remove(&virtual_allocation_handle);
            state.settle(id)?;
            Err(error)
        }
    }
}

pub fn cuMemFree_v2(address: CUdeviceptr) -> Result<()> {
    release(address, false)
}

pub fn cuIpcCloseMemHandle(address: CUdeviceptr) -> Result<()> {
    release(address, true)
}

fn release(address: CUdeviceptr, imported: bool) -> Result<()> {
    // Synchronization must not hold STATE: another host thread may need the
    // shim or peer listener to complete the kernels being synchronized.
    {
        let mut state = state::active()?;
        let Some(mapping) = state.mallocs.get_mut(&address) else {
            if imported {
                return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
            }
            drop(state);
            return unsafe { driver::cuMemFree_v2(address) };
        };
        if (mapping.opens != 0) != imported {
            return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
        }
        if imported && mapping.opens > 1 {
            mapping.opens -= 1;
            return Ok(());
        }
        if mapping.context != state::context() {
            return Err(CudaError(CUresult::CUDA_ERROR_NOT_SUPPORTED));
        }
    }
    unsafe { driver::cuCtxSynchronize() }?;
    let mut state = state::active()?;
    if imported {
        let mapping = state
            .mallocs
            .get_mut(&address)
            .ok_or(CUresult::CUDA_ERROR_INVALID_VALUE)?;
        // An open may acquire a reference while synchronization runs unlocked.
        if mapping.opens > 1 {
            mapping.opens -= 1;
            return Ok(());
        }
    }
    let mapping = state
        .mallocs
        .get(&address)
        .ok_or(CUresult::CUDA_ERROR_INVALID_VALUE)?
        .clone();
    let id = state.virtual_allocation_handles[&mapping.virtual_allocation_handle];
    unsafe { driver::cuMemUnmap(address, mapping.extent) }?;
    state.mappings.remove(&address);
    state
        .virtual_allocation_handles
        .remove(&mapping.virtual_allocation_handle);
    state.mallocs.remove(&address);
    state.settle(id)?;
    unsafe { driver::cuMemAddressFree(address, mapping.extent) }
}

pub fn cuMemGetAddressRange_v2(
    base: *mut CUdeviceptr,
    size: *mut usize,
    address: CUdeviceptr,
) -> Result<()> {
    let state = state::active()?;
    if let Some((&start, mapping)) = state.mallocs.range(..=address).next_back()
        && address - start < mapping.requested as u64
    {
        unsafe {
            if !base.is_null() {
                base.write(start);
            }
            if !size.is_null() {
                size.write(mapping.requested);
            }
        }
        return Ok(());
    }
    drop(state);
    unsafe { driver::cuMemGetAddressRange_v2(base, size, address) }
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
