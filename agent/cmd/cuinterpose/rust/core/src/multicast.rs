// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Multicast objects wrap unicast members. This module owns their CUDA lifetime
//! and replay; common memory APIs share ProcessState's virtual allocation handles and VA ranges.

use super::host_carrier::Context;
use super::state::{self, Mapping, Phase, ProcessState, Resource, Result};
use super::virtual_shareable_handle;
use crate::driver::CudaError;
use crate::virtual_allocation_handle::{
    VIRTUAL_ALLOCATION_HANDLE_MASK, VIRTUAL_ALLOCATION_HANDLE_TAG,
};
use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_READY,
    CUDA_ERROR_NOT_SUPPORTED, CUDA_ERROR_OUT_OF_MEMORY, CUDA_SUCCESS,
};
use cudarc::driver::sys::{
    CUmemAllocationHandleType, CUmulticastGranularity_flags, CUmulticastObjectProp,
};
use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, BindingVersion, MemberRange, NamespacePid,
    Operation, Record,
};
use std::ffi::c_void;
use std::os::fd::{AsFd, AsRawFd};
use std::sync::MutexGuard;
use std::sync::atomic::Ordering;

#[derive(Clone)]
pub struct MulticastObject {
    pub reference: AllocationReference,
    pub properties: CUmulticastObjectProp,
    pub driver: Option<u64>,
    pub context: usize,
    pub shared: bool,
    pub checkpointed: bool,
    pub effective_size: usize,
    pub devices: Vec<i32>,
    pub bindings: Vec<Binding>,
    pub inflight: usize,
}

#[derive(Clone)]
pub struct Binding {
    source: BindingSource,
    offset: usize,
    size: usize,
    flags: u64,
    device: i32,
    version: BindingVersion,
    checkpointed: bool,
}

/// Fork during the unlocked CUDA call is outside the supported contract.
/// These counters exclude inspection/prepare and
/// prevent release/unmap of the object or member being used by that call.
struct Flight {
    object: Option<(AllocationId, u64)>,
    member: Option<AllocationId>,
}

impl Flight {
    fn begin(
        state: &mut ProcessState,
        object: Option<AllocationId>,
        member: Option<AllocationId>,
    ) -> Result<Self> {
        let object = if let Some(id) = object {
            let object = state
                .resources
                .get_mut(&id)
                .and_then(Resource::multicast_mut)
                .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
            let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
            object.inflight += 1;
            Some((id, driver))
        } else {
            None
        };
        if let Some(id) = member {
            state
                .resources
                .get_mut(&id)
                .and_then(Resource::unicast_mut)
                .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                .pins += 1;
        }
        state.inflight += 1;
        Ok(Self { object, member })
    }

    fn finish(self) -> Result<MutexGuard<'static, ProcessState>> {
        let mut state = state::get()?;
        state.inflight -= 1;
        if let Some(id) = self.member {
            state
                .resources
                .get_mut(&id)
                .and_then(Resource::unicast_mut)
                .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                .pins -= 1;
        }
        if let Some((id, driver)) = self.object {
            let object = state
                .resources
                .get_mut(&id)
                .and_then(Resource::multicast_mut)
                .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
            object.inflight -= 1;
            if object.driver != Some(driver) {
                super::G_FAILED.store(true, Ordering::Release);
                return Err(CudaError::from(CUDA_ERROR_NOT_READY));
            }
        }
        if state.phase != Phase::Active {
            super::G_FAILED.store(true, Ordering::Release);
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        Ok(state)
    }
}

pub fn cuMulticastCreate(out: *mut u64, properties: *const CUmulticastObjectProp) -> Result<()> {
    if out.is_null() || properties.is_null() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let properties = unsafe { *properties };
    let mut state = state::active()?;
    let id = state::random()?;
    let flight = Flight::begin(&mut state, None, None)?;
    drop(state);
    let mut driver = 0;
    let created = (|| -> Result<()> {
        let function = crate::driver::symbols::cuMulticastCreate()?;
        let result = unsafe { function(&mut driver, &properties) };
        if result != CUDA_SUCCESS {
            // Preserve output written by a failing driver, but leave it alone
            // if symbol resolution failed and no driver call took place.
            unsafe {
                out.write(driver);
            }
            return Err(result.into());
        }
        Ok(())
    })();
    let mut state = match flight.finish() {
        Ok(state) => state,
        Err(error) => {
            if created.is_ok() {
                unsafe { crate::driver::cuMemRelease(driver) }?;
            }
            return Err(error);
        }
    };
    created?;
    if driver & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
        unsafe { crate::driver::cuMemRelease(driver) }?;
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    if properties.handleTypes
        != u64::from(CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0)
    {
        if properties.handleTypes != 0 {
            state.unsupported += 1;
        }
        unsafe {
            out.write(driver);
        }
        return Ok(());
    }
    let virtual_multicast_handle = match state.mint_virtual_allocation_handle(id) {
        Ok(handle) => handle,
        Err(error) => {
            unsafe { crate::driver::cuMemRelease(driver) }?;
            return Err(error);
        }
    };
    let reference = AllocationReference {
        creator_pid: state.namespace_pid,
        id,
    };
    state.resources.insert(
        id,
        Resource::Multicast(MulticastObject {
            reference,
            properties,
            driver: Some(driver),
            context: state::context(),
            shared: false,
            checkpointed: false,
            effective_size: properties.size,
            devices: Vec::new(),
            bindings: Vec::new(),
            inflight: 0,
        }),
    );
    unsafe {
        out.write(virtual_multicast_handle);
    }
    Ok(())
}

pub fn cuMulticastAddDevice(handle: u64, device: i32) -> Result<()> {
    let mut state = state::active()?;
    let Some(id) = state.virtual_allocation_handles.get(&handle).copied() else {
        if handle & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        drop(state);
        unsafe { crate::driver::cuMulticastAddDevice(handle, device) }?;
        return Ok(());
    };
    let object = state
        .resources
        .get_mut(&id)
        .and_then(Resource::multicast_mut)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    object
        .devices
        .try_reserve(object.inflight + 1)
        .map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
    let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let flight = Flight::begin(&mut state, Some(id), None)?;
    drop(state);
    let added = (|| -> Result<()> {
        unsafe { crate::driver::cuMulticastAddDevice(driver, device) }?;
        Ok(())
    })();
    // AddDevice has no inverse. A successful call that cannot be recorded must
    // leave the generation poisoned; Flight::finish enforces this invariant.
    let mut state = flight.finish()?;
    added?;
    let object = state
        .resources
        .get_mut(&id)
        .and_then(Resource::multicast_mut)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    if !object.devices.contains(&device) {
        object.devices.push(device);
    }
    if object.context == 0 {
        object.context = state::context();
    }
    Ok(())
}

pub fn map(
    mut state: MutexGuard<'static, ProcessState>,
    id: AllocationId,
    address: u64,
    size: usize,
    offset: usize,
    flags: u64,
) -> Result<()> {
    let end = offset.checked_add(size).ok_or(CUDA_ERROR_INVALID_VALUE)?;
    if size == 0 || !state.covered(address, size)?.is_empty() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    state
        .pending_maps
        .try_reserve(1)
        .map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
    let driver = state
        .resources
        .get(&id)
        .and_then(Resource::multicast)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?
        .driver
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let flight = Flight::begin(&mut state, Some(id), None)?;
    state.pending_maps.push((address, size));
    drop(state);
    let mapped = (|| -> Result<()> {
        unsafe { crate::driver::cuMemMap(address, size, offset, driver, flags) }?;
        Ok(())
    })();
    let mut state = match flight.finish() {
        Ok(state) => state,
        Err(error) => {
            if mapped.is_ok() {
                unsafe { crate::driver::cuMemUnmap(address, size) }?;
            }
            return Err(error);
        }
    };
    state.pending_maps.retain(|range| *range != (address, size));
    mapped?;
    let object = state
        .resources
        .get_mut(&id)
        .and_then(Resource::multicast_mut)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    object.effective_size = object.effective_size.max(end);
    if object.context == 0 {
        object.context = state::context();
    }
    state.mappings.insert(
        address,
        Mapping {
            id,
            address,
            size,
            offset,
            flags,
            access: Vec::new(),
            unknown: false,
            checkpointed: false,
        },
    );
    Ok(())
}

pub fn import(
    mut state: MutexGuard<'static, ProcessState>,
    out: *mut u64,
    reference: AllocationReference,
    fd: std::os::fd::OwnedFd,
    properties: CUmulticastObjectProp,
) -> Result<()> {
    let id = reference.id;
    if state
        .resources
        .get(&id)
        .and_then(Resource::unicast)
        .is_some()
    {
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    if let Some(object) = state
        .resources
        .get_mut(&id)
        .and_then(Resource::multicast_mut)
    {
        if object.reference != reference {
            return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
        }
        object.shared = true;
        unsafe {
            out.write(state.mint_virtual_allocation_handle(id)?);
        }
        return Ok(());
    }
    let flight = Flight::begin(&mut state, None, None)?;
    drop(state);
    let mut driver = 0;
    let imported = (|| -> Result<()> {
        unsafe {
            crate::driver::cuMemImportFromShareableHandle(
                &mut driver,
                fd.as_raw_fd() as usize as *mut c_void,
                CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
            )
        }?;
        Ok(())
    })();
    let mut state = match flight.finish() {
        Ok(state) => state,
        Err(error) => {
            if imported.is_ok() {
                unsafe { crate::driver::cuMemRelease(driver) }?;
            }
            return Err(error);
        }
    };
    imported?;
    if driver & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
        unsafe { crate::driver::cuMemRelease(driver) }?;
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    // Another importer can have completed while this thread waited in CUDA.
    let inserted = if let Some(object) = state
        .resources
        .get_mut(&id)
        .and_then(Resource::multicast_mut)
    {
        unsafe { crate::driver::cuMemRelease(driver) }?;
        if object.reference != reference {
            return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
        }
        false
    } else {
        state.resources.insert(
            id,
            Resource::Multicast(MulticastObject {
                reference,
                properties,
                driver: Some(driver),
                context: state::context(),
                shared: true,
                checkpointed: false,
                effective_size: properties.size,
                devices: Vec::new(),
                bindings: Vec::new(),
                inflight: 0,
            }),
        );
        true
    };
    let virtual_multicast_handle = match state.mint_virtual_allocation_handle(id) {
        Ok(handle) => handle,
        Err(error) => {
            if inserted {
                state.resources.remove(&id);
                unsafe { crate::driver::cuMemRelease(driver) }?;
            }
            return Err(error);
        }
    };
    unsafe {
        out.write(virtual_multicast_handle);
    }
    Ok(())
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
enum BindInput {
    Memory { handle: u64, offset: usize },
    Address(u64),
}

fn bind(
    handle: u64,
    offset: usize,
    size: usize,
    flags: u64,
    mut device: i32,
    version: BindingVersion,
    input: BindInput,
) -> Result<()> {
    let mut state = state::active()?;
    let target = state.virtual_allocation_handles.get(&handle).copied();
    if target.is_none() && handle & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG
    {
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    let (source, member, member_driver) = match input {
        BindInput::Memory {
            handle: member_handle,
            offset: member_offset,
        } => {
            let member = state
                .virtual_allocation_handles
                .get(&member_handle)
                .copied()
                .filter(|id| {
                    state
                        .resources
                        .get(id)
                        .and_then(Resource::unicast)
                        .is_some()
                });
            if let Some(id) = member {
                let allocation = &state.resources[&id]
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
                if member_handle & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
                    return Err(CUDA_ERROR_INVALID_HANDLE.into());
                }
                if target.is_some() {
                    return Err(CUDA_ERROR_NOT_SUPPORTED.into());
                }
                // Pass-through has no replay record, so no synthetic allocation ID.
                drop(state);
                return unsafe {
                    match version {
                        BindingVersion::V1 => crate::driver::cuMulticastBindMem(
                            handle,
                            offset,
                            member_handle,
                            member_offset,
                            size,
                            flags,
                        ),
                        BindingVersion::V2 => crate::driver::cuMulticastBindMem_v2(
                            handle,
                            device,
                            offset,
                            member_handle,
                            member_offset,
                            size,
                            flags,
                        ),
                    }
                };
            }
        }
        BindInput::Address(address) => {
            let end = address
                .checked_add(size as u64)
                .ok_or(CUDA_ERROR_INVALID_VALUE)?;
            if target.is_some() {
                for mapping in state.mappings.values() {
                    if mapping.address < end
                        && mapping.address + mapping.size as u64 > address
                        && (address < mapping.address
                            || end > mapping.address + mapping.size as u64
                            || state
                                .resources
                                .get(&mapping.id)
                                .and_then(Resource::unicast)
                                .is_none())
                    {
                        return Err(CUDA_ERROR_INVALID_VALUE.into());
                    }
                }
                if state
                    .pending_maps
                    .iter()
                    .any(|&(base, length)| base < end && base + length as u64 > address)
                {
                    return Err(CUDA_ERROR_NOT_READY.into());
                }
            }
            let mapping = state.mappings.values().find(|mapping| {
                address >= mapping.address
                    && address - mapping.address < mapping.size as u64
                    && state
                        .resources
                        .get(&mapping.id)
                        .and_then(Resource::unicast)
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
                    device = state.resources[&mapping.id]
                        .unicast()
                        .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                        .properties
                        .location
                        .id;
                }
                Some(MemberRange {
                    allocation: state.resources[&mapping.id]
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
        checkpointed: false,
    };
    let Some(id) = target else {
        if handle & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
            return Err(CUDA_ERROR_INVALID_HANDLE.into());
        }
        drop(state);
        return binding.apply(handle, member_driver);
    };
    let end = binding
        .offset
        .checked_add(binding.size)
        .ok_or(CUDA_ERROR_INVALID_VALUE)?;
    if binding.size == 0 {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let object = state
        .resources
        .get_mut(&id)
        .and_then(Resource::multicast_mut)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    object
        .bindings
        .try_reserve(object.inflight + 1)
        .map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
    let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let flight = Flight::begin(&mut state, Some(id), member)?;
    drop(state);
    let bound = binding.apply(driver, member_driver);
    let mut state = match flight.finish() {
        Ok(state) => state,
        Err(error) => {
            if bound.is_ok() {
                unsafe {
                    crate::driver::cuMulticastUnbind(
                        driver,
                        binding.device,
                        binding.offset,
                        binding.size,
                    )
                }?;
            }
            return Err(error);
        }
    };
    bound?;
    if let Some(id) = member {
        state
            .resources
            .get_mut(&id)
            .and_then(Resource::unicast_mut)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?
            .shared = true;
    }
    let object = state
        .resources
        .get_mut(&id)
        .and_then(Resource::multicast_mut)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    object.effective_size = object.effective_size.max(end);
    object.bindings.push(binding);
    if object.context == 0 {
        object.context = state::context();
    }
    Ok(())
}

pub fn cuMulticastBindMem(
    handle: u64,
    offset: usize,
    member: u64,
    member_offset: usize,
    size: usize,
    flags: u64,
) -> Result<()> {
    bind(
        handle,
        offset,
        size,
        flags,
        0,
        BindingVersion::V1,
        BindInput::Memory {
            handle: member,
            offset: member_offset,
        },
    )
}

pub fn cuMulticastBindMem_v2(
    handle: u64,
    device: i32,
    offset: usize,
    member: u64,
    member_offset: usize,
    size: usize,
    flags: u64,
) -> Result<()> {
    bind(
        handle,
        offset,
        size,
        flags,
        device,
        BindingVersion::V2,
        BindInput::Memory {
            handle: member,
            offset: member_offset,
        },
    )
}

pub fn cuMulticastBindAddr(
    handle: u64,
    offset: usize,
    address: u64,
    size: usize,
    flags: u64,
) -> Result<()> {
    bind(
        handle,
        offset,
        size,
        flags,
        0,
        BindingVersion::V1,
        BindInput::Address(address),
    )
}

pub fn cuMulticastBindAddr_v2(
    handle: u64,
    device: i32,
    offset: usize,
    address: u64,
    size: usize,
    flags: u64,
) -> Result<()> {
    bind(
        handle,
        offset,
        size,
        flags,
        device,
        BindingVersion::V2,
        BindInput::Address(address),
    )
}

pub fn cuMulticastGetGranularity(
    out: *mut usize,
    properties: *const CUmulticastObjectProp,
    flags: CUmulticastGranularity_flags,
) -> Result<()> {
    unsafe { crate::driver::cuMulticastGetGranularity(out, properties, flags) }?;
    Ok(())
}

pub fn cuMulticastUnbind(handle: u64, device: i32, offset: usize, size: usize) -> Result<()> {
    let mut state = state::active()?;
    let Some(id) = state.virtual_allocation_handles.get(&handle).copied() else {
        if handle & VIRTUAL_ALLOCATION_HANDLE_MASK == VIRTUAL_ALLOCATION_HANDLE_TAG {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        drop(state);
        unsafe { crate::driver::cuMulticastUnbind(handle, device, offset, size) }?;
        return Ok(());
    };
    let object = state
        .resources
        .get_mut(&id)
        .and_then(Resource::multicast_mut)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    if object.inflight != 0 {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    let end = offset.checked_add(size).ok_or(CUDA_ERROR_INVALID_VALUE)?;
    for binding in &object.bindings {
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
            object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
            device,
            offset,
            size,
        )
    }?;
    object.bindings.retain(|binding| {
        binding.device != device || binding.offset >= end || binding.offset + binding.size <= offset
    });
    Ok(())
}

pub fn describe(state: &ProcessState, records: &mut Vec<Record>) -> Result<()> {
    for (id, resource) in &state.resources {
        let Resource::Multicast(object) = resource else {
            continue;
        };
        let virtual_multicast_handle_count = state
            .virtual_allocation_handles
            .values()
            .filter(|value| *value == id)
            .count()
            .try_into()
            .map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
        records.push(Record::Multicast {
            allocation: object.reference,
            devices: object.properties.numDevices,
            size: object.effective_size as u64,
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
            if mapping.unknown {
                return Err(CudaError::from(CUDA_ERROR_NOT_SUPPORTED));
            }
            let record = Record::MulticastMapping {
                allocation: object.reference,
                address: mapping.address,
                size: mapping.size as u64,
                offset: mapping.offset as u64,
                flags: mapping.flags,
                access: state::access_metadata(&mapping.access),
            };
            records.push(record);
        }
    }
    Ok(())
}

pub fn prepare(state: &mut ProcessState) -> Result<()> {
    for (id, resource) in &mut state.resources {
        let Resource::Multicast(object) = resource else {
            continue;
        };
        let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        state::cache()?.remove(id)?;
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
                mapping.checkpointed = true;
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
                binding.checkpointed = true;
            }
            unsafe { crate::driver::cuMemRelease(driver) }?;
            object.driver = None;
            object.checkpointed = true;
            Ok(())
        })?;
    }
    Ok(())
}

/// Restore collectives must not hold STATE either. The phase reserves the
/// entire lifecycle operation; CPU records remain private until the driver
/// work completes. Fork during lifecycle execution is unsupported.
pub fn restore_phase(
    mut state: MutexGuard<'static, ProcessState>,
    operation: Operation,
) -> Result<u64> {
    let next_phase = state.phase.next(operation)?;
    let mut objects = state
        .resources
        .iter()
        .filter_map(|(id, resource)| resource.multicast().map(|value| (id, value)))
        .filter(|(_, object)| object.checkpointed)
        .map(|(id, object)| (*id, object.clone()))
        .collect();
    let bindings = operation == Operation::RestoreMulticastBindings;
    let mut mappings = state
        .mappings
        .iter()
        .filter(|_| bindings)
        .map(|(address, mapping)| (*address, mapping.clone()))
        .collect();
    let allocations = state
        .resources
        .iter()
        .filter_map(|(id, resource)| resource.unicast().map(|value| (id, value)))
        .filter(|_| bindings)
        .map(|(id, allocation)| (*id, allocation.driver))
        .collect();
    let namespace_pid = state.namespace_pid;
    state.phase = Phase::ReconstructingMulticast;
    drop(state);
    let result = restore(
        &mut objects,
        &mut mappings,
        &allocations,
        namespace_pid,
        operation,
    );
    let mut state = state::get()?;
    state.resources.extend(
        objects
            .into_iter()
            .map(|(id, object)| (id, Resource::Multicast(object))),
    );
    state.mappings.extend(mappings);
    result?;
    state.phase = next_phase;
    Ok(0)
}

fn restore(
    objects: &mut std::collections::BTreeMap<AllocationId, MulticastObject>,
    mappings: &mut std::collections::BTreeMap<u64, Mapping>,
    allocations: &std::collections::BTreeMap<AllocationId, Option<u64>>,
    namespace_pid: NamespacePid,
    operation: Operation,
) -> Result<()> {
    for (id, object) in objects {
        if !object.checkpointed {
            continue;
        }
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
                        state::cache()?.insert(*id, fd, Some(object.properties))?;
                    }
                }
                Operation::RestoreMulticastImporters if !creator => {
                    let (fd, properties) =
                        virtual_shareable_handle::request_export(object.reference)
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
                        if !binding.checkpointed {
                            continue;
                        }
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
                                    .find(|mapping| {
                                        mapping.id == range.allocation.id && !mapping.checkpointed
                                    })
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
                        let bound =
                            binding.apply(object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?, member);
                        let released = (|| -> Result<()> {
                            if temporary {
                                unsafe { crate::driver::cuMemRelease(member) }?;
                            }
                            Ok(())
                        })();
                        bound?;
                        released?;
                        binding.checkpointed = false;
                    }
                    for mapping in mappings
                        .values_mut()
                        .filter(|mapping| mapping.id == *id && mapping.checkpointed)
                    {
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
                        mapping.checkpointed = false;
                    }
                    object.checkpointed = false;
                }
                Operation::RestoreMulticastCreators | Operation::RestoreMulticastImporters => {}
                _ => return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE)),
            }
            Ok(())
        })?;
    }
    Ok(())
}
