// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Multicast objects wrap unicast members. This module owns their CUDA lifetime
//! and replay; common memory APIs share State's logical handles and VA ranges.

use super::host_carrier::Context;
use super::state::{self, Mapping, Result, State, call};
use super::ticket;
use cuinterpose_abi::*;
use cuinterpose_protocol::{AllocationId, Record, RecordFlags, RecordKind, Ticket};
use std::ffi::c_void;
use std::os::fd::{AsRawFd, FromRawFd, IntoRawFd, OwnedFd};
use std::sync::MutexGuard;
use std::sync::atomic::Ordering;

#[derive(Clone)]
pub struct Object {
    pub ticket: Ticket,
    pub properties: MulticastProp,
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
    member: AllocationId,
    address: u64,
    offset: usize,
    member_offset: usize,
    size: usize,
    flags: u64,
    device: i32,
    kind: u8,
    version: u8,
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
            let object = state.multicasts.get_mut(&id).ok_or(INVALID_HANDLE)?;
            let driver = object.driver.ok_or(INVALID_HANDLE)?;
            object.inflight += 1;
            Some((id, driver))
        } else {
            None
        };
        if let Some(id) = member {
            state.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?.pins += 1;
        }
        state.inflight += 1;
        Ok(Self { object, member })
    }

    fn finish(self) -> Result<MutexGuard<'static, State>> {
        let mut state = state::get()?;
        state.inflight -= 1;
        if let Some(id) = self.member {
            state.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?.pins -= 1;
        }
        if let Some((id, driver)) = self.object {
            let object = state.multicasts.get_mut(&id).ok_or(INVALID_HANDLE)?;
            object.inflight -= 1;
            if object.driver != Some(driver) {
                super::FAILED.store(true, Ordering::Release);
                return Err(NOT_READY);
            }
        }
        if state.phase != 0 {
            super::FAILED.store(true, Ordering::Release);
            return Err(NOT_READY);
        }
        Ok(state)
    }
}

pub fn cuMulticastCreate(out: *mut u64, properties: *const MulticastProp) -> Result<i32> {
    if out.is_null() || properties.is_null() {
        return Err(INVALID_VALUE);
    }
    let properties = unsafe { *properties };
    let mut state = state::active()?;
    let id = state::random()?;
    let flight = Flight::begin(&mut state, None, None)?;
    drop(state);
    let mut driver = 0;
    let created = (|| -> Result<()> {
        let address = super::driver(c"cuMulticastCreate");
        if address.is_null() {
            return Err(NOT_INITIALIZED);
        }
        let function: unsafe extern "C" fn(*mut u64, *const MulticastProp) -> i32 =
            unsafe { std::mem::transmute(address) };
        let result = unsafe { function(&mut driver, &properties) };
        if result != SUCCESS {
            // Preserve output written by a failing driver, but leave it alone
            // if symbol resolution failed and no driver call took place.
            unsafe {
                out.write(driver);
            }
            return Err(result);
        }
        Ok(())
    })();
    let mut state = match flight.finish() {
        Ok(state) => state,
        Err(error) => {
            if created.is_ok() {
                call!("cuMemRelease", fn(u64), driver);
            }
            return Err(error);
        }
    };
    created?;
    if driver & HANDLE_MASK == HANDLE_TAG {
        call!("cuMemRelease", fn(u64), driver);
        return Err(INVALID_HANDLE);
    }
    if properties.handle_types != 1 {
        if properties.handle_types != 0 {
            state.unsupported += 1;
        }
        unsafe {
            out.write(driver);
        }
        return Ok(SUCCESS);
    }
    let logical = match state.mint(id) {
        Ok(handle) => handle,
        Err(error) => {
            call!("cuMemRelease", fn(u64), driver);
            return Err(error);
        }
    };
    let ticket = Ticket {
        creator: state.identity,
        endpoint: state.endpoint.clone(),
        allocation: id,
        resource: 2,
        devices: properties.devices,
        size: properties.size as u64,
        handle_types: properties.handle_types,
        flags: properties.flags,
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
    Ok(SUCCESS)
}

pub fn cuMulticastAddDevice(handle: u64, device: i32) -> Result<i32> {
    let mut state = state::active()?;
    let Some(id) = state.handles.get(&handle).copied() else {
        if handle & HANDLE_MASK == HANDLE_TAG {
            return Err(INVALID_HANDLE);
        }
        drop(state);
        call!("cuMulticastAddDevice", fn(u64, i32), handle, device);
        return Ok(SUCCESS);
    };
    let object = state.multicasts.get_mut(&id).ok_or(INVALID_HANDLE)?;
    object
        .devices
        .try_reserve(object.inflight + 1)
        .map_err(|_| OUT_OF_MEMORY)?;
    let driver = object.driver.ok_or(INVALID_HANDLE)?;
    let flight = Flight::begin(&mut state, Some(id), None)?;
    drop(state);
    let added = (|| -> Result<()> {
        call!("cuMulticastAddDevice", fn(u64, i32), driver, device);
        Ok(())
    })();
    // AddDevice has no inverse. A successful call that cannot be recorded must
    // leave the generation poisoned; Flight::finish enforces this invariant.
    let mut state = flight.finish()?;
    added?;
    let object = state.multicasts.get_mut(&id).ok_or(INVALID_HANDLE)?;
    if !object.devices.contains(&device) {
        object.devices.push(device);
    }
    if object.context == 0 {
        object.context = state::context();
    }
    Ok(SUCCESS)
}

pub fn map(
    mut state: MutexGuard<'static, State>,
    id: AllocationId,
    address: u64,
    size: usize,
    offset: usize,
    flags: u64,
) -> Result<i32> {
    let end = offset.checked_add(size).ok_or(INVALID_VALUE)?;
    if size == 0 || !state.covered(address, size)?.is_empty() {
        return Err(INVALID_VALUE);
    }
    state
        .pending_maps
        .try_reserve(1)
        .map_err(|_| OUT_OF_MEMORY)?;
    let driver = state
        .multicasts
        .get(&id)
        .ok_or(INVALID_HANDLE)?
        .driver
        .ok_or(INVALID_HANDLE)?;
    let flight = Flight::begin(&mut state, Some(id), None)?;
    state.pending_maps.push((address, size));
    drop(state);
    let mapped = (|| -> Result<()> {
        call!(
            "cuMemMap",
            fn(u64, usize, usize, u64, u64),
            address,
            size,
            offset,
            driver,
            flags
        );
        Ok(())
    })();
    let mut state = match flight.finish() {
        Ok(state) => state,
        Err(error) => {
            if mapped.is_ok() {
                call!("cuMemUnmap", fn(u64, usize), address, size);
            }
            return Err(error);
        }
    };
    state.pending_maps.retain(|range| *range != (address, size));
    mapped?;
    let object = state.multicasts.get_mut(&id).ok_or(INVALID_HANDLE)?;
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
    Ok(SUCCESS)
}

pub fn settle(state: &mut State, id: AllocationId) -> Result<()> {
    if state.handles.values().any(|value| *value == id)
        || state.mappings.values().any(|mapping| mapping.id == id)
    {
        return Ok(());
    }
    let object = state.multicasts.get(&id).ok_or(INVALID_HANDLE)?;
    if object.inflight != 0 || object.checkpointed {
        return Err(NOT_READY);
    }
    state::cache()?.replace((2, id), None)?;
    if let Some(driver) = object.driver {
        call!("cuMemRelease", fn(u64), driver);
    }
    state.multicasts.remove(&id);
    Ok(())
}

pub fn export(state: &mut State, id: AllocationId, out: *mut c_void) -> Result<i32> {
    let object = state.multicasts.get_mut(&id).ok_or(INVALID_HANDLE)?;
    if object.creator && !state::cache()?.contains(&(2, id))? {
        let mut fd = -1;
        call!(
            "cuMemExportToShareableHandle",
            fn(*mut c_void, u64, u32, u64),
            (&mut fd as *mut i32).cast(),
            object.driver.ok_or(INVALID_HANDLE)?,
            1,
            0
        );
        if fd < 0 {
            return Err(INVALID_HANDLE);
        }
        state::cache()?.replace((2, id), Some(unsafe { OwnedFd::from_raw_fd(fd) }))?;
    }
    let fd = ticket::export(&object.ticket).map_err(|_| OUT_OF_MEMORY)?;
    object.shared = true;
    unsafe {
        out.cast::<i32>().write(fd.into_raw_fd());
    }
    Ok(SUCCESS)
}

pub fn import(mut state: MutexGuard<'static, State>, out: *mut u64, ticket: Ticket) -> Result<i32> {
    let id = ticket.allocation;
    if state.allocations.contains_key(&id) {
        return Err(INVALID_HANDLE);
    }
    if let Some(object) = state.multicasts.get_mut(&id) {
        if object.ticket != ticket {
            return Err(INVALID_VALUE);
        }
        object.shared = true;
        unsafe {
            out.write(state.mint(id)?);
        }
        return Ok(SUCCESS);
    }
    let flight = Flight::begin(&mut state, None, None)?;
    drop(state);
    let mut driver = 0;
    let imported = (|| -> Result<()> {
        let fd = ticket::request(&ticket).map_err(|_| INVALID_HANDLE)?;
        call!(
            "cuMemImportFromShareableHandle",
            fn(*mut u64, *mut c_void, u32),
            &mut driver,
            fd.as_raw_fd() as usize as *mut c_void,
            1
        );
        Ok(())
    })();
    let mut state = match flight.finish() {
        Ok(state) => state,
        Err(error) => {
            if imported.is_ok() {
                call!("cuMemRelease", fn(u64), driver);
            }
            return Err(error);
        }
    };
    imported?;
    if driver & HANDLE_MASK == HANDLE_TAG {
        call!("cuMemRelease", fn(u64), driver);
        return Err(INVALID_HANDLE);
    }
    // Another importer can have completed while this thread waited in CUDA.
    let inserted = if let Some(object) = state.multicasts.get_mut(&id) {
        call!("cuMemRelease", fn(u64), driver);
        if object.ticket != ticket {
            return Err(INVALID_VALUE);
        }
        false
    } else {
        let properties = MulticastProp {
            devices: ticket.devices,
            size: ticket.size as usize,
            handle_types: ticket.handle_types,
            flags: ticket.flags,
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
                call!("cuMemRelease", fn(u64), driver);
            }
            return Err(error);
        }
    };
    unsafe {
        out.write(logical);
    }
    Ok(SUCCESS)
}

impl Binding {
    fn apply(&self, driver: u64, member: u64) -> Result<()> {
        match (self.kind, self.version) {
            (1, 1) => call!(
                "cuMulticastBindMem",
                fn(u64, usize, u64, usize, usize, u64),
                driver,
                self.offset,
                member,
                self.member_offset,
                self.size,
                self.flags
            ),
            (1, 2) => call!(
                "cuMulticastBindMem_v2",
                fn(u64, i32, usize, u64, usize, usize, u64),
                driver,
                self.device,
                self.offset,
                member,
                self.member_offset,
                self.size,
                self.flags
            ),
            (2, 1) => call!(
                "cuMulticastBindAddr",
                fn(u64, usize, u64, usize, u64),
                driver,
                self.offset,
                self.address,
                self.size,
                self.flags
            ),
            (2, 2) => call!(
                "cuMulticastBindAddr_v2",
                fn(u64, i32, usize, u64, usize, u64),
                driver,
                self.device,
                self.offset,
                self.address,
                self.size,
                self.flags
            ),
            _ => return Err(INVALID_VALUE),
        }
        Ok(())
    }
}

fn bind(handle: u64, mut binding: Binding, member_handle: u64) -> Result<i32> {
    let mut state = state::active()?;
    let target = state.handles.get(&handle).copied();
    if binding.kind == 2 && target.is_some() {
        let end = binding
            .address
            .checked_add(binding.size as u64)
            .ok_or(INVALID_VALUE)?;
        // A native BindAddr is allowed, but an address range that intersects
        // tracked memory must be wholly contained in one unicast mapping.
        for mapping in state.mappings.values() {
            if mapping.address < end
                && mapping.address + mapping.size as u64 > binding.address
                && (binding.address < mapping.address
                    || end > mapping.address + mapping.size as u64
                    || !state.allocations.contains_key(&mapping.id))
            {
                return Err(INVALID_VALUE);
            }
        }
        for &(address, size) in &state.pending_maps {
            if address < end && address + size as u64 > binding.address {
                return Err(NOT_READY);
            }
        }
    }
    let member = if binding.kind == 1 {
        state
            .handles
            .get(&member_handle)
            .copied()
            .filter(|id| state.allocations.contains_key(id))
    } else {
        state
            .mappings
            .values()
            .find(|mapping| {
                binding.address >= mapping.address
                    && binding.address - mapping.address < mapping.size as u64
                    && state.allocations.contains_key(&mapping.id)
            })
            .map(|mapping| mapping.id)
    };
    let mut member_driver = member_handle;
    if let Some(id) = member {
        let allocation = &state.allocations[&id];
        // BindAddr may refer to a mapping whose logical handles were released.
        // Its driver handle is not an argument to that CUDA operation.
        if binding.kind == 1 {
            member_driver = allocation.driver.ok_or(INVALID_HANDLE)?;
        }
        binding.member = id;
        if binding.version == 1 {
            binding.device = allocation.properties.location.id;
        }
        if binding.kind == 1 {
            let end = binding
                .member_offset
                .checked_add(binding.size)
                .ok_or(INVALID_VALUE)?;
            if allocation.size != 0 && end > allocation.size {
                return Err(INVALID_VALUE);
            }
        } else {
            let mapping = state
                .mappings
                .values()
                .find(|mapping| {
                    mapping.id == id
                        && binding.address >= mapping.address
                        && binding.address - mapping.address < mapping.size as u64
                })
                .ok_or(INVALID_VALUE)?;
            let displacement = (binding.address - mapping.address) as usize;
            if displacement
                .checked_add(binding.size)
                .ok_or(INVALID_VALUE)?
                > mapping.size
            {
                return Err(INVALID_VALUE);
            }
            binding.member_offset = mapping
                .offset
                .checked_add(displacement)
                .ok_or(INVALID_VALUE)?;
        }
    } else if binding.kind == 2 {
        binding.member = state::random()?;
        if binding.version == 1 {
            call!("cuCtxGetDevice", fn(*mut i32), &mut binding.device);
        }
    }
    let Some(id) = target else {
        if handle & HANDLE_MASK == HANDLE_TAG {
            return Err(INVALID_HANDLE);
        }
        if member_handle & HANDLE_MASK == HANDLE_TAG && member.is_none() && binding.kind == 1 {
            return Err(INVALID_HANDLE);
        }
        drop(state);
        binding.apply(handle, member_driver)?;
        return Ok(SUCCESS);
    };
    if binding.kind == 1 && member.is_none() {
        return Err(NOT_SUPPORTED);
    }
    let end = binding
        .offset
        .checked_add(binding.size)
        .ok_or(INVALID_VALUE)?;
    if binding.size == 0 {
        return Err(INVALID_VALUE);
    }
    let object = state.multicasts.get_mut(&id).ok_or(INVALID_HANDLE)?;
    object
        .bindings
        .try_reserve(object.inflight + 1)
        .map_err(|_| OUT_OF_MEMORY)?;
    let driver = object.driver.ok_or(INVALID_HANDLE)?;
    let flight = Flight::begin(&mut state, Some(id), member)?;
    drop(state);
    let bound = binding.apply(driver, member_driver);
    let mut state = match flight.finish() {
        Ok(state) => state,
        Err(error) => {
            if bound.is_ok() {
                call!(
                    "cuMulticastUnbind",
                    fn(u64, i32, usize, usize),
                    driver,
                    binding.device,
                    binding.offset,
                    binding.size
                );
            }
            return Err(error);
        }
    };
    bound?;
    if let Some(id) = member {
        state.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?.shared = true;
    }
    let object = state.multicasts.get_mut(&id).ok_or(INVALID_HANDLE)?;
    object.effective_size = object.effective_size.max(end);
    object.bindings.push(binding);
    if object.context == 0 {
        object.context = state::context();
    }
    Ok(SUCCESS)
}

pub fn cuMulticastBindMem(
    handle: u64,
    offset: usize,
    member: u64,
    member_offset: usize,
    size: usize,
    flags: u64,
) -> Result<i32> {
    bind(
        handle,
        Binding {
            member: [0; 16],
            address: 0,
            offset,
            member_offset,
            size,
            flags,
            device: 0,
            kind: 1,
            version: 1,
            checkpointed: false,
        },
        member,
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
) -> Result<i32> {
    bind(
        handle,
        Binding {
            member: [0; 16],
            address: 0,
            offset,
            member_offset,
            size,
            flags,
            device,
            kind: 1,
            version: 2,
            checkpointed: false,
        },
        member,
    )
}

pub fn cuMulticastBindAddr(
    handle: u64,
    offset: usize,
    address: u64,
    size: usize,
    flags: u64,
) -> Result<i32> {
    bind(
        handle,
        Binding {
            member: [0; 16],
            address,
            offset,
            member_offset: 0,
            size,
            flags,
            device: 0,
            kind: 2,
            version: 1,
            checkpointed: false,
        },
        0,
    )
}

pub fn cuMulticastBindAddr_v2(
    handle: u64,
    device: i32,
    offset: usize,
    address: u64,
    size: usize,
    flags: u64,
) -> Result<i32> {
    bind(
        handle,
        Binding {
            member: [0; 16],
            address,
            offset,
            member_offset: 0,
            size,
            flags,
            device,
            kind: 2,
            version: 2,
            checkpointed: false,
        },
        0,
    )
}

pub fn cuMulticastGetGranularity(
    out: *mut usize,
    properties: *const MulticastProp,
    flags: u32,
) -> Result<i32> {
    call!(
        "cuMulticastGetGranularity",
        fn(*mut usize, *const MulticastProp, u32),
        out,
        properties,
        flags
    );
    Ok(SUCCESS)
}

pub fn cuMulticastUnbind(handle: u64, device: i32, offset: usize, size: usize) -> Result<i32> {
    let mut state = state::active()?;
    let Some(id) = state.handles.get(&handle).copied() else {
        if handle & HANDLE_MASK == HANDLE_TAG {
            return Err(INVALID_HANDLE);
        }
        drop(state);
        call!(
            "cuMulticastUnbind",
            fn(u64, i32, usize, usize),
            handle,
            device,
            offset,
            size
        );
        return Ok(SUCCESS);
    };
    let object = state.multicasts.get_mut(&id).ok_or(INVALID_HANDLE)?;
    if object.inflight != 0 {
        return Err(NOT_READY);
    }
    let end = offset.checked_add(size).ok_or(INVALID_VALUE)?;
    for binding in &object.bindings {
        if binding.device == device
            && binding.offset < end
            && binding.offset + binding.size > offset
            && (binding.offset < offset || binding.offset + binding.size > end)
        {
            return Err(INVALID_VALUE);
        }
    }
    call!(
        "cuMulticastUnbind",
        fn(u64, i32, usize, usize),
        object.driver.ok_or(INVALID_HANDLE)?,
        device,
        offset,
        size
    );
    object.bindings.retain(|binding| {
        binding.device != device || binding.offset >= end || binding.offset + binding.size <= offset
    });
    Ok(SUCCESS)
}

pub fn describe(state: &State, records: &mut Vec<Record>) -> Result<()> {
    for (id, object) in &state.multicasts {
        let handles = state.handles.values().filter(|value| *value == id).count() as u32;
        records.push(Record {
            kind: RecordKind::Multicast,
            flags: RecordFlags(u32::from(object.creator) | (u32::from(handles != 0) << 1)),
            allocation_id: *id,
            allocation_size: object.effective_size as u64,
            application_handle_count: handles,
            handle_types: object.properties.handle_types,
            object_flags: object.properties.flags,
            num_devices: object.properties.devices,
            creator_participant: object.ticket.creator,
            ..Record::default()
        });
        for device in &object.devices {
            records.push(Record {
                kind: RecordKind::MulticastDevice,
                allocation_id: *id,
                device: *device,
                ..Record::default()
            });
        }
        for binding in &object.bindings {
            records.push(Record {
                kind: RecordKind::MulticastBinding,
                allocation_id: *id,
                member_id: binding.member,
                address: binding.address,
                size: binding.size as u64,
                offset: binding.offset as u64,
                member_offset: binding.member_offset as u64,
                operation_flags: binding.flags,
                binding_kind: binding.kind,
                api_version: binding.version,
                device: binding.device,
                ..Record::default()
            });
        }
        for mapping in state.mappings.values().filter(|mapping| mapping.id == *id) {
            if mapping.unknown {
                return Err(NOT_SUPPORTED);
            }
            let mut record = Record {
                kind: RecordKind::MulticastMapping,
                allocation_id: *id,
                address: mapping.address,
                size: mapping.size as u64,
                offset: mapping.offset as u64,
                operation_flags: mapping.flags,
                access_count: mapping.access.len() as u32,
                ..Record::default()
            };
            for (slot, access) in record.access.iter_mut().zip(&mapping.access) {
                *slot = cuinterpose_protocol::Access {
                    location_type: access.location.kind,
                    location_id: access.location.id,
                    flags: u64::from(access.flags),
                };
            }
            records.push(record);
        }
    }
    Ok(())
}

pub fn prepare(state: &mut State) -> Result<()> {
    for (id, object) in &mut state.multicasts {
        let driver = object.driver.ok_or(INVALID_HANDLE)?;
        state::cache()?.replace((2, *id), None)?;
        let device = object
            .devices
            .first()
            .copied()
            .or_else(|| object.bindings.first().map(|b| b.device))
            .unwrap_or(0);
        let context = Context::enter(object.context, device)?;
        let prepared = (|| -> Result<()> {
            for mapping in state
                .mappings
                .values_mut()
                .filter(|mapping| mapping.id == *id)
            {
                call!("cuMemUnmap", fn(u64, usize), mapping.address, mapping.size);
                mapping.checkpointed = true;
            }
            for binding in &mut object.bindings {
                call!(
                    "cuMulticastUnbind",
                    fn(u64, i32, usize, usize),
                    driver,
                    binding.device,
                    binding.offset,
                    binding.size
                );
                binding.checkpointed = true;
            }
            call!("cuMemRelease", fn(u64), driver);
            object.driver = None;
            object.checkpointed = true;
            Ok(())
        })();
        let left = context.leave();
        prepared?;
        left?;
    }
    Ok(())
}

/// Restore collectives must not hold STATE either. The phase reserves the
/// entire lifecycle operation; CPU records remain private until the driver
/// work completes. Fork during lifecycle execution is unsupported.
pub fn restore_phase(mut state: MutexGuard<'static, State>, operation: u16) -> Result<u64> {
    let mut objects = state.multicasts.clone();
    let mut mappings = state.mappings.clone();
    let allocations = state.allocations.clone();
    state.phase = u16::MAX;
    drop(state);
    let result = restore(&mut objects, &mut mappings, &allocations, operation);
    let mut state = state::get()?;
    state.multicasts = objects;
    state.mappings = mappings;
    result?;
    state.phase = if operation == 12 { 0 } else { operation };
    Ok(0)
}

fn restore(
    objects: &mut std::collections::BTreeMap<AllocationId, Object>,
    mappings: &mut std::collections::BTreeMap<u64, Mapping>,
    allocations: &std::collections::BTreeMap<AllocationId, state::Allocation>,
    operation: u16,
) -> Result<()> {
    for (id, object) in objects {
        if !object.checkpointed {
            continue;
        }
        if (operation == 9 && !object.creator) || (operation == 10 && object.creator) {
            continue;
        }
        let device = object
            .devices
            .first()
            .copied()
            .or_else(|| object.bindings.first().map(|b| b.device))
            .unwrap_or(0);
        let context = Context::enter(object.context, device)?;
        let restored = (|| -> Result<()> {
            match operation {
                9 if object.creator => {
                    let mut driver = 0;
                    call!(
                        "cuMulticastCreate",
                        fn(*mut u64, *const MulticastProp),
                        &mut driver,
                        &object.properties
                    );
                    object.driver = Some(driver);
                    if object.shared {
                        let mut fd = -1;
                        call!(
                            "cuMemExportToShareableHandle",
                            fn(*mut c_void, u64, u32, u64),
                            (&mut fd as *mut i32).cast(),
                            driver,
                            1,
                            0
                        );
                        if fd < 0 {
                            return Err(INVALID_HANDLE);
                        }
                        state::cache()?
                            .replace((2, *id), Some(unsafe { OwnedFd::from_raw_fd(fd) }))?;
                    }
                }
                10 if !object.creator => {
                    let fd = ticket::request(&object.ticket).map_err(|_| INVALID_HANDLE)?;
                    let mut driver = 0;
                    call!(
                        "cuMemImportFromShareableHandle",
                        fn(*mut u64, *mut c_void, u32),
                        &mut driver,
                        fd.as_raw_fd() as usize as *mut c_void,
                        1
                    );
                    object.driver = Some(driver);
                }
                11 => {
                    for device in &object.devices {
                        call!(
                            "cuMulticastAddDevice",
                            fn(u64, i32),
                            object.driver.ok_or(INVALID_HANDLE)?,
                            *device
                        );
                    }
                }
                12 => {
                    for binding in &mut object.bindings {
                        if !binding.checkpointed {
                            continue;
                        }
                        let mut member = 0;
                        let mut temporary = false;
                        if binding.kind == 1 {
                            let allocation =
                                allocations.get(&binding.member).ok_or(INVALID_HANDLE)?;
                            if let Some(driver) = allocation.driver {
                                member = driver;
                            } else {
                                let mapping = mappings
                                    .values()
                                    .find(|mapping| {
                                        mapping.id == binding.member && !mapping.checkpointed
                                    })
                                    .ok_or(INVALID_HANDLE)?;
                                call!(
                                    "cuMemRetainAllocationHandle",
                                    fn(*mut u64, *mut c_void),
                                    &mut member,
                                    mapping.address as usize as *mut c_void
                                );
                                temporary = true;
                            }
                        }
                        let bound = binding.apply(object.driver.ok_or(INVALID_HANDLE)?, member);
                        let released = (|| -> Result<()> {
                            if temporary {
                                call!("cuMemRelease", fn(u64), member);
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
                        call!(
                            "cuMemMap",
                            fn(u64, usize, usize, u64, u64),
                            mapping.address,
                            mapping.size,
                            mapping.offset,
                            object.driver.ok_or(INVALID_HANDLE)?,
                            mapping.flags
                        );
                        if !mapping.access.is_empty() {
                            call!(
                                "cuMemSetAccess",
                                fn(u64, usize, *const Access, usize),
                                mapping.address,
                                mapping.size,
                                mapping.access.as_ptr(),
                                mapping.access.len()
                            );
                        }
                        mapping.checkpointed = false;
                    }
                    object.checkpointed = false;
                }
                9 | 10 => {}
                _ => return Err(INVALID_VALUE),
            }
            Ok(())
        })();
        let left = context.leave();
        restored?;
        left?;
    }
    Ok(())
}
