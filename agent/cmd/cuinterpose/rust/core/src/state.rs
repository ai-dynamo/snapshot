// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Per-generation logical handles, mappings, and shared-allocation lifecycle state.
//! The generation owns its locks and CUDA records; fork abandons rather than drops it.

use super::ticket;
use crate::driver::CudaError;
use crate::logical_handle::{LOGICAL_HANDLE_MASK, LOGICAL_HANDLE_TAG};
use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_INITIALIZED,
    CUDA_ERROR_NOT_READY, CUDA_ERROR_NOT_SUPPORTED, CUDA_ERROR_OUT_OF_MEMORY, CUDA_ERROR_UNKNOWN,
};
use cudarc::driver::sys::{
    CUmemAccess_flags, CUmemAccessDesc, CUmemAllocationGranularity_flags,
    CUmemAllocationHandleType, CUmemAllocationProp, CUmemAllocationType, CUmemLocationType,
};
use cuinterpose_protocol::Ticket;
use cuinterpose_protocol::{AllocationId, Operation, ParticipantId, Resource, ResourceKind};

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lifecycle_transitions_require_each_local_milestone() {
        let transitions = [
            (
                Phase::Active,
                Operation::PrepareMulticast,
                Phase::MulticastPrepared,
            ),
            (
                Phase::MulticastPrepared,
                Operation::SaveAllocations,
                Phase::AllocationsSaved,
            ),
            (
                Phase::AllocationsSaved,
                Operation::PrepareUnicast,
                Phase::UnicastPrepared,
            ),
            (
                Phase::UnicastPrepared,
                Operation::LoadAllocations,
                Phase::AllocationsLoaded,
            ),
            (
                Phase::AllocationsLoaded,
                Operation::RestoreUnicast,
                Phase::UnicastRestored,
            ),
            (
                Phase::UnicastRestored,
                Operation::RestoreMulticastCreators,
                Phase::MulticastCreatorsRestored,
            ),
            (
                Phase::MulticastCreatorsRestored,
                Operation::RestoreMulticastImporters,
                Phase::MulticastImportersRestored,
            ),
            (
                Phase::MulticastImportersRestored,
                Operation::RestoreMulticastDevices,
                Phase::MulticastDevicesRestored,
            ),
            (
                Phase::MulticastDevicesRestored,
                Operation::RestoreMulticastBindings,
                Phase::Active,
            ),
        ];
        for (phase, operation, next) in transitions {
            assert_eq!(phase.next(operation), Ok(next));
            for (other, _, _) in transitions {
                if other != phase {
                    assert_eq!(
                        other.next(operation),
                        Err(CudaError::from(CUDA_ERROR_NOT_READY))
                    );
                }
            }
            assert_eq!(
                Phase::ReconstructingMulticast.next(operation),
                Err(CudaError::from(CUDA_ERROR_NOT_READY))
            );
        }
    }

    #[test]
    fn oversized_inspection_is_refused_before_building_records() {
        let state = State {
            identity: ParticipantId::default(),
            endpoint: String::new(),
            allocations: BTreeMap::new(),
            multicasts: BTreeMap::new(),
            handles: BTreeMap::new(),
            mappings: (0..=cuinterpose_protocol::MAX_RECORDS)
                .map(|index| {
                    let address = (index * 4096) as u64;
                    (
                        address,
                        Mapping {
                            id: AllocationId::default(),
                            address,
                            size: 4096,
                            offset: 0,
                            access: Vec::new(),
                            unknown: false,
                            flags: 0,
                            checkpointed: false,
                        },
                    )
                })
                .collect(),
            raw: BTreeMap::new(),
            unreleased_handles: Vec::new(),
            unsupported: 0,
            phase: Phase::Active,
            arena: None,
            inflight: 0,
            pending_maps: Vec::new(),
            next: 1,
        };
        // Missing allocation records would panic if serialization began.
        assert_eq!(
            state.inspect(),
            Err(CudaError::from(CUDA_ERROR_NOT_SUPPORTED))
        );
    }
}
use std::collections::BTreeMap;
use std::ffi::c_void;
use std::os::fd::{AsFd, IntoRawFd};
use std::sync::atomic::{AtomicBool, AtomicPtr, Ordering};
use std::sync::{Mutex, MutexGuard, TryLockError};

pub use crate::driver::Result;
struct Generation {
    state: Mutex<State>,
    cache: super::export_cache::ExportCache,
}
// Release publication follows successful worker startup; Acquire readers may
// then borrow the generation for its process lifetime. Only a quiescent fork
// child abandons the inherited pointer; it never frees the parent's generation.
static G_STATE: AtomicPtr<Generation> = AtomicPtr::new(std::ptr::null_mut());
static G_INITIALIZING: Mutex<()> = Mutex::new(());
static G_CHILD: AtomicBool = AtomicBool::new(false);

pub struct ForkState {
    // Field order releases locks in reverse acquisition order in the parent.
    cache: Option<MutexGuard<'static, super::export_cache::Entries>>,
    state: Option<MutexGuard<'static, State>>,
    initializing: Option<MutexGuard<'static, ()>>,
}

impl ForkState {
    pub fn abandon(&mut self) {
        if let Some(arena) = self.state.as_ref().and_then(|state| state.arena.as_ref()) {
            unsafe { libc::syscall(libc::SYS_munmap, arena.base, arena.size) };
        }
        // These guards protect CUDA state belonging to the parent's generation.
        // The child must neither unlock nor drop that state through Rust/CUDA.
        std::mem::forget(self.state.take());
        std::mem::forget(self.cache.take());
        drop(self.initializing.take());
        G_STATE.store(std::ptr::null_mut(), Ordering::Release);
        G_CHILD.store(true, Ordering::Release);
        super::G_FAILED.store(false, Ordering::Release);
    }
}
pub fn fork_lock(descriptors: &mut Vec<i32>) -> ForkState {
    let initializing = G_INITIALIZING.lock().unwrap_or_else(|e| e.into_inner());
    let pointer = G_STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return ForkState {
            initializing: Some(initializing),
            state: None,
            cache: None,
        };
    }
    let generation = unsafe { &*pointer };
    let state = generation.state.lock().unwrap_or_else(|e| e.into_inner());
    let cache = generation.cache.fork_lock(descriptors);
    ForkState {
        initializing: Some(initializing),
        state: Some(state),
        cache: Some(cache),
    }
}

pub fn cache() -> Result<&'static super::export_cache::ExportCache> {
    let pointer = G_STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return Err(CudaError::from(CUDA_ERROR_NOT_INITIALIZED));
    }
    Ok(&unsafe { &*pointer }.cache)
}

#[derive(Clone)]
pub struct Allocation {
    pub id: AllocationId,
    pub ticket: Ticket,
    pub driver: Option<u64>,
    pub size: usize,
    pub properties: CUmemAllocationProp,
    pub creator: bool,
    pub shared: bool,
    pub context: usize,
    pub checkpointed: bool,
    pub content_saved: bool,
    pub pins: usize,
}

impl Allocation {
    fn owns_content(&self) -> bool {
        self.creator
            && self.properties.type_ == CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED
            && self.properties.location.type_ == CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE
            && self.shared
            && self.properties.requestedHandleTypes.0 != 0
    }
}

// CUmemAllocationProp's Win32 pointer is opaque and is never dereferenced on Linux.
// Driver access and allocation metadata are serialized under State's mutex.
unsafe impl Send for Allocation {}

#[derive(Clone)]
pub struct Mapping {
    pub id: AllocationId,
    pub address: u64,
    pub size: usize,
    pub offset: usize,
    pub access: Vec<CUmemAccessDesc>,
    pub unknown: bool,
    pub flags: u64,
    pub checkpointed: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    Active,
    MulticastPrepared,
    AllocationsSaved,
    UnicastPrepared,
    AllocationsLoaded,
    UnicastRestored,
    MulticastCreatorsRestored,
    MulticastImportersRestored,
    MulticastDevicesRestored,
    ReconstructingMulticast,
}

impl Phase {
    /// Validate ordering before mutation and identify the state to publish on
    /// success. The coordinator, not this local state, owns global barriers.
    pub(super) fn next(self, operation: Operation) -> Result<Self> {
        let (expected, next) = match operation {
            Operation::PrepareMulticast => (Self::Active, Self::MulticastPrepared),
            Operation::SaveAllocations => (Self::MulticastPrepared, Self::AllocationsSaved),
            Operation::PrepareUnicast => (Self::AllocationsSaved, Self::UnicastPrepared),
            Operation::LoadAllocations => (Self::UnicastPrepared, Self::AllocationsLoaded),
            Operation::RestoreUnicast => (Self::AllocationsLoaded, Self::UnicastRestored),
            Operation::RestoreMulticastCreators => {
                (Self::UnicastRestored, Self::MulticastCreatorsRestored)
            }
            Operation::RestoreMulticastImporters => (
                Self::MulticastCreatorsRestored,
                Self::MulticastImportersRestored,
            ),
            Operation::RestoreMulticastDevices => (
                Self::MulticastImportersRestored,
                Self::MulticastDevicesRestored,
            ),
            Operation::RestoreMulticastBindings => (Self::MulticastDevicesRestored, Self::Active),
        };
        if self != expected {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        Ok(next)
    }
}

pub struct State {
    pub identity: ParticipantId,
    pub endpoint: String,
    pub allocations: BTreeMap<AllocationId, Allocation>,
    pub multicasts: BTreeMap<AllocationId, super::multicast::Object>,
    pub handles: BTreeMap<u64, AllocationId>,
    pub mappings: BTreeMap<u64, Mapping>,
    pub raw: BTreeMap<u64, u32>,
    // Failed redundant-reference cleanup poisons capture, but ownership remains
    // recorded until the failed process is terminated.
    pub unreleased_handles: Vec<u64>,
    pub unsupported: u64,
    pub phase: Phase,
    pub arena: Option<super::host_carrier::Arena>,
    pub inflight: usize,
    pub pending_maps: Vec<(u64, usize)>,
    next: u64,
}

impl State {
    pub fn inspect(&self) -> Result<Vec<cuinterpose_protocol::Record>> {
        use cuinterpose_protocol::Record;
        if self.phase != Phase::Active || self.inflight != 0 {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        let count = self
            .allocations
            .len()
            .checked_add(self.mappings.len())
            .and_then(|n| {
                self.multicasts.values().try_fold(n, |n, object| {
                    n.checked_add(1 + object.devices.len() + object.bindings.len())
                })
            })
            .ok_or(CUDA_ERROR_OUT_OF_MEMORY)?;
        if count > cuinterpose_protocol::MAX_RECORDS {
            return Err(CudaError::from(CUDA_ERROR_NOT_SUPPORTED));
        }
        let mut records = Vec::new();
        records
            .try_reserve_exact(count)
            .map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
        for allocation in self.allocations.values() {
            let handles = self
                .handles
                .values()
                .filter(|id| **id == allocation.id)
                .count() as u32;
            let record = Record::Allocation {
                id: allocation.id,
                creator: allocation.creator,
                content: allocation.owns_content(),
                size: allocation.size as u64,
                allocation_type: allocation.properties.type_ as i32,
                handle_types: allocation.properties.requestedHandleTypes.0,
                location_type: allocation.properties.location.type_ as i32,
                location_id: allocation.properties.location.id,
                handles,
            };
            records.push(record);
        }
        for mapping in self.mappings.values() {
            if mapping.unknown {
                return Err(CudaError::from(CUDA_ERROR_NOT_SUPPORTED));
            }
            if self.multicasts.contains_key(&mapping.id) {
                continue;
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
            let record = Record::Mapping {
                creator: self.allocations[&mapping.id].creator,
                id: mapping.id,
                address: mapping.address,
                size: mapping.size as u64,
                offset: mapping.offset as u64,
                access,
            };
            records.push(record);
        }
        super::multicast::describe(self, &mut records)?;
        Ok(records)
    }

    pub fn validate_lifecycle(&self, operation: Operation) -> Result<()> {
        self.phase.next(operation)?;
        if self.inflight != 0 {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        if self.unsupported != 0 || !self.raw.is_empty() {
            return Err(CudaError::from(CUDA_ERROR_NOT_SUPPORTED));
        }
        if operation == Operation::PrepareMulticast {
            self.inspect()?;
        }
        Ok(())
    }

    /// Validation has completed without mutation. Failures here are fail-stop.
    pub fn lifecycle(&mut self, operation: Operation) -> Result<super::host_carrier::Transfer> {
        use super::host_carrier::{AllocationContent, Arena, Context};
        let next_phase = self.phase.next(operation)?;
        let mut bytes = 0u64;
        let mut copy_us = 0u32;
        match operation {
            Operation::PrepareMulticast => {
                super::multicast::prepare(self)?;
            }
            Operation::SaveAllocations => {
                let ids: Vec<_> = self
                    .allocations
                    .values()
                    .filter(|a| a.owns_content())
                    .map(|a| a.id)
                    .collect();
                let mut recovered = Vec::new();
                let saved = (|| -> Result<(Option<Arena>, u32)> {
                    let mut allocations = Vec::new();
                    for id in ids {
                        let allocation = self
                            .allocations
                            .get_mut(&id)
                            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                        if allocation.driver.is_none() {
                            let mapping = self
                                .mappings
                                .values()
                                .find(|m| m.id == id)
                                .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                            Context::run(
                                allocation.context,
                                allocation.properties.location.id,
                                || {
                                    let mut driver = 0;
                                    unsafe {
                                        crate::driver::cuMemRetainAllocationHandle(
                                            &mut driver,
                                            mapping.address as usize as *mut c_void,
                                        )
                                    }?;
                                    allocation.driver = Some(driver);
                                    recovered.push(id);
                                    Ok(())
                                },
                            )?;
                        }
                        bytes = bytes
                            .checked_add(allocation.size as u64)
                            .ok_or(CUDA_ERROR_OUT_OF_MEMORY)?;
                        allocations.push(AllocationContent::from(&*allocation));
                    }
                    Arena::save(&allocations)
                })();
                let (arena, elapsed) = match saved {
                    Ok(saved) => saved,
                    Err(error) => {
                        for id in recovered {
                            if let Some(allocation) = self.allocations.get_mut(&id)
                                && let Ok(context) = Context::enter(
                                    allocation.context,
                                    allocation.properties.location.id,
                                )
                            {
                                if let Some(driver) = allocation.driver
                                    && unsafe { crate::driver::cuMemRelease(driver) }.is_ok()
                                {
                                    allocation.driver = None;
                                }
                                let _ = context.leave();
                            }
                        }
                        return Err(error);
                    }
                };
                self.arena = arena;
                copy_us = elapsed;
                for allocation in self.allocations.values_mut().filter(|a| a.owns_content()) {
                    allocation.content_saved = true;
                }
            }
            Operation::PrepareUnicast => {
                cache()?.clear()?;
                for allocation in self.allocations.values_mut().filter(|a| a.shared) {
                    Context::run(
                        allocation.context,
                        allocation.properties.location.id,
                        || {
                            for mapping in
                                self.mappings.values_mut().filter(|m| m.id == allocation.id)
                            {
                                unsafe {
                                    crate::driver::cuMemUnmap(mapping.address, mapping.size)
                                }?;
                                mapping.checkpointed = true;
                            }
                            if let Some(driver) = allocation.driver {
                                unsafe { crate::driver::cuMemRelease(driver) }?;
                                allocation.driver = None;
                            }
                            allocation.checkpointed = true;
                            Ok(())
                        },
                    )?;
                }
            }
            Operation::LoadAllocations => {
                let mut allocations: Vec<_> = self
                    .allocations
                    .values()
                    .filter(|a| a.content_saved)
                    .map(AllocationContent::from)
                    .collect();
                bytes = allocations.iter().try_fold(0u64, |sum, a| {
                    sum.checked_add(a.size as u64)
                        .ok_or(CUDA_ERROR_OUT_OF_MEMORY)
                })?;
                if let Some(arena) = &self.arena {
                    copy_us = arena.load(&mut allocations)?;
                } else if !allocations.is_empty() {
                    return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
                }
                for allocation in allocations {
                    self.allocations
                        .get_mut(&allocation.id)
                        .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                        .driver = allocation.driver;
                }
                self.remap(true)?;
            }
            Operation::RestoreUnicast => {
                for allocation in self
                    .allocations
                    .values_mut()
                    .filter(|a| !a.creator && a.checkpointed)
                {
                    let raw = ticket::request(&allocation.ticket)
                        .map_err(|_| CUDA_ERROR_INVALID_HANDLE)?;
                    let mut imported = None;
                    let result = Context::run(
                        allocation.context,
                        allocation.properties.location.id,
                        || {
                            imported = Some(crate::driver::import_posix(raw.as_fd())?);
                            Ok(())
                        },
                    );
                    if let Err(error) = result {
                        if let Some(driver) = imported {
                            let _ = Context::run(
                                allocation.context,
                                allocation.properties.location.id,
                                || unsafe { crate::driver::cuMemRelease(driver) },
                            );
                        }
                        return Err(error);
                    }
                    allocation.driver = imported;
                }
                self.remap(false)?;
            }
            _ => return Err(CudaError::from(CUDA_ERROR_NOT_SUPPORTED)),
        }
        self.phase = next_phase;
        Ok(super::host_carrier::Transfer { bytes, copy_us })
    }

    fn remap(&mut self, creator: bool) -> Result<()> {
        for allocation in self
            .allocations
            .values_mut()
            .filter(|a| a.checkpointed && a.creator == creator)
        {
            super::host_carrier::Context::run(
                allocation.context,
                allocation.properties.location.id,
                || {
                    for mapping in self
                        .mappings
                        .values_mut()
                        .filter(|m| m.id == allocation.id && m.checkpointed)
                    {
                        unsafe {
                            crate::driver::cuMemMap(
                                mapping.address,
                                mapping.size,
                                mapping.offset,
                                allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                                0,
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
                    if creator && allocation.shared {
                        let fd = crate::driver::export_posix(
                            allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                        )?;
                        cache()?.replace((ResourceKind::Unicast, allocation.id), Some(fd))?;
                    }
                    if !self.handles.values().any(|id| *id == allocation.id) {
                        unsafe {
                            crate::driver::cuMemRelease(
                                allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                            )
                        }?;
                        allocation.driver = None;
                    }
                    allocation.checkpointed = false;
                    allocation.content_saved = false;
                    Ok(())
                },
            )?;
        }
        Ok(())
    }

    pub(super) fn mint(&mut self, id: AllocationId) -> Result<u64> {
        if self.next & LOGICAL_HANDLE_MASK != 0 {
            return Err(CudaError::from(CUDA_ERROR_OUT_OF_MEMORY));
        }
        let handle = LOGICAL_HANDLE_TAG | self.next;
        self.next += 1;
        self.handles.insert(handle, id);
        Ok(handle)
    }

    pub fn live_raw_imports(&self) -> u64 {
        self.raw.values().map(|count| u64::from(*count)).sum()
    }

    pub fn unsupported_exportable_creations(&self) -> u64 {
        self.unsupported
    }

    pub(super) fn settle(&mut self, id: AllocationId) -> Result<()> {
        if self.multicasts.contains_key(&id) {
            return super::multicast::settle(self, id);
        }
        let handle_live = self.handles.values().any(|value| *value == id);
        let mapped = self.mappings.values().any(|mapping| mapping.id == id);
        let allocation = self
            .allocations
            .get_mut(&id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        if !handle_live && let Some(driver) = allocation.driver {
            unsafe { crate::driver::cuMemRelease(driver) }?;
            allocation.driver = None;
        }
        if !handle_live && !mapped {
            cache()?.replace((ResourceKind::Unicast, id), None)?;
            self.allocations.remove(&id);
        }
        Ok(())
    }

    pub(super) fn covered(&self, address: u64, size: usize) -> Result<Vec<u64>> {
        let end = address
            .checked_add(size as u64)
            .ok_or(CUDA_ERROR_INVALID_VALUE)?;
        for &(base, length) in &self.pending_maps {
            if base < end && base + length as u64 > address {
                return Err(CudaError::from(CUDA_ERROR_NOT_READY));
            }
        }
        let mut result = Vec::new();
        for (base, mapping) in &self.mappings {
            let limit = base
                .checked_add(mapping.size as u64)
                .ok_or(CUDA_ERROR_INVALID_VALUE)?;
            if *base < end && limit > address {
                if *base < address || limit > end {
                    return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
                }
                result.push(*base);
            }
        }
        Ok(result)
    }
}

pub(super) fn random<const N: usize>() -> Result<[u8; N]> {
    let mut bytes = [0; N];
    let mut offset = 0;
    while offset < N {
        let count = unsafe { libc::getrandom(bytes[offset..].as_mut_ptr().cast(), N - offset, 0) };
        if count > 0 {
            offset += count as usize;
        } else if count < 0
            && std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted
        {
            continue;
        } else {
            return Err(CudaError::from(CUDA_ERROR_NOT_INITIALIZED));
        }
    }
    Ok(bytes)
}

pub fn initialize() -> Result<()> {
    // STATE denotes a ready generation, never one whose listener is still
    // starting. A caller may own the loader lock needed by another initializer,
    // so it must not wait for that initializer's thread/TLS setup.
    if !G_STATE.load(Ordering::Acquire).is_null() {
        return Ok(());
    }
    if super::G_FAILED.load(Ordering::Acquire) {
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    let _initializing = match G_INITIALIZING.try_lock() {
        Ok(guard) => guard,
        Err(TryLockError::WouldBlock) => {
            return Err(CudaError::from(CUDA_ERROR_NOT_INITIALIZED));
        }
        Err(TryLockError::Poisoned(poison)) if G_CHILD.load(Ordering::Acquire) => {
            poison.into_inner()
        }
        Err(TryLockError::Poisoned(_)) => return Err(CudaError::from(CUDA_ERROR_UNKNOWN)),
    };
    if !G_STATE.load(Ordering::Acquire).is_null() {
        return Ok(());
    }
    if super::G_FAILED.load(Ordering::Acquire) {
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    let result = initialize_generation();
    if result.is_err() {
        // Actual setup failure is sticky; contention above is a transient
        // refusal and must not poison the initializer that is making progress.
        super::G_FAILED.store(true, Ordering::Release);
    }
    result
}

fn initialize_generation() -> Result<()> {
    let pid = unsafe { libc::getpid() };
    let configured = if G_CHILD.load(Ordering::Acquire)
        || super::G_FRONTEND_ABI
            .get()
            .is_some_and(|host| host.origin_pid != pid)
    {
        Err(std::env::VarError::NotPresent)
    } else {
        std::env::var("CUINTERPOSE_PARTICIPANT_ID")
    };
    let identity = match configured {
        Ok(value) => value.parse().map_err(|_| CUDA_ERROR_INVALID_VALUE)?,
        Err(std::env::VarError::NotPresent) => ParticipantId(random()?),
        Err(_) => return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE)),
    };
    let directory =
        std::env::var("SNAPSHOT_CONTROL_DIR").unwrap_or_else(|_| "/snapshot-control".into());
    if !directory.starts_with('/') {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let endpoint = format!("{directory}/cuinterpose-{pid}.sock");
    std::os::unix::net::SocketAddr::from_pathname(&endpoint)
        .map_err(|_| CUDA_ERROR_INVALID_VALUE)?;
    let state = State {
        identity,
        endpoint,
        allocations: BTreeMap::new(),
        multicasts: BTreeMap::new(),
        handles: BTreeMap::new(),
        mappings: BTreeMap::new(),
        raw: BTreeMap::new(),
        unreleased_handles: Vec::new(),
        unsupported: 0,
        next: 1,
        phase: Phase::Active,
        arena: None,
        inflight: 0,
        pending_maps: Vec::new(),
    };
    let mut generation = Box::new(Generation {
        state: Mutex::new(state),
        cache: super::export_cache::ExportCache::default(),
    });
    let state = generation.state.get_mut().map_err(|_| CUDA_ERROR_UNKNOWN)?;
    super::control::start(&state.endpoint, state.identity)?;
    // No fallible work follows successful startup. Failed startup drops only
    // the unpublished, empty generation; no CUDA resources have been created.
    G_STATE.store(Box::into_raw(generation), Ordering::Release);
    Ok(())
}

pub fn get() -> Result<MutexGuard<'static, State>> {
    if super::G_FAILED.load(Ordering::Acquire) {
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    let pointer = G_STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return Err(CudaError::from(CUDA_ERROR_NOT_INITIALIZED));
    }
    let state = unsafe { &*pointer }
        .state
        .lock()
        .map_err(|_| CUDA_ERROR_UNKNOWN)?;
    // A caller may have waited behind a failed lifecycle operation. Do not
    // admit queued mutations using only the pre-lock check.
    if super::G_FAILED.load(Ordering::Acquire) {
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    Ok(state)
}

pub(super) fn active() -> Result<MutexGuard<'static, State>> {
    let state = get()?;
    if state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    Ok(state)
}

pub(super) fn context() -> usize {
    let mut context = std::ptr::null_mut::<c_void>();
    if unsafe { crate::driver::cuCtxGetCurrent(&mut context) }.is_err() {
        return 0;
    }
    context as usize
}

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
    if supported && state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    // Reserve the logical identity before acquiring backing. Recoverable
    // metadata errors must not leave an unpublished CUDA allocation behind.
    let tracked = if supported {
        if state.next & LOGICAL_HANDLE_MASK != 0 {
            return Err(CudaError::from(CUDA_ERROR_OUT_OF_MEMORY));
        }
        Some(AllocationId(random()?))
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
    if driver & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
        let _ = unsafe { crate::driver::cuMemRelease(driver) };
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    if !supported {
        if properties.requestedHandleTypes.0 != 0 {
            state.unsupported += 1;
        }
        unsafe {
            out.write(driver);
        }
        return Ok(());
    }
    let id = tracked.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    let ticket = Ticket {
        creator: state.identity,
        allocation: id,
        endpoint: state.endpoint.clone(),
        resource: Resource::Unicast,
    };
    let allocation = Allocation {
        id,
        ticket,
        driver: Some(driver),
        size,
        properties,
        creator: true,
        shared: false,
        context: context(),
        checkpointed: false,
        content_saved: false,
        pins: 0,
    };
    let logical = match state.mint(id) {
        Ok(logical) => logical,
        Err(error) => {
            let _ = unsafe { crate::driver::cuMemRelease(driver) };
            return Err(error);
        }
    };
    state.allocations.insert(id, allocation);
    unsafe {
        out.write(logical);
    }
    Ok(())
}

pub fn cuMemRelease(handle: u64) -> Result<()> {
    let mut state = get()?;
    if let Some(id) = state.handles.get(&handle)
        && (state.phase != Phase::Active
            || state.multicasts.get(id).is_some_and(|a| a.inflight != 0)
            || state.allocations.get(id).is_some_and(|a| a.pins != 0))
    {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    if let Some(id) = state.handles.remove(&handle) {
        if let Err(error) = state.settle(id) {
            state.handles.insert(handle, id);
            return Err(error);
        }
    } else {
        if handle & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
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
    // CUDA may already have mapped a multicast range while its record is
    // still pending publication. Do not let the native fallback expose that
    // object's real driver handle before we can return a tracked alias.
    for &(base, size) in &state.pending_maps {
        let end = base
            .checked_add(size as u64)
            .ok_or(CUDA_ERROR_INVALID_VALUE)?;
        if (address as u64) >= base && (address as u64) < end {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
    }
    let id = state
        .mappings
        .values()
        .find(|m| address as u64 >= m.address && (address as u64) - m.address < m.size as u64)
        .map(|m| m.id);
    if let Some(id) = id {
        if state.phase != Phase::Active {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        if state.next & LOGICAL_HANDLE_MASK != 0 {
            return Err(CudaError::from(CUDA_ERROR_OUT_OF_MEMORY));
        }
        if let Some(object) = state.multicasts.get(&id) {
            if object.driver.is_none() {
                return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
            }
            unsafe {
                out.write(state.mint(id)?);
            }
            return Ok(());
        }
    }
    let mut driver = 0;
    unsafe { crate::driver::cuMemRetainAllocationHandle(&mut driver, address) }?;
    if let Some(id) = id {
        let allocation = state
            .allocations
            .get_mut(&id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        if allocation.driver.is_some() {
            if let Err(error) = unsafe { crate::driver::cuMemRelease(driver) } {
                state.unreleased_handles.push(driver);
                super::G_FAILED.store(true, Ordering::Release);
                return Err(error);
            }
        } else {
            allocation.driver = Some(driver);
        }
        unsafe {
            out.write(state.mint(id)?);
        }
    } else {
        if driver & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
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
    let Some(id) = state.handles.get(&handle).copied() else {
        if handle & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
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
    if state.multicasts.contains_key(&id) {
        return super::multicast::map(state, id, address, size, offset, flags);
    }
    if size == 0 || !state.covered(address, size)?.is_empty() {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let allocation = state
        .allocations
        .get_mut(&id)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    unsafe {
        crate::driver::cuMemMap(
            address,
            size,
            offset,
            allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
            flags,
        )
    }?;
    if allocation.context == 0 {
        allocation.context = context();
    }
    state.mappings.insert(
        address,
        Mapping {
            id,
            address,
            size,
            offset,
            access: Vec::new(),
            unknown: false,
            flags,
            checkpointed: false,
        },
    );
    Ok(())
}

pub fn cuMemUnmap(address: u64, size: usize) -> Result<()> {
    let mut state = get()?;
    let mappings = state.covered(address, size)?;
    if !mappings.is_empty() && state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    for base in &mappings {
        let id = &state.mappings[base].id;
        if state.multicasts.get(id).is_some_and(|a| a.inflight != 0)
            || state.allocations.get(id).is_some_and(|a| a.pins != 0)
        {
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
    let mut merged = Vec::new();
    for base in &mappings {
        let mut entries = state.mappings[base].access.clone();
        for descriptor in descriptors {
            entries.retain(|entry| {
                entry.location.type_ != descriptor.location.type_
                    || entry.location.id != descriptor.location.id
            });
            if descriptor.flags != CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_NONE {
                entries.push(*descriptor);
            }
            if entries.len() > 32 {
                return Err(CudaError::from(CUDA_ERROR_NOT_SUPPORTED));
            }
        }
        merged.push(entries);
    }
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
    let Some(id) = state.handles.get(&handle).copied() else {
        if handle & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
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
    if state.multicasts.contains_key(&id) {
        return super::multicast::export(&mut state, id, out);
    }
    let allocation = state
        .allocations
        .get_mut(&id)
        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    if allocation.properties.requestedHandleTypes.0 & kind.0 == 0 {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    if allocation.creator && !cache()?.contains(&(ResourceKind::Unicast, id))? {
        let fd = crate::driver::export_posix(allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?)?;
        cache()?.replace((ResourceKind::Unicast, id), Some(fd))?;
    }
    let ticket = ticket::export(&allocation.ticket).map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
    allocation.shared = true;
    if allocation.context == 0 {
        allocation.context = context();
    }
    unsafe {
        out.cast::<i32>().write(ticket.into_raw_fd());
    }
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
    let ticket = if kind == CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR {
        ticket::read(fd as isize as i32).map_err(|_| CUDA_ERROR_INVALID_HANDLE)?
    } else {
        None
    };
    let mut state = get()?;
    let Some(ticket) = ticket else {
        let mut driver = 0;
        unsafe { crate::driver::cuMemImportFromShareableHandle(&mut driver, fd, kind) }?;
        if driver & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
            let _ = unsafe { crate::driver::cuMemRelease(driver) };
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        *state.raw.entry(driver).or_insert(0) += 1;
        unsafe {
            out.write(driver);
        }
        return Ok(());
    };
    if matches!(ticket.resource, Resource::Multicast { .. }) {
        if state.phase != Phase::Active {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        return super::multicast::import(state, out, ticket);
    }
    let logical = import_ticket(&mut state, ticket)?;
    unsafe { out.write(logical) };
    Ok(())
}

pub(super) fn import_ticket(state: &mut State, ticket: Ticket) -> Result<u64> {
    if state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    if state.next & LOGICAL_HANDLE_MASK != 0 {
        return Err(CudaError::from(CUDA_ERROR_OUT_OF_MEMORY));
    }
    let id = ticket.allocation;
    if state.multicasts.contains_key(&id) {
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    if let Some(allocation) = state.allocations.get_mut(&id) {
        if allocation.ticket != ticket {
            return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
        }
        if allocation.driver.is_none() {
            let raw = ticket::request(&ticket).map_err(|_| CUDA_ERROR_INVALID_HANDLE)?;
            let driver = crate::driver::import_posix(raw.as_fd())?;
            allocation.driver = Some(driver);
        }
        allocation.shared = true;
        return state.mint(id);
    }
    // EXPORT service uses only CACHE, never STATE, so a same-process request
    // can complete while this call holds its allocation metadata lock.
    let raw = ticket::request(&ticket).map_err(|_| CUDA_ERROR_INVALID_HANDLE)?;
    let driver = crate::driver::import_posix(raw.as_fd())?;
    let mut properties = std::mem::MaybeUninit::<CUmemAllocationProp>::zeroed();
    let recorded = (|| -> Result<u64> {
        if driver & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        unsafe {
            crate::driver::cuMemGetAllocationPropertiesFromHandle(properties.as_mut_ptr(), driver)
        }?;
        state.mint(id)
    })();
    let logical = match recorded {
        Ok(logical) => logical,
        Err(error) => {
            let _ = unsafe { crate::driver::cuMemRelease(driver) };
            return Err(error);
        }
    };
    state.allocations.insert(
        id,
        Allocation {
            id,
            ticket,
            driver: Some(driver),
            size: 0,
            properties: unsafe { properties.assume_init() },
            creator: false,
            shared: true,
            context: context(),
            checkpointed: false,
            content_saved: false,
            pins: 0,
        },
    );
    Ok(logical)
}

pub fn cuMemGetAllocationPropertiesFromHandle(
    out: *mut CUmemAllocationProp,
    handle: u64,
) -> Result<()> {
    let state = get()?;
    if state.handles.contains_key(&handle) && state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    let driver = match state.handles.get(&handle) {
        Some(id) => match state.multicasts.get(id) {
            Some(object) => object.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
            None => state.allocations[id]
                .driver
                .ok_or(CUDA_ERROR_INVALID_HANDLE)?,
        },
        None if handle & LOGICAL_HANDLE_MASK == LOGICAL_HANDLE_TAG => {
            return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
        }
        None => handle,
    };
    unsafe { crate::driver::cuMemGetAllocationPropertiesFromHandle(out, driver) }?;
    if let Some(allocation) = state
        .handles
        .get(&handle)
        .and_then(|id| state.allocations.get(id))
    {
        // Preserve driver-returned flags while hiding the internal POSIX
        // capability of an application-private allocation.
        unsafe {
            (*out).requestedHandleTypes = allocation.properties.requestedHandleTypes;
        }
    }
    Ok(())
}

pub use super::multicast::{
    cuMulticastAddDevice, cuMulticastBindAddr, cuMulticastBindAddr_v2, cuMulticastBindMem,
    cuMulticastBindMem_v2, cuMulticastCreate, cuMulticastGetGranularity, cuMulticastUnbind,
};
