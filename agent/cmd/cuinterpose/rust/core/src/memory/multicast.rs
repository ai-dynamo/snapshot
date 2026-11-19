// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Manage multicast objects and their unicast members during allocation and restore.
//! Memory APIs share the virtual allocation handles and address ranges in ProcessState.

use super::sharing;
use super::vmm::Mapping;
use super::{Memblock, ProcessState, VirtualAllocationHandle};
use crate::driver::Context;
use crate::error::Error;
use crate::error::Result;
use crate::runtime;
use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_SUPPORTED,
};
use cudarc::driver::sys::{CUmemAllocationHandleType, CUmemLocation, CUmulticastObjectProp};
use cuinterpose_protocol::{
    AllocationId, AllocationReference, BindingSource, BindingVersion, FabricHandle, FabricKind,
    FabricReplacement, MemberRange, Operation, Record,
};
use std::ffi::c_void;
use std::os::fd::{AsFd, AsRawFd};
use std::sync::{Arc, Mutex, MutexGuard};

#[derive(Clone)]
pub struct MulticastObject {
    pub reference: AllocationReference,
    pub refcounts: super::Refcounts,
    pub properties: CUmulticastObjectProp,
    pub driver: Option<u64>,
    pub context: usize,
    pub shared: bool,
    pub devices: Vec<i32>,
    pub bindings: Vec<Binding>,
    // Clones share this lock so bind/unbind calls retain their order.
    pub(super) binding_lock: Arc<Mutex<()>>,
    /// The fabric handle that the creator exported, or that an importer imported.
    pub fabric: Option<FabricHandle>,
}

impl MulticastObject {
    /// An object imported from its creator, without devices or bindings yet.
    pub(crate) fn imported(
        reference: AllocationReference,
        properties: CUmulticastObjectProp,
        driver: u64,
        context: usize,
    ) -> Self {
        Self {
            reference,
            refcounts: Default::default(),
            properties,
            driver: Some(driver),
            context,
            shared: true,
            devices: Vec::new(),
            bindings: Vec::new(),
            binding_lock: Arc::default(),
            fabric: None,
        }
    }
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

/// Map a multicast object, or a FABRIC import of unknown kind, which may be one. CUDA can
/// wait for other processes while it maps a multicast object, so the registry is released
/// around the call.
pub fn map(
    state: MutexGuard<'static, ProcessState>,
    id: AllocationId,
    handle: VirtualAllocationHandle,
    address: u64,
    size: usize,
    offset: usize,
    flags: u64,
) -> Result<()> {
    let (driver, context) = multicast_driver(&state, id, None)?;
    let context = if context == 0 {
        crate::driver::context()?
    } else {
        context
    };
    let (mut state, ()) = runtime::call_unlocked(state, || unsafe {
        crate::driver::cuMemMap(address, size, offset, driver, flags)
    })?;
    // Another call may have classified an import meanwhile. The mapping is the same for
    // either kind.
    let memblock = runtime::must_complete(
        state
            .memblocks
            .get_mut(&id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE.into()),
    );
    let recorded = memblock.context_mut();
    if *recorded == 0 {
        *recorded = context;
    }
    memblock.refcounts_mut().mappings += 1;
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

/// The driver handle and context of `id` for a call that only a multicast object
/// accepts. A FABRIC import of unknown kind may be one, and the caller classifies it once
/// the driver accepts the call. A caller that holds a binding lock passes it, so that an
/// object removed and imported again under the same ID does not match.
fn multicast_driver(
    state: &ProcessState,
    id: AllocationId,
    binding_lock: Option<&Arc<Mutex<()>>>,
) -> Result<(u64, usize)> {
    let (driver, context, lock) = match state.memblocks.get(&id) {
        Some(Memblock::Multicast(object)) => (
            object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
            object.context,
            &object.binding_lock,
        ),
        Some(Memblock::Unclassified(import)) => {
            (import.driver, import.context, &import.binding_lock)
        }
        _ => return Err(CUDA_ERROR_INVALID_HANDLE.into()),
    };
    if binding_lock.is_some_and(|held| !Arc::ptr_eq(held, lock)) {
        return Err(CUDA_ERROR_INVALID_HANDLE.into());
    }
    Ok((driver, context))
}

/// The lock that orders bind and unbind calls on `id`, which `multicast_driver` accepts.
fn binding_lock(state: &ProcessState, id: AllocationId) -> Result<Arc<Mutex<()>>> {
    match state.memblocks.get(&id) {
        Some(Memblock::Multicast(object)) => Ok(Arc::clone(&object.binding_lock)),
        Some(Memblock::Unclassified(import)) => Ok(Arc::clone(&import.binding_lock)),
        _ => Err(CUDA_ERROR_INVALID_HANDLE.into()),
    }
}

/// What a binding needs of its member: memory, or a FABRIC import of unknown kind, which
/// is memory if the driver binds it. An import does not know its size, so it reports 0.
struct Member {
    reference: AllocationReference,
    size: usize,
    location: CUmemLocation,
    driver: Option<u64>,
}

impl Member {
    fn of(memblock: &Memblock) -> Option<Self> {
        match memblock {
            Memblock::Unicast(allocation) => Some(Self {
                reference: allocation.reference,
                size: allocation.size,
                location: allocation.properties.location,
                driver: allocation.driver,
            }),
            Memblock::Unclassified(import) => Some(Self {
                reference: import.reference,
                size: 0,
                location: import.location,
                driver: Some(import.driver),
            }),
            Memblock::Multicast(_) => None,
        }
    }
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
        return Err(Error::from(CUDA_ERROR_INVALID_HANDLE));
    }
    if let Some(object) = state
        .memblocks
        .get_mut(&id)
        .and_then(Memblock::multicast_mut)
    {
        if object.reference != reference {
            return Err(Error::from(CUDA_ERROR_INVALID_VALUE));
        }
        object.shared = true;
        let handle = state.mint_virtual_allocation_handle(id)?;
        state
            .memblocks
            .get_mut(&id)
            .unwrap()
            .refcounts_mut()
            .handle_entries += 1;
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
    // Another importer may have completed while this thread waited in CUDA.
    if let Some(object) = state
        .memblocks
        .get_mut(&id)
        .and_then(Memblock::multicast_mut)
    {
        unsafe { crate::driver::cuMemRelease(driver) }?;
        if object.reference != reference {
            return Err(Error::from(CUDA_ERROR_INVALID_VALUE));
        }
    } else {
        state.memblocks.insert(
            id,
            Memblock::Multicast(MulticastObject::imported(
                reference, properties, driver, context,
            )),
        );
    }
    let virtual_multicast_handle = runtime::must_complete(state.mint_virtual_allocation_handle(id));
    state
        .memblocks
        .get_mut(&id)
        .unwrap()
        .refcounts_mut()
        .handle_entries += 1;
    Ok((state, virtual_multicast_handle))
}

impl Binding {
    fn apply(&self, group: u64, member: u64) -> Result<()> {
        use crate::driver;
        // Both binding versions record the same source, with v2 adding an explicit
        // device argument.
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

// Resolve application handles before recording metadata for restore.
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
    let state = runtime::active()?;
    let target = state
        .resolve_virtual_handle(handle)?
        .ok_or(CUDA_ERROR_NOT_SUPPORTED)?;
    let binding_lock = binding_lock(&state, target)?;
    let (state, _binding_guard) = lock_bindings(state, &binding_lock)?;
    let (driver, context) = multicast_driver(&state, target, Some(&binding_lock))?;
    let (source, member, member_driver) = match input {
        BindInput::Memory {
            handle: member_handle,
            offset: member_offset,
        } => {
            let member = match state.resolve_virtual_handle(member_handle)? {
                // One handle cannot name both the object and its member.
                Some(id) if id != target => Some(id),
                Some(_) => return Err(CUDA_ERROR_INVALID_HANDLE.into()),
                None => None,
            };
            if let Some(id) = member {
                let allocation = state
                    .memblocks
                    .get(&id)
                    .and_then(Member::of)
                    .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                if member_offset
                    .checked_add(size)
                    .is_none_or(|end| allocation.size != 0 && end > allocation.size)
                {
                    return Err(CUDA_ERROR_INVALID_VALUE.into());
                }
                if version == BindingVersion::V1 {
                    let location = allocation.location;
                    if location.type_
                        != cudarc::driver::sys::CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE
                    {
                        return Err(CUDA_ERROR_NOT_SUPPORTED.into());
                    }
                    device = location.id;
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
                                .and_then(Member::of)
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
                        .and_then(Member::of)
                        .is_some()
            });
            let range = if let Some(mapping) = mapping {
                let allocation =
                    Member::of(&state.memblocks[&mapping.id]).ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                let displacement = address - mapping.address;
                if displacement
                    .checked_add(size as u64)
                    .is_none_or(|end| end > mapping.size as u64)
                {
                    return Err(CUDA_ERROR_INVALID_VALUE.into());
                }
                if version == BindingVersion::V1 {
                    let location = allocation.location;
                    if location.type_
                        != cudarc::driver::sys::CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE
                    {
                        return Err(CUDA_ERROR_NOT_SUPPORTED.into());
                    }
                    device = location.id;
                }
                Some(MemberRange {
                    allocation: allocation.reference,
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
    let context = if context == 0 {
        crate::driver::context()?
    } else {
        context
    };
    let (mut state, ()) = runtime::call_unlocked(state, || binding.apply(driver, member_driver))?;
    // The driver accepted the binding, so an import of unknown kind is the multicast
    // object, or memory as its member.
    if let Some(id) = member {
        state.classify(id, FabricKind::Memory);
        runtime::must_complete(
            state
                .memblocks
                .get_mut(&id)
                .and_then(Memblock::unicast_mut)
                .ok_or(CUDA_ERROR_INVALID_HANDLE.into()),
        )
        .shared = true;
    }
    state.classify(target, FabricKind::Multicast);
    let object = runtime::must_complete(binding_object(&mut state, target, &binding_lock));
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
            properties: cuinterpose_protocol::MulticastProperties {
                devices: object.properties.numDevices,
                size: object.properties.size as u64,
                handle_types: object.properties.handleTypes,
                flags: object.properties.flags,
            },
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

/// Application threads remain paused throughout restore. Since the peer FD service uses
/// only the export cache, reconstruction can hold the registry lock even during driver
/// calls that wait for other processes. Creators return the fabric handles that they
/// export in place of checkpointed ones, and importers import those replacements.
pub fn restore(state: &mut ProcessState, operation: &Operation) -> Result<Vec<FabricReplacement>> {
    let mut exported = Vec::new();
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
        if (matches!(operation, Operation::RestoreMulticastCreators) && !creator)
            || (matches!(operation, Operation::RestoreMulticastImporters { .. }) && creator)
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
                    if let Some(old) = object.fabric {
                        let new = crate::driver::export_fabric(driver)?;
                        exported.push(FabricReplacement { old, new });
                        object.fabric = Some(new);
                    } else if object.shared {
                        let fd = crate::driver::export_posix(driver)?;
                        runtime::export_cache()?.insert(
                            object.reference,
                            fd,
                            sharing::ExportMetadata::Multicast(object.properties),
                        )?;
                    }
                }
                Operation::RestoreMulticastImporters { replacements }
                    if !creator && object.fabric.is_some() =>
                {
                    let new = super::fabric::replacement(replacements, object.fabric.unwrap())?;
                    object.driver = Some(crate::driver::import_fabric(&new)?);
                    object.fabric = Some(new);
                }
                Operation::RestoreMulticastImporters { .. } if !creator => {
                    let (fd, metadata) =
                        sharing::request_export(object.reference, object.reference.creator_pid)
                            .map_err(Error::PeerExport)?;
                    if !matches!(metadata, sharing::ExportMetadata::Multicast(properties) if properties == object.properties)
                    {
                        return Err(Error::from(CUDA_ERROR_INVALID_HANDLE));
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
                Operation::RestoreMulticastCreators
                | Operation::RestoreMulticastImporters { .. } => {}
                _ => return Err(Error::from(CUDA_ERROR_INVALID_VALUE)),
            }
            Ok(())
        })?;
    }
    Ok(exported)
}
/// The registry lock must be released around multicast creation because the CUDA call
/// can wait for other processes.
pub(crate) fn create_backing(
    state: MutexGuard<'static, ProcessState>,
    properties: &CUmulticastObjectProp,
) -> Result<(MutexGuard<'static, ProcessState>, u64)> {
    runtime::call_unlocked(state, || {
        let mut driver = 0;
        unsafe { crate::driver::cuMulticastCreate(&mut driver, properties) }?;
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
                refcounts: super::Refcounts {
                    handle_entries: 1,
                    mappings: 0,
                },
                properties,
                driver: Some(driver),
                context,
                shared: false,
                devices: Vec::new(),
                bindings: Vec::new(),
                binding_lock: Arc::default(),
                fabric: None,
            }),
        );
        Ok(virtual_multicast_handle)
    }
}

pub(crate) fn add_device(
    state: MutexGuard<'static, ProcessState>,
    id: AllocationId,
    device: i32,
) -> Result<()> {
    let (driver, context) = multicast_driver(&state, id, None)?;
    let context = if context == 0 {
        crate::driver::context()?
    } else {
        context
    };
    let (mut state, ()) = runtime::call_unlocked(state, || unsafe {
        crate::driver::cuMulticastAddDevice(driver, device)
    })?;
    state.classify(id, FabricKind::Multicast);
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

// Keep native bind/unbind order and record updates in the same order. Waiting for
// this object must not hold ProcessState: the previous operation needs it to commit.
// AddDevice remains independent so it can complete membership for a blocked bind.
fn lock_bindings<'a>(
    state: MutexGuard<'static, ProcessState>,
    binding_lock: &'a Arc<Mutex<()>>,
) -> Result<(MutexGuard<'static, ProcessState>, MutexGuard<'a, ()>)> {
    runtime::call_unlocked(state, || {
        binding_lock.lock().map_err(|_| Error::RuntimeFailed)
    })
}

fn binding_object<'a>(
    state: &'a mut ProcessState,
    id: AllocationId,
    binding_lock: &Arc<Mutex<()>>,
) -> Result<&'a mut MulticastObject> {
    // A lifetime violation can remove and re-import the same allocation identity.
    // The mutex identifies this instance without retaining its CUDA lifetime.
    state
        .memblocks
        .get_mut(&id)
        .and_then(Memblock::multicast_mut)
        .filter(|object| Arc::ptr_eq(&object.binding_lock, binding_lock))
        .ok_or(CUDA_ERROR_INVALID_HANDLE.into())
}

pub(crate) fn unbind(
    state: MutexGuard<'static, ProcessState>,
    id: AllocationId,
    device: i32,
    offset: usize,
    size: usize,
) -> Result<()> {
    let binding_lock = binding_lock(&state, id)?;
    let (state, _binding_guard) = lock_bindings(state, &binding_lock)?;
    let (driver, _) = multicast_driver(&state, id, Some(&binding_lock))?;
    let end = offset.checked_add(size).ok_or(CUDA_ERROR_INVALID_VALUE)?;
    // An import of unknown kind has no bindings.
    let bindings = state
        .memblocks
        .get(&id)
        .and_then(Memblock::multicast)
        .map_or(&[][..], |object| &object.bindings);
    for binding in bindings {
        if binding.device == device
            && binding.offset < end
            && binding.offset + binding.size > offset
            && (binding.offset < offset || binding.offset + binding.size > end)
        {
            return Err(Error::from(CUDA_ERROR_INVALID_VALUE));
        }
    }
    let (mut state, ()) = runtime::call_unlocked(state, || unsafe {
        crate::driver::cuMulticastUnbind(driver, device, offset, size)
    })?;
    state.classify(id, FabricKind::Multicast);
    let object = runtime::must_complete(binding_object(&mut state, id, &binding_lock));
    object.bindings.retain(|binding| {
        binding.device != device || binding.offset >= end || binding.offset + binding.size <= offset
    });
    Ok(())
}
