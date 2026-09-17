// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Synchronous malloc and memory IPC over tracked VMM sharing.
//! The CUDA IPC handle carries an identity ticket; native memory IPC is never called.

use crate::{driver, state};
use cudarc::driver::sys::*;
use cuinterpose_protocol::{AllocationId, ParticipantId, Resource, ResourceKind, Ticket};
use driver::{CudaError, Result};
use std::path::Path;

#[derive(Clone)]
pub struct Mapping {
    handle: CUmemGenericAllocationHandle,
    requested: usize,
    extent: usize,
    context: usize,
    opens: usize,
}

// CUDA fixes the public handle at 64 opaque bytes. Keep this representation
// local to the adapter; ordinary peer messages use the existing typed codec.
#[repr(C)]
#[derive(Clone, Copy)]
struct InlineTicket {
    version: [u8; 8],
    creator: [u8; 16],
    allocation: [u8; 16],
    pid: [u8; 4],
    reserved: [u8; 4],
    requested: [u8; 8],
    extent: [u8; 8],
}
const VERSION: [u8; 8] = *b"CUIPC001";
const _: () = assert!(size_of::<InlineTicket>() == size_of::<CUipcMemHandle>());

impl InlineTicket {
    fn decode(handle: CUipcMemHandle) -> Result<Self> {
        // Both representations contain only bytes with identical size/alignment.
        let ticket: Self = unsafe { std::mem::transmute(handle) };
        if ticket.version != VERSION
            || ticket.reserved != [0; 4]
            || u32::from_le_bytes(ticket.pid) == 0
            || u64::from_le_bytes(ticket.requested) == 0
            || u64::from_le_bytes(ticket.requested) > u64::from_le_bytes(ticket.extent)
        {
            return Err(CudaError(CUresult::CUDA_ERROR_INVALID_HANDLE));
        }
        Ok(ticket)
    }
}

/// Attach a new VA to an existing logical handle while holding the state lock.
fn map(
    state: &mut state::State,
    handle: CUmemGenericAllocationHandle,
    requested: usize,
    extent: usize,
    opens: usize,
) -> Result<CUdeviceptr> {
    let id = state.handles[&handle];
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
            handle,
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
    let mut handle = 0;
    state::cuMemCreate(&mut handle, extent, &properties, 0)?;
    let result = {
        let mut state = state::active()?;
        map(&mut state, handle, size, extent, 0)
    };
    match result {
        Ok(address) => {
            unsafe { out.write(address) };
            Ok(())
        }
        Err(error) => {
            state::cuMemRelease(handle)?;
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
    let id = state.handles[&mapping.handle];
    let allocation = state
        .allocations
        .get_mut(&id)
        .ok_or(CUresult::CUDA_ERROR_INVALID_HANDLE)?;
    if !state::cache()?.contains(&(ResourceKind::Unicast, id))? {
        let fd = driver::export_posix(
            allocation
                .driver
                .ok_or(CUresult::CUDA_ERROR_INVALID_HANDLE)?,
        )?;
        state::cache()?.replace((ResourceKind::Unicast, id), Some(fd))?;
    }
    let ticket = InlineTicket {
        version: VERSION,
        creator: allocation.ticket.creator.0,
        allocation: id.0,
        pid: (unsafe { libc::getpid() } as u32).to_le_bytes(),
        reserved: [0; 4],
        requested: (mapping.requested as u64).to_le_bytes(),
        extent: (mapping.extent as u64).to_le_bytes(),
    };
    allocation.shared = true;
    unsafe { out.write(std::mem::transmute::<InlineTicket, CUipcMemHandle>(ticket)) };
    Ok(())
}

pub fn cuIpcOpenMemHandle(out: *mut CUdeviceptr, handle: CUipcMemHandle, flags: u32) -> Result<()> {
    if out.is_null() || flags != CUipcMem_flags::CU_IPC_MEM_LAZY_ENABLE_PEER_ACCESS as u32 {
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_VALUE));
    }
    let inline = InlineTicket::decode(handle)?;
    let mut state = state::active()?;
    let id = AllocationId(inline.allocation);
    let context = state::context();
    if let Some(allocation) = state.allocations.get(&id)
        && allocation.ticket.creator != ParticipantId(inline.creator)
    {
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_HANDLE));
    }
    let state_ref = &mut *state;
    for (&address, mapping) in &mut state_ref.mallocs {
        if mapping.opens != 0 && state_ref.handles.get(&mapping.handle) == Some(&id) {
            if mapping.requested != u64::from_le_bytes(inline.requested) as usize
                || mapping.extent != u64::from_le_bytes(inline.extent) as usize
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
    if ParticipantId(inline.creator) == state.identity {
        return Err(CudaError(CUresult::CUDA_ERROR_INVALID_HANDLE));
    }
    let directory = Path::new(&state.endpoint)
        .parent()
        .ok_or(CUresult::CUDA_ERROR_INVALID_HANDLE)?;
    let ticket = Ticket {
        creator: ParticipantId(inline.creator),
        allocation: id,
        resource: Resource::Unicast,
        endpoint: directory
            .join(format!(
                "cuinterpose-{}.sock",
                u32::from_le_bytes(inline.pid)
            ))
            .to_str()
            .ok_or(CUresult::CUDA_ERROR_INVALID_HANDLE)?
            .to_owned(),
    };
    let logical = state::import_ticket(&mut state, ticket)?;
    let result = map(
        &mut state,
        logical,
        u64::from_le_bytes(inline.requested) as usize,
        u64::from_le_bytes(inline.extent) as usize,
        1,
    );
    match result {
        Ok(address) => {
            unsafe { out.write(address) };
            Ok(())
        }
        Err(error) => {
            state.handles.remove(&logical);
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
    let id = state.handles[&mapping.handle];
    unsafe { driver::cuMemUnmap(address, mapping.extent) }?;
    state.mappings.remove(&address);
    state.handles.remove(&mapping.handle);
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
    fn inline_ticket_rejects_foreign_handles_and_invalid_lengths() {
        assert!(InlineTicket::decode(unsafe { std::mem::zeroed() }).is_err());
        let mut ticket = InlineTicket {
            version: VERSION,
            creator: [1; 16],
            allocation: [2; 16],
            pid: 42u32.to_le_bytes(),
            reserved: [0; 4],
            requested: 17u64.to_le_bytes(),
            extent: 4096u64.to_le_bytes(),
        };
        let decoded = InlineTicket::decode(unsafe {
            std::mem::transmute::<InlineTicket, CUipcMemHandle>(ticket)
        })
        .expect("valid inline ticket");
        assert_eq!(decoded.creator, [1; 16]);
        assert_eq!(u64::from_le_bytes(decoded.requested), 17);
        ticket.extent = 16u64.to_le_bytes();
        assert!(
            InlineTicket::decode(unsafe {
                std::mem::transmute::<InlineTicket, CUipcMemHandle>(ticket)
            })
            .is_err()
        );
    }
}
