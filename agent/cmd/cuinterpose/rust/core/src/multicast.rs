// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Multicast objects wrap unicast members. This module owns their CUDA lifetime
//! and replay; common memory APIs share State's logical handles and VA ranges.

use super::host_carrier::Context;
use super::state::{self, Mapping, Phase, Result, State};
use super::ticket;
use crate::driver::CudaError;
use crate::logical_handle::{LOGICAL_HANDLE_MASK, LOGICAL_HANDLE_TAG};
use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_READY,
    CUDA_ERROR_NOT_SUPPORTED, CUDA_ERROR_OUT_OF_MEMORY, CUDA_SUCCESS,
};
use cudarc::driver::sys::{
    CUmemAllocationHandleType, CUmulticastGranularity_flags, CUmulticastObjectProp,
};
use cuinterpose_protocol::{
    AllocationId, BindingSource, BindingVersion, MemberRange, Operation, Record, Resource,
    ResourceKind, Ticket,
};
use std::ffi::c_void;
use std::os::fd::{AsFd, AsRawFd, IntoRawFd};
use std::sync::MutexGuard;
use std::sync::atomic::Ordering;

#[derive(Clone)]
pub struct Object {
    pub ticket: Ticket,
    pub properties: CUmulticastObjectProp,
    pub driver: Option<u64>,
    pub context: usize,
    pub creator: bool,
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
        state: &mut State,
        object: Option<AllocationId>,
        member: Option<AllocationId>,
    ) -> Result<Self> {
        let object = if let Some(id) = object {
            let object = state
                .multicasts
                .get_mut(&id)
                .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
            let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
            object.inflight += 1;
            Some((id, driver))
        } else {
            None
        };
        if let Some(id) = member {
            state
                .allocations
                .get_mut(&id)
                .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                .pins += 1;
        }
        state.inflight += 1;
        Ok(Self { object, member })
    }

    fn finish(self) -> Result<MutexGuard<'static, State>> {
        let mut state = state::get()?;
        state.inflight -= 1;
        if let Some(id) = self.member {
            state
                .allocations
                .get_mut(&id)
                .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                .pins -= 1;
        }
        if let Some((id, driver)) = self.object {
            let object = state
                .multicasts
                .get_mut(&id)
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
    let id = AllocationId(state::random()?);
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
    if driver & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
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
    let logical = match state.mint(id) {
        Ok(handle) => handle,
        Err(error) => {
            unsafe { crate::driver::cuMemRelease(driver) }?;
            return Err(error);
        }
    };
    let ticket = Ticket {
        creator: state.identity,
        endpoint: state.endpoint.clone(),
        allocation: id,
        resource: Resource::Multicast {
            devices: properties.numDevices,
            size: properties.size as u64,
            handle_types: properties.handleTypes,
            flags: properties.flags,
        },
    };
    state.multicasts.insert(
        id,
        Object {
            ticket,
            properties,
            driver: Some(driver),
            context: state::context(),
            creator: true,
            shared: false,
            checkpointed: false,
            effective_size: properties.size,
            devices: Vec::new(),
            bindings: Vec::new(),
            inflight: 0,
        },
    );
    unsafe {
        out.write(logical);
    }
    Ok(())
}

pub fn cuMulticastAddDevice(handle: u64, device: i32) -> Result<()> {
    let mut state = state::active()?;
    let Some(id) = state.handles.get(&handle).copied() else {
        if handle & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        drop(state);
        unsafe { crate::driver::cuMulticastAddDevice(handle, device) }?;
        return Ok(());
    };
    let object = state
        .multicasts
        .get_mut(&id)
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
        .multicasts
        .get_mut(&id)
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
    mut state: MutexGuard<'static, State>,
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
        .multicasts
        .get(&id)
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
        .multicasts
        .get_mut(&id)
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

pub fn settle(state: &mut State, id: AllocationId) -> Result<()> {
    if state.handles.values().any(|value| *value == id)
        || state.mappings.values().any(|mapping| mapping.id == id)
    {
        return Ok(());
    }
    let object = state.multicasts.get(&id).ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    if object.inflight != 0 || object.checkpointed {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    state::cache()?.replace((ResourceKind::Multicast, id), None)?;
    if let Some(driver) = object.driver {
        unsafe { crate::driver::cuMemRelease(driver) }?;
    }
    state.multicasts.remove(&id);
    Ok(())
}

pub fn export(state: &mut State, id: AllocationId, out: *mut c_void) -> Result<()> {
    let object = state
        .multicasts
        .get_mut(&id)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    if object.creator && !state::cache()?.contains(&(ResourceKind::Multicast, id))? {
        let fd = crate::driver::export_posix(object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?)?;
        state::cache()?.replace((ResourceKind::Multicast, id), Some(fd))?;
    }
    let fd = ticket::export(&object.ticket).map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
    object.shared = true;
    unsafe {
        out.cast::<i32>().write(fd.into_raw_fd());
    }
    Ok(())
}

pub fn import(mut state: MutexGuard<'static, State>, out: *mut u64, ticket: Ticket) -> Result<()> {
    let id = ticket.allocation;
    if state.allocations.contains_key(&id) {
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    if let Some(object) = state.multicasts.get_mut(&id) {
        if object.ticket != ticket {
            return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
        }
        object.shared = true;
        unsafe {
            out.write(state.mint(id)?);
        }
        return Ok(());
    }
    let flight = Flight::begin(&mut state, None, None)?;
    drop(state);
    let mut driver = 0;
    let imported = (|| -> Result<()> {
        let fd = ticket::request(&ticket).map_err(|_| CUDA_ERROR_INVALID_HANDLE)?;
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
    if driver & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
        unsafe { crate::driver::cuMemRelease(driver) }?;
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    // Another importer can have completed while this thread waited in CUDA.
    let inserted = if let Some(object) = state.multicasts.get_mut(&id) {
        unsafe { crate::driver::cuMemRelease(driver) }?;
        if object.ticket != ticket {
            return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
        }
        false
    } else {
        let Resource::Multicast {
            devices,
            size,
            handle_types,
            flags,
        } = ticket.resource
        else {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        };
        let properties = CUmulticastObjectProp {
            numDevices: devices,
            size: size as usize,
            handleTypes: handle_types,
            flags,
        };
        state.multicasts.insert(
            id,
            Object {
                ticket,
                properties,
                driver: Some(driver),
                context: state::context(),
                creator: false,
                shared: true,
                checkpointed: false,
                effective_size: properties.size,
                devices: Vec::new(),
                bindings: Vec::new(),
                inflight: 0,
            },
        );
        true
    };
    let logical = match state.mint(id) {
        Ok(handle) => handle,
        Err(error) => {
            if inserted {
                state.multicasts.remove(&id);
                unsafe { crate::driver::cuMemRelease(driver) }?;
            }
            return Err(error);
        }
    };
    unsafe {
        out.write(logical);
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
    let target = state.handles.get(&handle).copied();
    if target.is_none() && handle & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    let (source, member, member_driver) = match input {
        BindInput::Memory {
            handle: member_handle,
            offset: member_offset,
        } => {
            let member = state
                .handles
                .get(&member_handle)
                .copied()
                .filter(|id| state.allocations.contains_key(id));
            if let Some(id) = member {
                let allocation = &state.allocations[&id];
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
                        allocation: id,
                        offset: member_offset as u64,
                    }),
                    member,
                    allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                )
            } else {
                if member_handle & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
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
                            || !state.allocations.contains_key(&mapping.id))
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
                    && state.allocations.contains_key(&mapping.id)
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
                    device = state.allocations[&mapping.id].properties.location.id;
                }
                Some(MemberRange {
                    allocation: mapping.id,
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
                range.map(|r| r.allocation),
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
        if handle & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
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
        .multicasts
        .get_mut(&id)
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
            .allocations
            .get_mut(&id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?
            .shared = true;
    }
    let object = state
        .multicasts
        .get_mut(&id)
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
    let Some(id) = state.handles.get(&handle).copied() else {
        if handle & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        drop(state);
        unsafe { crate::driver::cuMulticastUnbind(handle, device, offset, size) }?;
        return Ok(());
    };
    let object = state
        .multicasts
        .get_mut(&id)
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

pub fn describe(state: &State, records: &mut Vec<Record>) -> Result<()> {
    for (id, object) in &state.multicasts {
        let handles = state.handles.values().filter(|value| *value == id).count() as u32;
        records.push(Record::Multicast {
            owned: object.creator,
            id: *id,
            size: object.effective_size as u64,
            handles,
            handle_types: object.properties.handleTypes,
            flags: object.properties.flags,
            devices: object.properties.numDevices,
            creator: object.ticket.creator,
        });
        for device in &object.devices {
            records.push(Record::MulticastDevice {
                id: *id,
                device: *device,
            });
        }
        for binding in &object.bindings {
            records.push(Record::MulticastBinding {
                id: *id,
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
            let mut access: Vec<_> = mapping
                .access
                .iter()
                .map(|access| cuinterpose_protocol::Access {
                    location_type: access.location.type_ as i32,
                    location_id: access.location.id,
                    flags: access.flags as u64,
                })
                .collect();
            access.sort();
            let record = Record::MulticastMapping {
                id: *id,
                address: mapping.address,
                size: mapping.size as u64,
                offset: mapping.offset as u64,
                flags: mapping.flags,
                access,
            };
            records.push(record);
        }
    }
    Ok(())
}

pub fn prepare(state: &mut State) -> Result<()> {
    for (id, object) in &mut state.multicasts {
        let driver = object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        state::cache()?.replace((ResourceKind::Multicast, *id), None)?;
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
pub fn restore_phase(mut state: MutexGuard<'static, State>, operation: Operation) -> Result<u64> {
    let next_phase = state.phase.next(operation)?;
    let mut objects = state
        .multicasts
        .iter()
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
        .allocations
        .iter()
        .filter(|_| bindings)
        .map(|(id, allocation)| (*id, allocation.driver))
        .collect();
    state.phase = Phase::ReconstructingMulticast;
    drop(state);
    let result = restore(&mut objects, &mut mappings, &allocations, operation);
    let mut state = state::get()?;
    state.multicasts.extend(objects);
    state.mappings.extend(mappings);
    result?;
    state.phase = next_phase;
    Ok(0)
}

fn restore(
    objects: &mut std::collections::BTreeMap<AllocationId, Object>,
    mappings: &mut std::collections::BTreeMap<u64, Mapping>,
    allocations: &std::collections::BTreeMap<AllocationId, Option<u64>>,
    operation: Operation,
) -> Result<()> {
    for (id, object) in objects {
        if !object.checkpointed {
            continue;
        }
        if (operation == Operation::RestoreMulticastCreators && !object.creator)
            || (operation == Operation::RestoreMulticastImporters && object.creator)
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
                Operation::RestoreMulticastCreators if object.creator => {
                    let mut driver = 0;
                    unsafe { crate::driver::cuMulticastCreate(&mut driver, &object.properties) }?;
                    object.driver = Some(driver);
                    if object.shared {
                        let fd = crate::driver::export_posix(driver)?;
                        state::cache()?.replace((ResourceKind::Multicast, *id), Some(fd))?;
                    }
                }
                Operation::RestoreMulticastImporters if !object.creator => {
                    let fd =
                        ticket::request(&object.ticket).map_err(|_| CUDA_ERROR_INVALID_HANDLE)?;
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
                                .get(&range.allocation)
                                .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                            if let Some(driver) = allocation {
                                member = *driver;
                            } else {
                                let mapping = mappings
                                    .values()
                                    .find(|mapping| {
                                        mapping.id == range.allocation && !mapping.checkpointed
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
