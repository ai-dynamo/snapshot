// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! CUDA API policy and orchestration; memory modules own bookkeeping.

use crate::driver::{self};
use crate::driver::{CudaError, Result};
use crate::memory::sharing;
use crate::memory::{self, Memblock, VirtualAllocationHandle};
use crate::runtime;
use cudarc::driver::sys::CUresult::*;
use cudarc::driver::sys::*;
use runtime::active;
use std::ffi::c_void;
use std::os::fd::IntoRawFd;

pub fn cuMemGetAllocationGranularity(
    out: *mut usize,
    prop: *const CUmemAllocationProp,
    flags: CUmemAllocationGranularity_flags,
) -> Result<()> {
    if prop.is_null() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    unsafe { crate::driver::cuMemGetAllocationGranularity(out, prop, flags) }
}

pub fn cuMemCreate(
    out: *mut u64,
    size: usize,
    prop: *const CUmemAllocationProp,
    flags: u64,
) -> Result<()> {
    if out.is_null() || prop.is_null() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let properties = unsafe { *prop };
    let mut state = active()?;
    let supported = properties.requestedHandleTypes
        == CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
    if !supported && properties.requestedHandleTypes.0 != 0 {
        return Err(CUDA_ERROR_NOT_SUPPORTED.into());
    }
    let reference = if supported {
        Some(state.new_reference()?)
    } else {
        None
    };
    let mut driver = 0;
    let create = crate::driver::symbols::cuMemCreate()?;
    if let Err(error) =
        crate::driver::CudaError::result(unsafe { create(&mut driver, size, &properties, flags) })
    {
        unsafe {
            out.write(driver);
        }
        return Err(error);
    }
    let driver = runtime::must_complete(VirtualAllocationHandle::from_driver(driver));
    let handle = match reference {
        Some(reference) => {
            runtime::must_complete(state.adopt_unicast(reference, driver, size, properties, false))
        }
        None => driver,
    };
    unsafe { out.write(handle) };
    Ok(())
}

pub fn cuMemRelease(handle: u64) -> Result<()> {
    let mut state = active()?;
    if let Some(handle) = VirtualAllocationHandle::from_raw(handle) {
        let id = handle.id(&state)?;
        state.virtual_allocation_handles.remove(&handle);
        runtime::must_complete(state.release_unused_memblock(id));
    } else {
        unsafe { crate::driver::cuMemRelease(handle) }?;
    }
    Ok(())
}

pub fn cuMemRetainAllocationHandle(out: *mut u64, address: *mut c_void) -> Result<()> {
    if out.is_null() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let mut state = active()?;
    let id = state.mapped_memblock(address as u64);
    if id.is_some() {
        state.check_handle_capacity()?;
    }
    let mut driver = 0;
    unsafe { crate::driver::cuMemRetainAllocationHandle(&mut driver, address) }?;
    if let Some(id) = id {
        unsafe { out.write(runtime::must_complete(state.retain_backing(id, driver))) };
    } else {
        let driver = runtime::must_complete(VirtualAllocationHandle::from_driver(driver));
        unsafe {
            out.write(driver);
        }
    }
    Ok(())
}

pub fn cuMemMap(address: u64, size: usize, offset: usize, handle: u64, flags: u64) -> Result<()> {
    let mut state = active()?;
    let Some(id) = state.resolve_virtual_handle(handle)? else {
        unsafe { crate::driver::cuMemMap(address, size, offset, handle, flags) }?;
        return Ok(());
    };

    state.map_unicast(id, address, size, offset, flags)
}

pub fn cuMemUnmap(address: u64, size: usize) -> Result<()> {
    let mut state = active()?;
    unsafe { crate::driver::cuMemUnmap(address, size) }?;
    // CUDA only unmaps whole mappings; a successful range can contain several.
    let addresses: Vec<_> = state
        .mappings
        .range(address..)
        .take_while(|(start, _)| **start - address < size as u64)
        .map(|(start, _)| *start)
        .collect();
    for start in addresses {
        let mapping = state.mappings.remove(&start).unwrap();
        runtime::must_complete(state.release_unused_memblock(mapping.id));
    }
    Ok(())
}

pub fn cuMemSetAccess(
    address: u64,
    size: usize,
    access: *const CUmemAccessDesc,
    count: usize,
) -> Result<()> {
    let mut state = active()?;
    if access.is_null() {
        unsafe { crate::driver::cuMemSetAccess(address, size, access, count) }?;
        return Ok(());
    }
    if count > isize::MAX as usize / size_of::<CUmemAccessDesc>() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let descriptors = unsafe { std::slice::from_raw_parts(access, count) };
    // Access applies to a fully mapped range, potentially spanning allocations.
    // Prepare metadata before CUDA and publish it only after the call succeeds.
    let updates: Vec<_> = state
        .mappings
        .range(address..)
        .take_while(|(start, _)| **start - address < size as u64)
        .map(|(start, mapping)| (*start, mapping.merged_access(descriptors)))
        .collect();
    unsafe { crate::driver::cuMemSetAccess(address, size, access, count) }?;
    for (start, access) in updates {
        state.mappings.get_mut(&start).unwrap().access = access;
    }
    Ok(())
}

pub fn cuMemExportToShareableHandle(
    out: *mut c_void,
    handle: u64,
    kind: CUmemAllocationHandleType,
    flags: u64,
) -> Result<()> {
    let mut state = active()?;
    let Some(id) = state.resolve_virtual_handle(handle)? else {
        unsafe { crate::driver::cuMemExportToShareableHandle(out, handle, kind, flags) }?;
        return Ok(());
    };
    if out.is_null()
        || kind != CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
        || flags != 0
    {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let namespace_pid = state.namespace_pid;
    let memblock = state
        .memblocks
        .get_mut(&id)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    if let Memblock::Unicast(allocation) = memblock
        && allocation.properties.requestedHandleTypes.0 & kind.0 == 0
    {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let fd = sharing::create(memblock.reference()).map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
    memblock.export(namespace_pid)?;
    unsafe { out.cast::<i32>().write(fd.into_raw_fd()) };
    Ok(())
}

pub fn cuMemImportFromShareableHandle(
    out: *mut u64,
    fd: *mut c_void,
    kind: CUmemAllocationHandleType,
) -> Result<()> {
    if out.is_null() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    if kind != CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR {
        return Err(CUDA_ERROR_NOT_SUPPORTED.into());
    }
    let reference = sharing::decode(fd as isize as i32)
        .map_err(|_| CUDA_ERROR_INVALID_HANDLE)?
        .ok_or(CUDA_ERROR_NOT_SUPPORTED)?;
    let state = active()?;
    let handle = sharing::import_reference(state, reference)?;
    unsafe { out.write(handle) };
    Ok(())
}

pub fn cuMemGetAllocationPropertiesFromHandle(
    out: *mut CUmemAllocationProp,
    handle: u64,
) -> Result<()> {
    let state = active()?;
    let driver = match VirtualAllocationHandle::from_raw(handle) {
        Some(handle) => handle.driver_handle(&state)?,
        None => handle,
    };
    unsafe { crate::driver::cuMemGetAllocationPropertiesFromHandle(out, driver) }?;
    if let Some(allocation) = state
        .resolve_virtual_handle(handle)?
        .and_then(|id| state.memblocks.get(&id).and_then(Memblock::unicast))
    {
        // Preserve driver-returned flags while hiding the internal POSIX
        // capability of an application-private allocation.
        unsafe {
            (*out).requestedHandleTypes = allocation.properties.requestedHandleTypes;
        }
    }
    Ok(())
}
