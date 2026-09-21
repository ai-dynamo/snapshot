// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Multicast objects wrap unicast members. This module owns their CUDA lifetime
//! and replay; common memory APIs share ProcessState's virtual allocation handles and VA ranges.

use super::sharing;
use super::vmm::Mapping;
use super::{Memblock, ProcessState, VirtualAllocationHandle};
use crate::driver::Context;
use crate::driver::CudaError;
use crate::driver::Result;
use crate::runtime;
use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_SUPPORTED, CUDA_SUCCESS,
};
use cudarc::driver::sys::{CUmemAllocationHandleType, CUmulticastObjectProp};
use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, BindingVersion, MemberRange, Operation,
    Record,
};
use std::ffi::c_void;
use std::os::fd::{AsFd, AsRawFd};
use std::sync::MutexGuard;

#[derive(Clone)]
pub struct MulticastObject {
    pub reference: AllocationReference,
    pub properties: CUmulticastObjectProp,
    pub driver: Option<u64>,
    pub context: usize,
    pub shared: bool,
    pub devices: Vec<i32>,
    pub bindings: Vec<Binding>,
}

#[derive(Clone)]
pub struct Binding {
    source: BindingSource,
    offset: usize,
    size: usize,
    flags: u64,
    device: i32,
    version: BindingVersion,
}

pub fn map(
    state: MutexGuard<'static, ProcessState>,
    id: AllocationId,
    handle: VirtualAllocationHandle,
    address: u64,
    size: usize,
    offset: usize,
    flags: u64,
) -> Result<()> {
    let object = state
        .memblocks
        .get(&id)
        .and_then(Memblock::multicast)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let context = if object.context == 0 {
        crate::driver::context()?
    } else {
        object.context
    };
    let (mut state, ()) = runtime::call_unlocked(state, || unsafe {
        crate::driver::cuMemMap(address, size, offset, driver, flags)
    })?;
    let object = runtime::must_complete(
        state
            .memblocks
            .get_mut(&id)
            .and_then(Memblock::multicast_mut)
            .ok_or(CUDA_ERROR_INVALID_HANDLE.into()),
    );
    if object.context == 0 {
        object.context = context;
    }
    state.mappings.insert(
        address,
        Mapping {
            id,
            handle,
            address,
            size,
            offset,
            flags,
            access: Vec::new(),
        },
    );
    Ok(())
}

pub fn import(
    mut state: MutexGuard<'static, ProcessState>,
    reference: AllocationReference,
    fd: std::os::fd::OwnedFd,
    properties: CUmulticastObjectProp,
) -> Result<(MutexGuard<'static, ProcessState>, u64)> {
    let id = reference.id;
    if state
        .memblocks
        .get(&id)
        .and_then(Memblock::unicast)
        .is_some()
    {
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    if let Some(object) = state
        .memblocks
        .get_mut(&id)
        .and_then(Memblock::multicast_mut)
    {
        if object.reference != reference {
            return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
        }
        object.shared = true;
        let handle = state.mint_virtual_allocation_handle(id)?;
        return Ok((state, handle));
    }
    let mut driver = 0;
    let context = crate::driver::context()?;
    let (mut state, ()) = runtime::call_unlocked(state, || {
        unsafe {
            crate::driver::cuMemImportFromShareableHandle(
                &mut driver,
                fd.as_raw_fd() as usize as *mut c_void,
                CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
            )
        }?;
        Ok(())
    })?;
    let driver = runtime::must_complete(VirtualAllocationHandle::from_driver(driver));
    // Another importer can have completed while this thread waited in CUDA.
    if let Some(object) = state
        .memblocks
        .get_mut(&id)
        .and_then(Memblock::multicast_mut)
    {
        unsafe { crate::driver::cuMemRelease(driver) }?;
        if object.reference != reference {
            return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
        }
    } else {
        state.memblocks.insert(
            id,
            Memblock::Multicast(MulticastObject {
                reference,
                properties,
                driver: Some(driver),
                context,
                shared: true,
                devices: Vec::new(),
                bindings: Vec::new(),
            }),
        );
    }
    let virtual_multicast_handle = runtime::must_complete(state.mint_virtual_allocation_handle(id));
    Ok((state, virtual_multicast_handle))
}

impl Binding {
    fn apply(&self, group: u64, member: u64) -> Result<()> {
        use crate::driver;
        // v1/v2 differ in the explicit device argument, not the recorded source.
        unsafe {
            match (self.source, self.version) {
                (BindingSource::Memory(range), BindingVersion::V1) => driver::cuMulticastBindMem(
                    group,
                    self.offset,
                    member,
                    range.offset as usize,
                    self.size,
                    self.flags,
                ),
                (BindingSource::Memory(range), BindingVersion::V2) => {
                    driver::cuMulticastBindMem_v2(
                        group,
                        self.device,
                        self.offset,
                        member,
                        range.offset as usize,
                        self.size,
                        self.flags,
                    )
                }
                (BindingSource::Address { address, .. }, BindingVersion::V1) => {
                    driver::cuMulticastBindAddr(group, self.offset, address, self.size, self.flags)
                }
                (BindingSource::Address { address, .. }, BindingVersion::V2) => {
                    driver::cuMulticastBindAddr_v2(
                        group,
                        self.device,
                        self.offset,
                        address,
                        self.size,
                        self.flags,
                    )
                }
            }
        }
    }
}

// Application handles are resolved before constructing replay metadata.
pub(crate) enum BindInput {
    Memory { handle: u64, offset: usize },
    Address(u64),
}

pub(crate) fn bind(
    handle: u64,
    offset: usize,
    size: usize,
    flags: u64,
    mut device: i32,
    version: BindingVersion,
    input: BindInput,
) -> Result<()> {
    let mut state = runtime::active()?;
    let target = state
        .resolve_virtual_handle(handle)?
        .ok_or(CUDA_ERROR_NOT_SUPPORTED)?;
    let (source, member, member_driver) = match input {
        BindInput::Memory {
            handle: member_handle,
            offset: member_offset,
        } => {
            let member = match state.resolve_virtual_handle(member_handle)? {
                Some(id)
                    if state
                        .memblocks
                        .get(&id)
                        .and_then(Memblock::unicast)
                        .is_some() =>
                {
                    Some(id)
                }
                Some(_) => return Err(CUDA_ERROR_INVALID_HANDLE.into()),
                None => None,
            };
            if let Some(id) = member {
                let allocation = &state.memblocks[&id]
                    .unicast()
                    .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                if member_offset
                    .checked_add(size)
                    .is_none_or(|end| allocation.size != 0 && end > allocation.size)
                {
                    return Err(CUDA_ERROR_INVALID_VALUE.into());
                }
                if version == BindingVersion::V1 {
                    device = allocation.properties.location.id;
                }
                (
                    BindingSource::Memory(MemberRange {
                        allocation: allocation.reference,
                        offset: member_offset as u64,
                    }),
                    member,
                    allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                )
            } else {
                return Err(CUDA_ERROR_NOT_SUPPORTED.into());
            }
        }
        BindInput::Address(address) => {
            let end = address
                .checked_add(size as u64)
                .ok_or(CUDA_ERROR_INVALID_VALUE)?;
            {
                for mapping in state.mappings.values() {
                    if mapping.address < end
                        && mapping.address + mapping.size as u64 > address
                        && (address < mapping.address
                            || end > mapping.address + mapping.size as u64
                            || state
                                .memblocks
                                .get(&mapping.id)
                                .and_then(Memblock::unicast)
                                .is_none())
                    {
                        return Err(CUDA_ERROR_INVALID_VALUE.into());
                    }
                }
            }
            let mapping = state.mappings.values().find(|mapping| {
                address >= mapping.address
                    && address - mapping.address < mapping.size as u64
                    && state
                        .memblocks
                        .get(&mapping.id)
                        .and_then(Memblock::unicast)
                        .is_some()
            });
            let range = if let Some(mapping) = mapping {
                let displacement = address - mapping.address;
                if displacement
                    .checked_add(size as u64)
                    .is_none_or(|end| end > mapping.size as u64)
                {
                    return Err(CUDA_ERROR_INVALID_VALUE.into());
                }
                if version == BindingVersion::V1 {
                    device = state.memblocks[&mapping.id]
                        .unicast()
                        .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                        .properties
                        .location
                        .id;
                }
                Some(MemberRange {
                    allocation: state.memblocks[&mapping.id]
                        .unicast()
                        .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                        .reference,
                    offset: (mapping.offset as u64)
                        .checked_add(displacement)
                        .ok_or(CUDA_ERROR_INVALID_VALUE)?,
                })
            } else {
                if version == BindingVersion::V1 {
                    unsafe { crate::driver::cuCtxGetDevice(&mut device) }?;
                }
                None
            };
            (
                BindingSource::Address {
                    address,
                    tracked_member: range,
                },
                range.map(|r| r.allocation.id),
                0,
            )
        }
    };
    let binding = Binding {
        source,
        offset,
        size,
        flags,
        device,
        version,
    };
    let id = target;
    let object = state
        .memblocks
        .get_mut(&id)
        .and_then(Memblock::multicast_mut)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let context = if object.context == 0 {
        crate::driver::context()?
    } else {
        object.context
    };
    let (mut state, ()) = runtime::call_unlocked(state, || binding.apply(driver, member_driver))?;
    if let Some(id) = member {
        runtime::must_complete(
            state
                .memblocks
                .get_mut(&id)
                .and_then(Memblock::unicast_mut)
                .ok_or(CUDA_ERROR_INVALID_HANDLE.into()),
        )
        .shared = true;
    }
    let object = runtime::must_complete(
        state
            .memblocks
            .get_mut(&id)
            .and_then(Memblock::multicast_mut)
            .ok_or(CUDA_ERROR_INVALID_HANDLE.into()),
    );
    object.bindings.push(binding);
    if object.context == 0 {
        object.context = context;
    }
    Ok(())
}

pub fn describe(state: &ProcessState, records: &mut Vec<Record>) -> Result<()> {
    for (id, resource) in &state.memblocks {
        let Memblock::Multicast(object) = resource else {
            continue;
        };
        let virtual_multicast_handle_count = state
            .virtual_allocation_handles
            .values()
            .filter(|entry| entry.id == *id)
            .map(|entry| entry.references)
            .sum();
        records.push(Record::Multicast {
            allocation: object.reference,
            devices: object.properties.numDevices,
            size: object.properties.size as u64,
            handle_types: object.properties.handleTypes,
            flags: object.properties.flags,
            virtual_multicast_handle_count,
        });
        for device in &object.devices {
            records.push(Record::MulticastDevice {
                allocation: object.reference,
                device: *device,
            });
        }
        for binding in &object.bindings {
            records.push(Record::MulticastBinding {
                allocation: object.reference,
                source: binding.source,
                size: binding.size as u64,
                offset: binding.offset as u64,
                flags: binding.flags,
                version: binding.version,
                device: binding.device,
            });
        }
        for mapping in state.mappings.values().filter(|mapping| mapping.id == *id) {
            let record = Record::MulticastMapping {
                allocation: object.reference,
                address: mapping.address,
                size: mapping.size as u64,
                offset: mapping.offset as u64,
                flags: mapping.flags,
                access: super::vmm::access_metadata(&mapping.access),
            };
            records.push(record);
        }
    }
    Ok(())
}

pub fn prepare(state: &mut ProcessState) -> Result<()> {
    for (id, resource) in &mut state.memblocks {
        let Memblock::Multicast(object) = resource else {
            continue;
        };
        let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        runtime::export_cache()?.remove(id)?;
        let device = object
            .devices
            .first()
            .copied()
            .or_else(|| object.bindings.first().map(|b| b.device))
            .unwrap_or(0);
        Context::run(object.context, device, || {
            for mapping in state
                .mappings
                .values_mut()
                .filter(|mapping| mapping.id == *id)
            {
                unsafe { crate::driver::cuMemUnmap(mapping.address, mapping.size) }?;
            }
            for binding in &mut object.bindings {
                unsafe {
                    crate::driver::cuMulticastUnbind(
                        driver,
                        binding.device,
                        binding.offset,
                        binding.size,
                    )
                }?;
            }
            unsafe { crate::driver::cuMemRelease(driver) }?;
            object.driver = None;
            Ok(())
        })?;
    }
    Ok(())
}

/// Application threads remain parked throughout restore. Peer FD service uses
/// only the export cache, so reconstruction can hold the registry lock even
/// across driver calls that wait for other processes.
pub fn restore(state: &mut ProcessState, operation: Operation) -> Result<()> {
    let allocations: std::collections::BTreeMap<_, _> = state
        .memblocks
        .iter()
        .filter_map(|(id, memblock)| memblock.unicast().map(|a| (*id, a.driver)))
        .collect();
    let namespace_pid = state.namespace_pid;
    let mappings = &state.mappings;
    for (id, memblock) in &mut state.memblocks {
        let Memblock::Multicast(object) = memblock else {
            continue;
        };
        let creator = object.reference.creator_pid == namespace_pid;
        if (operation == Operation::RestoreMulticastCreators && !creator)
            || (operation == Operation::RestoreMulticastImporters && creator)
        {
            continue;
        }
        let device = object
            .devices
            .first()
            .copied()
            .or_else(|| object.bindings.first().map(|b| b.device))
            .unwrap_or(0);
        Context::run(object.context, device, || {
            match operation {
                Operation::RestoreMulticastCreators if creator => {
                    let mut driver = 0;
                    unsafe { crate::driver::cuMulticastCreate(&mut driver, &object.properties) }?;
                    object.driver = Some(driver);
                    if object.shared {
                        let fd = crate::driver::export_posix(driver)?;
                        runtime::export_cache()?.insert(*id, fd, Some(object.properties))?;
                    }
                }
                Operation::RestoreMulticastImporters if !creator => {
                    let (fd, properties) = sharing::request_export(object.reference)
                        .map_err(|_| CUDA_ERROR_INVALID_HANDLE)?;
                    if properties != Some(object.properties) {
                        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
                    }
                    let driver = crate::driver::import_posix(fd.as_fd())?;
                    object.driver = Some(driver);
                }
                Operation::RestoreMulticastDevices => {
                    for device in &object.devices {
                        unsafe {
                            crate::driver::cuMulticastAddDevice(
                                object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                                *device,
                            )
                        }?;
                    }
                }
                Operation::RestoreMulticastBindings => {
                    for binding in &mut object.bindings {
                        let mut member = 0;
                        let mut temporary = false;
                        if let BindingSource::Memory(range) = binding.source {
                            let allocation = allocations
                                .get(&range.allocation.id)
                                .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                            if let Some(driver) = allocation {
                                member = *driver;
                            } else {
                                let mapping = mappings
                                    .values()
                                    .find(|mapping| mapping.id == range.allocation.id)
                                    .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                                unsafe {
                                    crate::driver::cuMemRetainAllocationHandle(
                                        &mut member,
                                        mapping.address as usize as *mut c_void,
                                    )
                                }?;
                                temporary = true;
                            }
                        }
                        binding.apply(object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?, member)?;
                        if temporary {
                            unsafe { crate::driver::cuMemRelease(member) }?;
                        }
                    }
                    for mapping in mappings.values().filter(|mapping| mapping.id == *id) {
                        unsafe {
                            crate::driver::cuMemMap(
                                mapping.address,
                                mapping.size,
                                mapping.offset,
                                object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                                mapping.flags,
                            )
                        }?;
                        if !mapping.access.is_empty() {
                            unsafe {
                                crate::driver::cuMemSetAccess(
                                    mapping.address,
                                    mapping.size,
                                    mapping.access.as_ptr(),
                                    mapping.access.len(),
                                )
                            }?;
                        }
                    }
                }
                Operation::RestoreMulticastCreators | Operation::RestoreMulticastImporters => {}
                _ => return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE)),
            }
            Ok(())
        })?;
    }
    Ok(())
}
/// Create without holding the registry across a collective CUDA call.
/// Preserve the driver's error output at the ABI boundary.
pub(crate) fn create_backing(
    state: MutexGuard<'static, ProcessState>,
    properties: &CUmulticastObjectProp,
    out: *mut u64,
) -> Result<(MutexGuard<'static, ProcessState>, u64)> {
    runtime::call_unlocked(state, || {
        let mut driver = 0;
        let function = crate::driver::symbols::cuMulticastCreate()?;
        let result = unsafe { function(&mut driver, properties) };
        if result != CUDA_SUCCESS {
            // Preserve a failing driver's output without modifying it when
            // symbol resolution fails before the driver is called.
            unsafe {
                out.write(driver);
            }
            return Err(result.into());
        }
        Ok(driver)
    })
}

impl ProcessState {
    pub(crate) fn adopt_multicast(
        &mut self,
        id: AllocationId,
        driver: u64,
        properties: CUmulticastObjectProp,
        context: usize,
    ) -> Result<u64> {
        let virtual_multicast_handle = self.mint_virtual_allocation_handle(id)?;
        let reference = AllocationReference {
            creator_pid: self.namespace_pid,
            id,
        };
        self.memblocks.insert(
            id,
            Memblock::Multicast(MulticastObject {
                reference,
                properties,
                driver: Some(driver),
                context,
                shared: false,
                devices: Vec::new(),
                bindings: Vec::new(),
            }),
        );
        Ok(virtual_multicast_handle)
    }
}

pub(crate) fn add_device(
    mut state: MutexGuard<'static, ProcessState>,
    id: AllocationId,
    device: i32,
) -> Result<()> {
    let object = state
        .memblocks
        .get_mut(&id)
        .and_then(Memblock::multicast_mut)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let context = if object.context == 0 {
        crate::driver::context()?
    } else {
        object.context
    };
    let (mut state, ()) = runtime::call_unlocked(state, || unsafe {
        crate::driver::cuMulticastAddDevice(driver, device)
    })?;
    let object = runtime::must_complete(
        state
            .memblocks
            .get_mut(&id)
            .and_then(Memblock::multicast_mut)
            .ok_or(CUDA_ERROR_INVALID_HANDLE.into()),
    );
    if !object.devices.contains(&device) {
        object.devices.push(device);
    }
    if object.context == 0 {
        object.context = context;
    }
    Ok(())
}

impl MulticastObject {
    pub(crate) fn unbind(&mut self, device: i32, offset: usize, size: usize) -> Result<()> {
        let end = offset.checked_add(size).ok_or(CUDA_ERROR_INVALID_VALUE)?;
        for binding in &self.bindings {
            if binding.device == device
                && binding.offset < end
                && binding.offset + binding.size > offset
                && (binding.offset < offset || binding.offset + binding.size > end)
            {
                return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
            }
        }
        unsafe {
            crate::driver::cuMulticastUnbind(
                self.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                device,
                offset,
                size,
            )
        }?;
        self.bindings.retain(|binding| {
            binding.device != device
                || binding.offset >= end
                || binding.offset + binding.size <= offset
        });
        Ok(())
    }
}
