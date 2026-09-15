// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Per-generation logical handles, mappings, and shared-allocation lifecycle state.
//! The generation owns its locks and CUDA records; fork abandons rather than drops it.

use super::ticket;
use cuinterpose_abi::*;
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
                        Err(crate::driver::CudaError(NOT_READY))
                    );
                }
            }
            assert_eq!(
                Phase::ReconstructingMulticast.next(operation),
                Err(crate::driver::CudaError(NOT_READY))
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
            Err(crate::driver::CudaError(NOT_SUPPORTED))
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
        return Err(crate::driver::CudaError(NOT_INITIALIZED));
    }
    Ok(&unsafe { &*pointer }.cache)
}

#[derive(Clone)]
pub struct Allocation {
    pub id: AllocationId,
    pub ticket: Ticket,
    pub driver: Option<u64>,
    pub size: usize,
    pub properties: AllocationProp,
    pub creator: bool,
    pub shared: bool,
    pub context: usize,
    pub checkpointed: bool,
    pub host_checkpointed: bool,
    pub pins: usize,
}

// AllocationProp's Win32 pointer is opaque and is never dereferenced on Linux.
// Driver access and allocation metadata are serialized under State's mutex.
unsafe impl Send for Allocation {}

#[derive(Clone)]
pub struct Mapping {
    pub id: AllocationId,
    pub address: u64,
    pub size: usize,
    pub offset: usize,
    pub access: Vec<Access>,
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
            return Err(crate::driver::CudaError(NOT_READY));
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
            return Err(crate::driver::CudaError(NOT_READY));
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
            .ok_or(OUT_OF_MEMORY)?;
        if count > cuinterpose_protocol::MAX_RECORDS {
            return Err(crate::driver::CudaError(NOT_SUPPORTED));
        }
        let mut records = Vec::new();
        records
            .try_reserve_exact(count)
            .map_err(|_| OUT_OF_MEMORY)?;
        for allocation in self.allocations.values() {
            let handles = self
                .handles
                .values()
                .filter(|id| **id == allocation.id)
                .count() as u32;
            let record = Record::Allocation {
                id: allocation.id,
                creator: allocation.creator,
                content: allocation.creator
                    && allocation.shared
                    && allocation.properties.handle_types != 0
                    && allocation.properties.kind == 1
                    && allocation.properties.location.kind == 1,
                size: allocation.size as u64,
                allocation_type: allocation.properties.kind,
                handle_types: allocation.properties.handle_types,
                location_type: allocation.properties.location.kind,
                location_id: allocation.properties.location.id,
                handles,
            };
            records.push(record);
        }
        for mapping in self.mappings.values() {
            if mapping.unknown {
                return Err(crate::driver::CudaError(NOT_SUPPORTED));
            }
            if self.multicasts.contains_key(&mapping.id) {
                continue;
            }
            let mut access: Vec<_> = mapping
                .access
                .iter()
                .map(|access| cuinterpose_protocol::Access {
                    location_type: access.location.kind,
                    location_id: access.location.id,
                    flags: u64::from(access.flags),
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
            return Err(crate::driver::CudaError(NOT_READY));
        }
        if self.unsupported != 0 || !self.raw.is_empty() {
            return Err(crate::driver::CudaError(NOT_SUPPORTED));
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
        let selected = |a: &Allocation| {
            a.creator
                && a.shared
                && a.properties.handle_types != 0
                && a.properties.kind == 1
                && a.properties.location.kind == 1
        };
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
                    .filter(|a| selected(a))
                    .map(|a| a.id)
                    .collect();
                let mut recovered = Vec::new();
                let saved = (|| -> Result<(Option<Arena>, u32)> {
                    let mut allocations = Vec::new();
                    for id in ids {
                        let allocation = self.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
                        if allocation.driver.is_none() {
                            let mapping = self
                                .mappings
                                .values()
                                .find(|m| m.id == id)
                                .ok_or(INVALID_HANDLE)?;
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
                            .ok_or(OUT_OF_MEMORY)?;
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
                for allocation in self.allocations.values_mut().filter(|a| selected(a)) {
                    allocation.host_checkpointed = true;
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
                    .filter(|a| a.host_checkpointed)
                    .map(AllocationContent::from)
                    .collect();
                bytes = allocations.iter().try_fold(0u64, |sum, a| {
                    sum.checked_add(a.size as u64).ok_or(OUT_OF_MEMORY)
                })?;
                if let Some(arena) = &self.arena {
                    copy_us = arena.load(&mut allocations)?;
                } else if !allocations.is_empty() {
                    return Err(crate::driver::CudaError(INVALID_VALUE));
                }
                for allocation in allocations {
                    self.allocations
                        .get_mut(&allocation.id)
                        .ok_or(INVALID_HANDLE)?
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
                    let raw = ticket::request(&allocation.ticket).map_err(|_| INVALID_HANDLE)?;
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
            _ => return Err(crate::driver::CudaError(NOT_SUPPORTED)),
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
                                allocation.driver.ok_or(INVALID_HANDLE)?,
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
                    if creator {
                        let fd =
                            crate::driver::export_posix(allocation.driver.ok_or(INVALID_HANDLE)?)?;
                        cache()?.replace((ResourceKind::Unicast, allocation.id), Some(fd))?;
                    }
                    if !self.handles.values().any(|id| *id == allocation.id) {
                        unsafe {
                            crate::driver::cuMemRelease(allocation.driver.ok_or(INVALID_HANDLE)?)
                        }?;
                        allocation.driver = None;
                    }
                    allocation.checkpointed = false;
                    allocation.host_checkpointed = false;
                    Ok(())
                },
            )?;
        }
        Ok(())
    }

    pub(super) fn mint(&mut self, id: AllocationId) -> Result<u64> {
        if self.next & HANDLE_MASK != 0 {
            return Err(crate::driver::CudaError(OUT_OF_MEMORY));
        }
        let handle = HANDLE_TAG | self.next;
        self.next += 1;
        self.handles.insert(handle, id);
        Ok(handle)
    }

    pub fn stats(&self) -> DebugStats {
        DebugStats {
            allocations: self.allocations.len() as u64,
            // Preserve the debug ABI: these two counts describe unicast only.
            handles: self
                .handles
                .values()
                .filter(|id| self.allocations.contains_key(*id))
                .count() as u64,
            mappings: self
                .mappings
                .values()
                .filter(|mapping| self.allocations.contains_key(&mapping.id))
                .count() as u64,
            multicasts: self.multicasts.len() as u64,
            cached_exports: cache().and_then(|cache| cache.len()).unwrap_or(0) as u64,
            live_raw_imports: self.raw.values().map(|n| u64::from(*n)).sum(),
            unsupported_exportable_creations: self.unsupported,
            phase: (if super::G_FAILED.load(Ordering::Acquire) {
                DebugPhase::Failed
            } else {
                match self.phase {
                    Phase::Active => DebugPhase::Active,
                    Phase::MulticastPrepared | Phase::AllocationsSaved => DebugPhase::Preparing,
                    Phase::UnicastPrepared => DebugPhase::Prepared,
                    _ => DebugPhase::Restoring,
                }
            }) as u32,
        }
    }

    fn settle(&mut self, id: AllocationId) -> Result<()> {
        if self.multicasts.contains_key(&id) {
            return super::multicast::settle(self, id);
        }
        let handle_live = self.handles.values().any(|value| *value == id);
        let mapped = self.mappings.values().any(|mapping| mapping.id == id);
        let allocation = self.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
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
        let end = address.checked_add(size as u64).ok_or(INVALID_VALUE)?;
        for &(base, length) in &self.pending_maps {
            if base < end && base + length as u64 > address {
                return Err(crate::driver::CudaError(NOT_READY));
            }
        }
        let mut result = Vec::new();
        for (base, mapping) in &self.mappings {
            let limit = base.checked_add(mapping.size as u64).ok_or(INVALID_VALUE)?;
            if *base < end && limit > address {
                if *base < address || limit > end {
                    return Err(crate::driver::CudaError(INVALID_VALUE));
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
            return Err(crate::driver::CudaError(NOT_INITIALIZED));
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
        return Err(crate::driver::CudaError(UNKNOWN));
    }
    let _initializing = match G_INITIALIZING.try_lock() {
        Ok(guard) => guard,
        Err(TryLockError::WouldBlock) => return Err(crate::driver::CudaError(NOT_INITIALIZED)),
        Err(TryLockError::Poisoned(poison)) if G_CHILD.load(Ordering::Acquire) => {
            poison.into_inner()
        }
        Err(TryLockError::Poisoned(_)) => return Err(crate::driver::CudaError(UNKNOWN)),
    };
    if !G_STATE.load(Ordering::Acquire).is_null() {
        return Ok(());
    }
    if super::G_FAILED.load(Ordering::Acquire) {
        return Err(crate::driver::CudaError(UNKNOWN));
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
        || super::G_HOST
            .get()
            .is_some_and(|host| host.origin_pid != pid)
    {
        Err(std::env::VarError::NotPresent)
    } else {
        std::env::var("CUINTERPOSE_PARTICIPANT_ID")
    };
    let identity = match configured {
        Ok(value) => value.parse().map_err(|_| INVALID_VALUE)?,
        Err(std::env::VarError::NotPresent) => ParticipantId(random()?),
        Err(_) => return Err(crate::driver::CudaError(INVALID_VALUE)),
    };
    let directory =
        std::env::var("SNAPSHOT_CONTROL_DIR").unwrap_or_else(|_| "/snapshot-control".into());
    if !directory.starts_with('/') {
        return Err(crate::driver::CudaError(INVALID_VALUE));
    }
    let endpoint = format!("{directory}/cuinterpose-{pid}.sock");
    std::os::unix::net::SocketAddr::from_pathname(&endpoint).map_err(|_| INVALID_VALUE)?;
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
    let state = generation.state.get_mut().map_err(|_| UNKNOWN)?;
    super::control::start(&state.endpoint, state.identity)?;
    // No fallible work follows successful startup. Failed startup drops only
    // the unpublished, empty generation; no CUDA resources have been created.
    G_STATE.store(Box::into_raw(generation), Ordering::Release);
    Ok(())
}

pub fn get() -> Result<MutexGuard<'static, State>> {
    if super::G_FAILED.load(Ordering::Acquire) {
        return Err(crate::driver::CudaError(UNKNOWN));
    }
    let pointer = G_STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return Err(crate::driver::CudaError(NOT_INITIALIZED));
    }
    let state = unsafe { &*pointer }.state.lock().map_err(|_| UNKNOWN)?;
    // A caller may have waited behind a failed lifecycle operation. Do not
    // admit queued mutations using only the pre-lock check.
    if super::G_FAILED.load(Ordering::Acquire) {
        return Err(crate::driver::CudaError(UNKNOWN));
    }
    Ok(state)
}

pub(super) fn active() -> Result<MutexGuard<'static, State>> {
    let state = get()?;
    if state.phase != Phase::Active {
        return Err(crate::driver::CudaError(NOT_READY));
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

pub fn cuMemCreate(
    out: *mut u64,
    size: usize,
    prop: *const AllocationProp,
    flags: u64,
) -> Result<()> {
    if out.is_null() || prop.is_null() {
        return Err(crate::driver::CudaError(INVALID_VALUE));
    }
    let properties = unsafe { *prop };
    let mut state = get()?;
    if properties.handle_types == 1 && state.phase != Phase::Active {
        return Err(crate::driver::CudaError(NOT_READY));
    }
    // Reserve the logical identity before acquiring backing. Recoverable
    // metadata errors must not leave an unpublished CUDA allocation behind.
    let tracked = if properties.handle_types == 1 {
        if state.next & HANDLE_MASK != 0 {
            return Err(crate::driver::CudaError(OUT_OF_MEMORY));
        }
        Some(AllocationId(random()?))
    } else {
        None
    };
    let mut driver = 0;
    let create = crate::driver::symbols::cuMemCreate()?;
    if let Err(error) =
        crate::driver::CudaError::result(unsafe { create(&mut driver, size, prop, flags) })
    {
        unsafe {
            out.write(driver);
        }
        return Err(error);
    }
    if driver & HANDLE_MASK == HANDLE_TAG {
        let _ = unsafe { crate::driver::cuMemRelease(driver) };
        return Err(crate::driver::CudaError(INVALID_HANDLE));
    }
    if properties.handle_types != 1 {
        if properties.handle_types != 0 {
            state.unsupported += 1;
        }
        unsafe {
            out.write(driver);
        }
        return Ok(());
    }
    let id = tracked.ok_or(INVALID_HANDLE)?;
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
        host_checkpointed: false,
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
        return Err(crate::driver::CudaError(NOT_READY));
    }
    if let Some(id) = state.handles.remove(&handle) {
        if let Err(error) = state.settle(id) {
            state.handles.insert(handle, id);
            return Err(error);
        }
    } else {
        if handle & HANDLE_MASK == HANDLE_TAG {
            return Err(crate::driver::CudaError(INVALID_HANDLE));
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
        return Err(crate::driver::CudaError(INVALID_VALUE));
    }
    let mut state = get()?;
    // CUDA may already have mapped a multicast range while its record is
    // still pending publication. Do not let the native fallback expose that
    // object's real driver handle before we can return a tracked alias.
    for &(base, size) in &state.pending_maps {
        let end = base.checked_add(size as u64).ok_or(INVALID_VALUE)?;
        if (address as u64) >= base && (address as u64) < end {
            return Err(crate::driver::CudaError(NOT_READY));
        }
    }
    let id = state
        .mappings
        .values()
        .find(|m| address as u64 >= m.address && (address as u64) - m.address < m.size as u64)
        .map(|m| m.id);
    if let Some(id) = id {
        if state.phase != Phase::Active {
            return Err(crate::driver::CudaError(NOT_READY));
        }
        if state.next & HANDLE_MASK != 0 {
            return Err(crate::driver::CudaError(OUT_OF_MEMORY));
        }
        if let Some(object) = state.multicasts.get(&id) {
            if object.driver.is_none() {
                return Err(crate::driver::CudaError(INVALID_HANDLE));
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
        let allocation = state.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
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
        if driver & HANDLE_MASK == HANDLE_TAG {
            let _ = unsafe { crate::driver::cuMemRelease(driver) };
            return Err(crate::driver::CudaError(INVALID_HANDLE));
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
        if handle & HANDLE_MASK == HANDLE_TAG {
            return Err(crate::driver::CudaError(INVALID_HANDLE));
        }
        // Native handles must not overwrite tracked or pending ranges while
        // those mappings are temporarily absent from CUDA during checkpoint.
        if !state.covered(address, size)?.is_empty() {
            return Err(crate::driver::CudaError(INVALID_VALUE));
        }
        unsafe { crate::driver::cuMemMap(address, size, offset, handle, flags) }?;
        return Ok(());
    };
    if state.phase != Phase::Active {
        return Err(crate::driver::CudaError(NOT_READY));
    }
    if state.multicasts.contains_key(&id) {
        return super::multicast::map(state, id, address, size, offset, flags);
    }
    if size == 0 || !state.covered(address, size)?.is_empty() {
        return Err(crate::driver::CudaError(INVALID_VALUE));
    }
    let allocation = state.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
    unsafe {
        crate::driver::cuMemMap(
            address,
            size,
            offset,
            allocation.driver.ok_or(INVALID_HANDLE)?,
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
        return Err(crate::driver::CudaError(NOT_READY));
    }
    for base in &mappings {
        let id = &state.mappings[base].id;
        if state.multicasts.get(id).is_some_and(|a| a.inflight != 0)
            || state.allocations.get(id).is_some_and(|a| a.pins != 0)
        {
            return Err(crate::driver::CudaError(NOT_READY));
        }
    }
    unsafe { crate::driver::cuMemUnmap(address, size) }?;
    for base in mappings {
        let mapping = state.mappings.remove(&base).ok_or(INVALID_VALUE)?;
        state.settle(mapping.id)?;
    }
    Ok(())
}

pub fn cuMemSetAccess(
    address: u64,
    size: usize,
    access: *const Access,
    count: usize,
) -> Result<()> {
    let mut state = get()?;
    let mappings = state.covered(address, size)?;
    if !mappings.is_empty() && state.phase != Phase::Active {
        return Err(crate::driver::CudaError(NOT_READY));
    }
    if mappings.is_empty() || access.is_null() {
        unsafe { crate::driver::cuMemSetAccess(address, size, access, count) }?;
        return Ok(());
    }
    if count > isize::MAX as usize / size_of::<Access>() {
        return Err(crate::driver::CudaError(INVALID_VALUE));
    }
    let descriptors = unsafe { std::slice::from_raw_parts(access, count) };
    let mut merged = Vec::new();
    for base in &mappings {
        let mut entries = state.mappings[base].access.clone();
        for descriptor in descriptors {
            entries.retain(|entry| {
                entry.location.kind != descriptor.location.kind
                    || entry.location.id != descriptor.location.id
            });
            if descriptor.flags != 0 {
                entries.push(*descriptor);
            }
            if entries.len() > 32 {
                return Err(crate::driver::CudaError(NOT_SUPPORTED));
            }
        }
        merged.push(entries);
    }
    let result = unsafe { crate::driver::cuMemSetAccess(address, size, access, count) };
    for (base, entries) in mappings.iter().zip(merged) {
        let mapping = state.mappings.get_mut(base).ok_or(INVALID_VALUE)?;
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
    kind: u32,
    flags: u64,
) -> Result<()> {
    let mut state = get()?;
    let Some(id) = state.handles.get(&handle).copied() else {
        if handle & HANDLE_MASK == HANDLE_TAG {
            return Err(crate::driver::CudaError(INVALID_HANDLE));
        }
        unsafe { crate::driver::cuMemExportToShareableHandle(out, handle, kind, flags) }?;
        return Ok(());
    };
    if state.phase != Phase::Active {
        return Err(crate::driver::CudaError(NOT_READY));
    }
    if out.is_null() || kind != 1 || flags != 0 {
        return Err(crate::driver::CudaError(INVALID_VALUE));
    }
    if state.multicasts.contains_key(&id) {
        return super::multicast::export(&mut state, id, out);
    }
    let allocation = state.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
    if allocation.creator && !cache()?.contains(&(ResourceKind::Unicast, id))? {
        let fd = crate::driver::export_posix(allocation.driver.ok_or(INVALID_HANDLE)?)?;
        cache()?.replace((ResourceKind::Unicast, id), Some(fd))?;
    }
    let ticket = ticket::export(&allocation.ticket).map_err(|_| OUT_OF_MEMORY)?;
    allocation.shared = true;
    if allocation.context == 0 {
        allocation.context = context();
    }
    unsafe {
        out.cast::<i32>().write(ticket.into_raw_fd());
    }
    Ok(())
}

pub fn cuMemImportFromShareableHandle(out: *mut u64, fd: *mut c_void, kind: u32) -> Result<()> {
    if out.is_null() {
        return Err(crate::driver::CudaError(INVALID_VALUE));
    }
    let ticket = if kind == 1 {
        ticket::read(fd as isize as i32).map_err(|_| INVALID_HANDLE)?
    } else {
        None
    };
    let mut state = get()?;
    let Some(ticket) = ticket else {
        let mut driver = 0;
        unsafe { crate::driver::cuMemImportFromShareableHandle(&mut driver, fd, kind) }?;
        if driver & HANDLE_MASK == HANDLE_TAG {
            let _ = unsafe { crate::driver::cuMemRelease(driver) };
            return Err(crate::driver::CudaError(INVALID_HANDLE));
        }
        *state.raw.entry(driver).or_insert(0) += 1;
        unsafe {
            out.write(driver);
        }
        return Ok(());
    };
    if state.phase != Phase::Active {
        return Err(crate::driver::CudaError(NOT_READY));
    }
    if state.next & HANDLE_MASK != 0 {
        return Err(crate::driver::CudaError(OUT_OF_MEMORY));
    }
    if matches!(ticket.resource, Resource::Multicast { .. }) {
        return super::multicast::import(state, out, ticket);
    }
    let id = ticket.allocation;
    if state.multicasts.contains_key(&id) {
        return Err(crate::driver::CudaError(INVALID_HANDLE));
    }
    if let Some(allocation) = state.allocations.get_mut(&id) {
        if allocation.ticket != ticket {
            return Err(crate::driver::CudaError(INVALID_VALUE));
        }
        if allocation.driver.is_none() {
            let raw = ticket::request(&ticket).map_err(|_| INVALID_HANDLE)?;
            let driver = crate::driver::import_posix(raw.as_fd())?;
            allocation.driver = Some(driver);
        }
        allocation.shared = true;
        unsafe {
            out.write(state.mint(id)?);
        }
        return Ok(());
    }
    // EXPORT service uses only CACHE, never STATE, so a same-process request
    // can complete while this call holds its allocation metadata lock.
    let raw = ticket::request(&ticket).map_err(|_| INVALID_HANDLE)?;
    let driver = crate::driver::import_posix(raw.as_fd())?;
    let mut properties = std::mem::MaybeUninit::<AllocationProp>::zeroed();
    let recorded = (|| -> Result<u64> {
        if driver & HANDLE_MASK == HANDLE_TAG {
            return Err(crate::driver::CudaError(INVALID_HANDLE));
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
            host_checkpointed: false,
            pins: 0,
        },
    );
    unsafe {
        out.write(logical);
    }
    Ok(())
}

pub fn cuMemGetAllocationPropertiesFromHandle(out: *mut AllocationProp, handle: u64) -> Result<()> {
    let state = get()?;
    if state.handles.contains_key(&handle) && state.phase != Phase::Active {
        return Err(crate::driver::CudaError(NOT_READY));
    }
    let driver = match state.handles.get(&handle) {
        Some(id) => match state.multicasts.get(id) {
            Some(object) => object.driver.ok_or(INVALID_HANDLE)?,
            None => state.allocations[id].driver.ok_or(INVALID_HANDLE)?,
        },
        None if handle & HANDLE_MASK == HANDLE_TAG => {
            return Err(crate::driver::CudaError(INVALID_HANDLE));
        }
        None => handle,
    };
    unsafe { crate::driver::cuMemGetAllocationPropertiesFromHandle(out, driver) }?;
    Ok(())
}

pub use super::multicast::{
    cuMulticastAddDevice, cuMulticastBindAddr, cuMulticastBindAddr_v2, cuMulticastBindMem,
    cuMulticastBindMem_v2, cuMulticastCreate, cuMulticastGetGranularity, cuMulticastUnbind,
};
