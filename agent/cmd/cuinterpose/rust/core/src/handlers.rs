// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! CUDA API policy and orchestration; memory modules own bookkeeping and rollback.

use crate::driver::{self};
use crate::driver::{CudaError, Result};
use crate::memory::{
    self, Resource, VIRTUAL_ALLOCATION_HANDLE_MASK, VIRTUAL_ALLOCATION_HANDLE_TAG,
};
use crate::memory::{checkpoint::Phase, sharing};
use crate::runtime;
use cudarc::driver::sys::CUresult::*;
use cudarc::driver::sys::*;
use runtime::get;
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
    let mut state = get()?;
    let supported = properties.requestedHandleTypes
        == CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
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
    if driver & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
        let _ = unsafe { crate::driver::cuMemRelease(driver) };
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    let handle = match reference {
        Some(reference) => state.adopt_unicast(reference, driver, size, properties, false)?,
        None => {
            if properties.requestedHandleTypes.0 != 0 {
                state.unsupported += 1;
            }
            driver
        }
    };
    unsafe { out.write(handle) };
    Ok(())
}

pub fn cuMemRelease(handle: u64) -> Result<()> {
    let mut state = get()?;
    if let Some(id) = state.virtual_allocation_handles.get(&handle)
        && (state.phase != Phase::Active || state.resources.get(id).is_some_and(Resource::busy))
    {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    if let Some(id) = state.virtual_allocation_handles.remove(&handle) {
        if let Err(error) = state.settle(id) {
            state.virtual_allocation_handles.insert(handle, id);
            return Err(error);
        }
    } else {
        if handle & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        unsafe { crate::driver::cuMemRelease(handle) }?;
        if let Some(count) = state.raw.get_mut(&handle) {
            *count -= 1;
            if *count == 0 {
                state.raw.remove(&handle);
            }
        }
    }
    Ok(())
}

pub fn cuMemRetainAllocationHandle(out: *mut u64, address: *mut c_void) -> Result<()> {
    if out.is_null() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let mut state = get()?;
    let id = state.mapped_resource(address as u64)?;
    if let Some(id) = id {
        if state.phase != Phase::Active {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        state.check_handle_capacity()?;
    }
    let mut driver = 0;
    unsafe { crate::driver::cuMemRetainAllocationHandle(&mut driver, address) }?;
    if let Some(id) = id {
        unsafe { out.write(state.retain_backing(id, driver)?) };
    } else {
        if driver & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
            let _ = unsafe { crate::driver::cuMemRelease(driver) };
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        unsafe {
            out.write(driver);
        }
    }
    Ok(())
}

pub fn cuMemMap(address: u64, size: usize, offset: usize, handle: u64, flags: u64) -> Result<()> {
    let mut state = get()?;
    let Some(id) = state.virtual_allocation_handles.get(&handle).copied() else {
        if handle & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        // Native handles must not overwrite tracked or pending ranges while
        // those mappings are temporarily absent from CUDA during checkpoint.
        if !state.covered(address, size)?.is_empty() {
            return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
        }
        unsafe { crate::driver::cuMemMap(address, size, offset, handle, flags) }?;
        return Ok(());
    };
    if state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }

    state.map_unicast(id, address, size, offset, flags)
}

pub fn cuMemUnmap(address: u64, size: usize) -> Result<()> {
    let mut state = get()?;
    let mappings = state.covered(address, size)?;
    if !mappings.is_empty() && state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    for base in &mappings {
        let id = &state.mappings[base].id;
        if state.resources.get(id).is_some_and(Resource::busy) {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
    }
    unsafe { crate::driver::cuMemUnmap(address, size) }?;
    for base in mappings {
        let mapping = state
            .mappings
            .remove(&base)
            .ok_or(CUDA_ERROR_INVALID_VALUE)?;
        state.settle(mapping.id)?;
    }
    Ok(())
}

pub fn cuMemSetAccess(
    address: u64,
    size: usize,
    access: *const CUmemAccessDesc,
    count: usize,
) -> Result<()> {
    let mut state = get()?;
    let mappings = state.covered(address, size)?;
    if !mappings.is_empty() && state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    if mappings.is_empty() || access.is_null() {
        unsafe { crate::driver::cuMemSetAccess(address, size, access, count) }?;
        return Ok(());
    }
    if count > isize::MAX as usize / size_of::<CUmemAccessDesc>() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let descriptors = unsafe { std::slice::from_raw_parts(access, count) };
    let merged = mappings
        .iter()
        .map(|base| state.mappings[base].merged_access(descriptors))
        .collect::<Result<Vec<_>>>()?;
    let result = unsafe { crate::driver::cuMemSetAccess(address, size, access, count) };
    for (base, entries) in mappings.iter().zip(merged) {
        let mapping = state
            .mappings
            .get_mut(base)
            .ok_or(CUDA_ERROR_INVALID_VALUE)?;
        if result.is_ok() {
            mapping.access = entries;
        } else {
            mapping.unknown = true;
        }
    }
    result
}

pub fn cuMemExportToShareableHandle(
    out: *mut c_void,
    handle: u64,
    kind: CUmemAllocationHandleType,
    flags: u64,
) -> Result<()> {
    let mut state = get()?;
    let Some(id) = state.virtual_allocation_handles.get(&handle).copied() else {
        if handle & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        unsafe { crate::driver::cuMemExportToShareableHandle(out, handle, kind, flags) }?;
        return Ok(());
    };
    if state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    if out.is_null()
        || kind != CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
        || flags != 0
    {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let namespace_pid = state.namespace_pid;
    let resource = state
        .resources
        .get_mut(&id)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    if let Resource::Unicast(allocation) = resource
        && allocation.properties.requestedHandleTypes.0 & kind.0 == 0
    {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let fd = sharing::create(resource.reference()).map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
    resource.share(namespace_pid)?;
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
    let reference = if kind == CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR {
        sharing::decode(fd as isize as i32).map_err(|_| CUDA_ERROR_INVALID_HANDLE)?
    } else {
        None
    };
    let mut state = get()?;
    let Some(reference) = reference else {
        let mut driver = 0;
        unsafe { crate::driver::cuMemImportFromShareableHandle(&mut driver, fd, kind) }?;
        if driver & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
            let _ = unsafe { crate::driver::cuMemRelease(driver) };
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        *state.raw.entry(driver).or_insert(0) += 1;
        unsafe {
            out.write(driver);
        }
        return Ok(());
    };
    let handle = sharing::import_reference(state, reference)?;
    unsafe { out.write(handle) };
    Ok(())
}

pub fn cuMemGetAllocationPropertiesFromHandle(
    out: *mut CUmemAllocationProp,
    handle: u64,
) -> Result<()> {
    let state = get()?;
    if state.virtual_allocation_handles.contains_key(&handle) && state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    let driver = match state.virtual_allocation_handles.get(&handle) {
        Some(id) => state
            .resources
            .get(id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?
            .driver()?,
        None if handle & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG => {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        None => handle,
    };
    unsafe { crate::driver::cuMemGetAllocationPropertiesFromHandle(out, driver) }?;
    if let Some(allocation) = state
        .virtual_allocation_handles
        .get(&handle)
        .and_then(|id| state.resources.get(id).and_then(Resource::unicast))
    {
        // Preserve driver-returned flags while hiding the internal POSIX
        // capability of an application-private allocation.
        unsafe {
            (*out).requestedHandleTypes = allocation.properties.requestedHandleTypes;
        }
    }
    Ok(())
}
