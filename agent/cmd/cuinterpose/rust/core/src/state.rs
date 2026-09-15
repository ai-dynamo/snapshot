// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::ticket;
use cuinterpose_abi::*;
use cuinterpose_protocol::Ticket;
use cuinterpose_protocol::{AllocationId, Identity};
use std::collections::BTreeMap;
use std::ffi::c_void;
use std::os::fd::{AsRawFd, FromRawFd, IntoRawFd, OwnedFd};
use std::sync::atomic::{AtomicBool, AtomicPtr, Ordering};
use std::sync::{Mutex, MutexGuard, TryLockError};

pub type Result<T> = std::result::Result<T, i32>;
struct Generation {
    state: Mutex<State>,
    cache: super::export_cache::ExportCache,
}
static STATE: AtomicPtr<Generation> = AtomicPtr::new(std::ptr::null_mut());
static INITIALIZING: Mutex<()> = Mutex::new(());
static CHILD: AtomicBool = AtomicBool::new(false);

pub fn fork_snapshot(descriptors: &mut Vec<i32>) -> Option<(usize, usize)> {
    let pointer = STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return None;
    }
    let generation = unsafe { &*pointer };
    generation.cache.fork_descriptors(descriptors);
    let state = generation.state.lock().unwrap_or_else(|e| e.into_inner());
    state.arena.as_ref().map(|arena| (arena.base, arena.size))
}

pub fn fork_child() {
    STATE.store(std::ptr::null_mut(), Ordering::Release);
    CHILD.store(true, Ordering::Release);
    // A lifecycle failure belongs to the abandoned generation. ABI/loader
    // poison remains sticky and is not reset.
    super::FAILED.store(false, Ordering::Release);
}

pub fn cache() -> Result<&'static super::export_cache::ExportCache> {
    let pointer = STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return Err(NOT_INITIALIZED);
    }
    Ok(&unsafe { &*pointer }.cache)
}

macro_rules! call {
    ($name:expr, fn($($ty:ty),*) $(, $arg:expr)* $(,)?) => {{
        let name = std::ffi::CStr::from_bytes_with_nul(concat!($name, "\0").as_bytes()).map_err(|_| NOT_INITIALIZED)?;
        let address = super::driver(name);
        if address.is_null() { return Err(NOT_INITIALIZED); }
        let function: unsafe extern "C" fn($($ty),*) -> i32 = unsafe { std::mem::transmute(address) };
        let code = unsafe { function($($arg),*) };
        if code != SUCCESS { return Err(code); }
    }};
}
pub(super) use call;

#[derive(Clone)]
pub struct Allocation {
    pub id: AllocationId,
    pub ticket: Ticket,
    pub driver: u64,
    pub size: usize,
    pub properties: AllocationProp,
    pub creator: bool,
    pub shared: bool,
    pub context: usize,
}

// AllocationProp's Win32 pointer is opaque and is never dereferenced on Linux.
// Driver access and allocation metadata are serialized under State's mutex.
unsafe impl Send for Allocation {}

pub struct Mapping {
    pub id: AllocationId,
    pub address: u64,
    pub size: usize,
    pub offset: usize,
    pub access: Vec<Access>,
    pub unknown: bool,
}

pub struct State {
    pub identity: Identity,
    pub endpoint: String,
    pub allocations: BTreeMap<AllocationId, Allocation>,
    pub handles: BTreeMap<u64, AllocationId>,
    pub mappings: BTreeMap<u64, Mapping>,
    pub raw: BTreeMap<u64, u32>,
    pub unsupported: u64,
    pub phase: u16,
    pub arena: Option<super::host_carrier::Arena>,
    next: u64,
}

impl State {
    pub fn inspect(&self) -> Result<Vec<cuinterpose_protocol::Record>> {
        use cuinterpose_protocol::{Record, RecordFlags, RecordKind};
        if self.phase != 0 {
            return Err(NOT_READY);
        }
        let mut records = Vec::new();
        for allocation in self.allocations.values() {
            let handles = self
                .handles
                .values()
                .filter(|id| **id == allocation.id)
                .count() as u32;
            let flags = u32::from(allocation.creator)
                | (u32::from(handles != 0) << 1)
                | (u32::from(
                    allocation.creator
                        && allocation.shared
                        && allocation.properties.handle_types != 0
                        && allocation.properties.kind == 1
                        && allocation.properties.location.kind == 1,
                ) << 2);
            let record = Record {
                kind: RecordKind::Allocation,
                flags: RecordFlags(flags),
                allocation_id: allocation.id,
                allocation_size: allocation.size as u64,
                allocation_type: allocation.properties.kind,
                requested_handle_types: allocation.properties.handle_types,
                allocation_location_type: allocation.properties.location.kind,
                allocation_location_id: allocation.properties.location.id,
                application_handle_count: handles,
                ..Record::default()
            };
            records.push(record);
        }
        for mapping in self.mappings.values() {
            if mapping.unknown {
                return Err(NOT_SUPPORTED);
            }
            let mut record = Record {
                kind: RecordKind::Mapping,
                flags: RecordFlags(u32::from(self.allocations[&mapping.id].creator)),
                allocation_id: mapping.id,
                address: mapping.address,
                size: mapping.size as u64,
                offset: mapping.offset as u64,
                access_count: mapping.access.len() as u32,
                ..Record::default()
            };
            for (index, access) in mapping.access.iter().enumerate() {
                record.access[index] = cuinterpose_protocol::Access {
                    location_type: access.location.kind,
                    location_id: access.location.id,
                    flags: u64::from(access.flags),
                };
            }
            records.push(record);
        }
        Ok(records)
    }

    pub fn lifecycle(&mut self, operation: u16) -> Result<u64> {
        use super::host_carrier::{Arena, Context};
        let expected = match operation {
            3 => 0,
            4 => 3,
            5 => 4,
            7 => 5,
            8 => 7,
            9 => 8,
            10 => 9,
            11 => 10,
            12 => 11,
            _ => return Err(NOT_SUPPORTED),
        };
        if self.phase != expected {
            return Err(NOT_READY);
        }
        if self.unsupported != 0 || !self.raw.is_empty() {
            return Err(NOT_SUPPORTED);
        }
        let selected = |a: &&Allocation| {
            a.creator
                && a.shared
                && a.properties.handle_types != 0
                && a.properties.kind == 1
                && a.properties.location.kind == 1
        };
        let mut bytes = 0u64;
        match operation {
            3 => {
                self.inspect()?;
            }
            4 => {
                let ids: Vec<_> = self
                    .allocations
                    .values()
                    .filter(selected)
                    .map(|a| a.id)
                    .collect();
                let mut allocations = Vec::new();
                for id in ids {
                    let allocation = self.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
                    if allocation.driver == 0 {
                        let mapping = self
                            .mappings
                            .values()
                            .find(|m| m.id == id)
                            .ok_or(INVALID_HANDLE)?;
                        let context =
                            Context::enter(allocation.context, allocation.properties.location.id)?;
                        let retained = (|| -> Result<()> {
                            call!(
                                "cuMemRetainAllocationHandle",
                                fn(*mut u64, *mut c_void),
                                &mut allocation.driver,
                                mapping.address as usize as *mut c_void
                            );
                            Ok(())
                        })();
                        let left = context.leave();
                        retained?;
                        left?;
                    }
                    bytes = bytes
                        .checked_add(allocation.size as u64)
                        .ok_or(OUT_OF_MEMORY)?;
                    allocations.push(allocation.clone());
                }
                self.arena = Arena::save(&allocations)?;
            }
            5 => {
                cache()?.clear()?;
                for allocation in self.allocations.values_mut().filter(|a| a.shared) {
                    let context =
                        Context::enter(allocation.context, allocation.properties.location.id)?;
                    let prepared = (|| -> Result<()> {
                        for mapping in self.mappings.values().filter(|m| m.id == allocation.id) {
                            call!("cuMemUnmap", fn(u64, usize), mapping.address, mapping.size);
                        }
                        if allocation.driver != 0 {
                            call!("cuMemRelease", fn(u64), allocation.driver);
                            allocation.driver = 0;
                        }
                        Ok(())
                    })();
                    let left = context.leave();
                    prepared?;
                    left?;
                }
            }
            7 => {
                let mut allocations: Vec<_> = self
                    .allocations
                    .values()
                    .filter(selected)
                    .cloned()
                    .collect();
                bytes = allocations.iter().try_fold(0u64, |sum, a| {
                    sum.checked_add(a.size as u64).ok_or(OUT_OF_MEMORY)
                })?;
                if let Some(arena) = &self.arena {
                    arena.load(&mut allocations)?;
                } else if !allocations.is_empty() {
                    return Err(INVALID_VALUE);
                }
                for allocation in allocations {
                    self.allocations.insert(allocation.id, allocation);
                }
                self.remap(true)?;
            }
            8 => {
                for allocation in self
                    .allocations
                    .values_mut()
                    .filter(|a| !a.creator && a.shared)
                {
                    let raw = ticket::request(&allocation.ticket).map_err(|_| INVALID_HANDLE)?;
                    let context =
                        Context::enter(allocation.context, allocation.properties.location.id)?;
                    let imported = (|| -> Result<()> {
                        call!(
                            "cuMemImportFromShareableHandle",
                            fn(*mut u64, *mut c_void, u32),
                            &mut allocation.driver,
                            raw.as_raw_fd() as usize as *mut c_void,
                            1
                        );
                        Ok(())
                    })();
                    let left = context.leave();
                    imported?;
                    left?;
                }
                self.remap(false)?;
            }
            9..=12 => {} // No multicast object may pass the unsupported-state gate.
            _ => return Err(NOT_SUPPORTED),
        }
        self.phase = if operation == 12 { 0 } else { operation };
        Ok(bytes)
    }

    fn remap(&self, creator: bool) -> Result<()> {
        for allocation in self
            .allocations
            .values()
            .filter(|a| a.shared && a.creator == creator)
        {
            let context = super::host_carrier::Context::enter(
                allocation.context,
                allocation.properties.location.id,
            )?;
            let replayed = (|| -> Result<()> {
                for mapping in self.mappings.values().filter(|m| m.id == allocation.id) {
                    call!(
                        "cuMemMap",
                        fn(u64, usize, usize, u64, u64),
                        mapping.address,
                        mapping.size,
                        mapping.offset,
                        allocation.driver,
                        0
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
                }
                if creator {
                    let mut fd = -1;
                    call!(
                        "cuMemExportToShareableHandle",
                        fn(*mut c_void, u64, u32, u64),
                        (&mut fd as *mut i32).cast(),
                        allocation.driver,
                        1,
                        0
                    );
                    if fd < 0 {
                        return Err(INVALID_HANDLE);
                    }
                    cache()?.replace(allocation.id, Some(unsafe { OwnedFd::from_raw_fd(fd) }))?;
                }
                Ok(())
            })();
            let left = context.leave();
            replayed?;
            left?;
        }
        Ok(())
    }

    fn mint(&mut self, id: AllocationId) -> Result<u64> {
        if self.next & HANDLE_MASK != 0 {
            return Err(OUT_OF_MEMORY);
        }
        let handle = HANDLE_TAG | self.next;
        self.next += 1;
        self.handles.insert(handle, id);
        Ok(handle)
    }

    pub fn stats(&self) -> DebugStats {
        DebugStats {
            allocations: self.allocations.len() as u64,
            handles: self.handles.len() as u64,
            mappings: self.mappings.len() as u64,
            multicasts: 0,
            cached_exports: cache().and_then(|cache| cache.len()).unwrap_or(0) as u64,
            live_raw_imports: self.raw.values().map(|n| u64::from(*n)).sum(),
            unsupported_exportable_creations: self.unsupported,
            phase: if super::FAILED.load(Ordering::Acquire) {
                5
            } else {
                match self.phase {
                    0 => 1,
                    3 | 4 => 2,
                    5 => 3,
                    _ => 4,
                }
            },
        }
    }

    fn settle(&mut self, id: AllocationId) -> Result<()> {
        let handle_live = self.handles.values().any(|value| *value == id);
        let mapped = self.mappings.values().any(|mapping| mapping.id == id);
        let allocation = self.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
        if !handle_live && allocation.driver != 0 {
            call!("cuMemRelease", fn(u64), allocation.driver);
            allocation.driver = 0;
        }
        if !handle_live && !mapped {
            cache()?.replace(id, None)?;
            self.allocations.remove(&id);
        }
        Ok(())
    }

    fn covered(&self, address: u64, size: usize) -> Result<Vec<u64>> {
        let end = address.checked_add(size as u64).ok_or(INVALID_VALUE)?;
        let mut result = Vec::new();
        for (base, mapping) in &self.mappings {
            let limit = base.checked_add(mapping.size as u64).ok_or(INVALID_VALUE)?;
            if *base < end && limit > address {
                if *base < address || limit > end {
                    return Err(INVALID_VALUE);
                }
                result.push(*base);
            }
        }
        Ok(result)
    }
}

fn random<const N: usize>() -> Result<[u8; N]> {
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
            return Err(NOT_INITIALIZED);
        }
    }
    Ok(bytes)
}

pub fn initialize() -> Result<()> {
    // STATE denotes a ready generation, never one whose listener is still
    // starting. A caller may own the loader lock needed by another initializer,
    // so it must not wait for that initializer's thread/TLS setup.
    if !STATE.load(Ordering::Acquire).is_null() {
        return Ok(());
    }
    if super::FAILED.load(Ordering::Acquire) {
        return Err(UNKNOWN);
    }
    let _initializing = match INITIALIZING.try_lock() {
        Ok(guard) => guard,
        Err(TryLockError::WouldBlock) => return Err(NOT_INITIALIZED),
        Err(TryLockError::Poisoned(poison)) if CHILD.load(Ordering::Acquire) => poison.into_inner(),
        Err(TryLockError::Poisoned(_)) => return Err(UNKNOWN),
    };
    if !STATE.load(Ordering::Acquire).is_null() {
        return Ok(());
    }
    if super::FAILED.load(Ordering::Acquire) {
        return Err(UNKNOWN);
    }
    let result = initialize_generation();
    if result.is_err() {
        // Actual setup failure is sticky; contention above is a transient
        // refusal and must not poison the initializer that is making progress.
        super::FAILED.store(true, Ordering::Release);
    }
    result
}

fn initialize_generation() -> Result<()> {
    let pid = unsafe { libc::getpid() };
    if CHILD.load(Ordering::Acquire) {
        super::process::reset_sockets();
    }
    let configured = if CHILD.load(Ordering::Acquire)
        || super::HOST.get().is_some_and(|host| host.origin_pid != pid)
    {
        Err(std::env::VarError::NotPresent)
    } else {
        std::env::var("CUINTERPOSE_PARTICIPANT_ID")
    };
    let identity = match configured {
        Ok(value) => {
            cuinterpose_protocol::parse_identity(value.as_bytes()).map_err(|_| INVALID_VALUE)?
        }
        Err(std::env::VarError::NotPresent) => {
            let mut id = [0; 33];
            for (index, byte) in random::<16>()?.iter().enumerate() {
                id[index * 2] = b"0123456789abcdef"[(byte >> 4) as usize];
                id[index * 2 + 1] = b"0123456789abcdef"[(byte & 15) as usize];
            }
            id
        }
        Err(_) => return Err(INVALID_VALUE),
    };
    let directory =
        std::env::var("SNAPSHOT_CONTROL_DIR").unwrap_or_else(|_| "/snapshot-control".into());
    if !directory.starts_with('/') {
        return Err(INVALID_VALUE);
    }
    let endpoint = format!("{directory}/cuinterpose-{pid}.sock");
    if endpoint.len() >= 108 {
        return Err(INVALID_VALUE);
    }
    let state = State {
        identity,
        endpoint,
        allocations: BTreeMap::new(),
        handles: BTreeMap::new(),
        mappings: BTreeMap::new(),
        raw: BTreeMap::new(),
        unsupported: 0,
        next: 1,
        phase: 0,
        arena: None,
    };
    let mut generation = Box::new(Generation {
        state: Mutex::new(state),
        cache: super::export_cache::ExportCache::default(),
    });
    let state = generation.state.get_mut().map_err(|_| UNKNOWN)?;
    super::control::start(&state.endpoint, state.identity)?;
    // No fallible work follows successful startup. Failed startup drops only
    // the unpublished, empty generation; no CUDA resources have been created.
    STATE.store(Box::into_raw(generation), Ordering::Release);
    Ok(())
}

pub fn get() -> Result<MutexGuard<'static, State>> {
    if super::FAILED.load(Ordering::Acquire) {
        return Err(UNKNOWN);
    }
    let pointer = STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return Err(NOT_INITIALIZED);
    }
    unsafe { &*pointer }.state.lock().map_err(|_| UNKNOWN)
}

fn active() -> Result<MutexGuard<'static, State>> {
    let state = get()?;
    if state.phase != 0 {
        return Err(NOT_READY);
    }
    Ok(state)
}

fn context() -> usize {
    let mut context = std::ptr::null_mut::<c_void>();
    let address = super::driver(c"cuCtxGetCurrent");
    if address.is_null() {
        return 0;
    }
    let function: unsafe extern "C" fn(*mut *mut c_void) -> i32 =
        unsafe { std::mem::transmute(address) };
    if unsafe { function(&mut context) } != SUCCESS {
        return 0;
    }
    context as usize
}

pub fn cuMemCreate(
    out: *mut u64,
    size: usize,
    prop: *const AllocationProp,
    flags: u64,
) -> Result<i32> {
    if out.is_null() || prop.is_null() {
        return Err(INVALID_VALUE);
    }
    let properties = unsafe { *prop };
    let mut state = active()?;
    let mut driver = 0;
    call!(
        "cuMemCreate",
        fn(*mut u64, usize, *const AllocationProp, u64),
        &mut driver,
        size,
        prop,
        flags
    );
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
    let id = match random() {
        Ok(id) => id,
        Err(error) => {
            call!("cuMemRelease", fn(u64), driver);
            return Err(error);
        }
    };
    let ticket = Ticket {
        creator: state.identity,
        allocation: id,
        endpoint: state.endpoint.clone(),
        resource: 1,
        devices: 0,
        // These ticket fields describe multicast objects only. Unicast size
        // and properties belong to Allocation, not the C v2 ticket.
        size: 0,
        handle_types: 0,
        flags: 0,
    };
    let allocation = Allocation {
        id,
        ticket,
        driver,
        size,
        properties,
        creator: true,
        shared: false,
        context: context(),
    };
    let logical = state.mint(id)?;
    state.allocations.insert(id, allocation);
    unsafe {
        out.write(logical);
    }
    Ok(SUCCESS)
}

pub fn cuMemRelease(handle: u64) -> Result<i32> {
    let mut state = active()?;
    if let Some(id) = state.handles.remove(&handle) {
        state.settle(id)?;
    } else {
        if handle & HANDLE_MASK == HANDLE_TAG {
            return Err(INVALID_HANDLE);
        }
        call!("cuMemRelease", fn(u64), handle);
        if let Some(count) = state.raw.get_mut(&handle) {
            *count -= 1;
            if *count == 0 {
                state.raw.remove(&handle);
            }
        }
    }
    Ok(SUCCESS)
}

pub fn cuMemRetainAllocationHandle(out: *mut u64, address: *mut c_void) -> Result<i32> {
    if out.is_null() {
        return Err(INVALID_VALUE);
    }
    let mut state = active()?;
    let id = state
        .mappings
        .values()
        .find(|m| address as u64 >= m.address && (address as u64) - m.address < m.size as u64)
        .map(|m| m.id);
    let mut driver = 0;
    call!(
        "cuMemRetainAllocationHandle",
        fn(*mut u64, *mut c_void),
        &mut driver,
        address
    );
    if let Some(id) = id {
        let allocation = state.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
        if allocation.driver != 0 {
            call!("cuMemRelease", fn(u64), driver);
        } else {
            allocation.driver = driver;
        }
        unsafe {
            out.write(state.mint(id)?);
        }
    } else {
        if driver & HANDLE_MASK == HANDLE_TAG {
            call!("cuMemRelease", fn(u64), driver);
            return Err(INVALID_HANDLE);
        }
        unsafe {
            out.write(driver);
        }
    }
    Ok(SUCCESS)
}

pub fn cuMemMap(address: u64, size: usize, offset: usize, handle: u64, flags: u64) -> Result<i32> {
    let mut state = active()?;
    let Some(id) = state.handles.get(&handle).copied() else {
        if handle & HANDLE_MASK == HANDLE_TAG {
            return Err(INVALID_HANDLE);
        }
        call!(
            "cuMemMap",
            fn(u64, usize, usize, u64, u64),
            address,
            size,
            offset,
            handle,
            flags
        );
        return Ok(SUCCESS);
    };
    if size == 0 || !state.covered(address, size)?.is_empty() {
        return Err(INVALID_VALUE);
    }
    let allocation = state.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
    call!(
        "cuMemMap",
        fn(u64, usize, usize, u64, u64),
        address,
        size,
        offset,
        allocation.driver,
        flags
    );
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
        },
    );
    Ok(SUCCESS)
}

pub fn cuMemUnmap(address: u64, size: usize) -> Result<i32> {
    let mut state = active()?;
    let mappings = state.covered(address, size)?;
    call!("cuMemUnmap", fn(u64, usize), address, size);
    for base in mappings {
        let mapping = state.mappings.remove(&base).ok_or(INVALID_VALUE)?;
        state.settle(mapping.id)?;
    }
    Ok(SUCCESS)
}

pub fn cuMemSetAccess(
    address: u64,
    size: usize,
    access: *const Access,
    count: usize,
) -> Result<i32> {
    let mut state = active()?;
    let mappings = state.covered(address, size)?;
    if mappings.is_empty() || access.is_null() {
        call!(
            "cuMemSetAccess",
            fn(u64, usize, *const Access, usize),
            address,
            size,
            access,
            count
        );
        return Ok(SUCCESS);
    }
    if count > isize::MAX as usize / size_of::<Access>() {
        return Err(INVALID_VALUE);
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
                return Err(NOT_SUPPORTED);
            }
        }
        merged.push(entries);
    }
    let function = super::driver(c"cuMemSetAccess");
    if function.is_null() {
        return Err(NOT_INITIALIZED);
    }
    let function: unsafe extern "C" fn(u64, usize, *const Access, usize) -> i32 =
        unsafe { std::mem::transmute(function) };
    let result = unsafe { function(address, size, access, count) };
    for (base, entries) in mappings.iter().zip(merged) {
        let mapping = state.mappings.get_mut(base).ok_or(INVALID_VALUE)?;
        if result == SUCCESS {
            mapping.access = entries;
        } else {
            mapping.unknown = true;
        }
    }
    Ok(result)
}

pub fn cuMemExportToShareableHandle(
    out: *mut c_void,
    handle: u64,
    kind: u32,
    flags: u64,
) -> Result<i32> {
    let mut state = active()?;
    let Some(id) = state.handles.get(&handle).copied() else {
        if handle & HANDLE_MASK == HANDLE_TAG {
            return Err(INVALID_HANDLE);
        }
        call!(
            "cuMemExportToShareableHandle",
            fn(*mut c_void, u64, u32, u64),
            out,
            handle,
            kind,
            flags
        );
        return Ok(SUCCESS);
    };
    if out.is_null() || kind != 1 || flags != 0 {
        return Err(INVALID_VALUE);
    }
    let allocation = state.allocations.get_mut(&id).ok_or(INVALID_HANDLE)?;
    if allocation.creator && !cache()?.contains(&id)? {
        let mut fd = -1;
        call!(
            "cuMemExportToShareableHandle",
            fn(*mut c_void, u64, u32, u64),
            (&mut fd as *mut i32).cast(),
            allocation.driver,
            1,
            0
        );
        if fd < 0 {
            return Err(INVALID_HANDLE);
        }
        cache()?.replace(id, Some(unsafe { OwnedFd::from_raw_fd(fd) }))?;
    }
    let ticket = ticket::export(&allocation.ticket).map_err(|_| OUT_OF_MEMORY)?;
    allocation.shared = true;
    if allocation.context == 0 {
        allocation.context = context();
    }
    unsafe {
        out.cast::<i32>().write(ticket.into_raw_fd());
    }
    Ok(SUCCESS)
}

pub fn cuMemImportFromShareableHandle(out: *mut u64, fd: *mut c_void, kind: u32) -> Result<i32> {
    if out.is_null() {
        return Err(INVALID_VALUE);
    }
    let ticket = if kind == 1 {
        ticket::read(fd as isize as i32).ok()
    } else {
        None
    };
    let mut state = active()?;
    let Some(ticket) = ticket else {
        let mut driver = 0;
        call!(
            "cuMemImportFromShareableHandle",
            fn(*mut u64, *mut c_void, u32),
            &mut driver,
            fd,
            kind
        );
        if driver & HANDLE_MASK == HANDLE_TAG {
            call!("cuMemRelease", fn(u64), driver);
            return Err(INVALID_HANDLE);
        }
        *state.raw.entry(driver).or_insert(0) += 1;
        unsafe {
            out.write(driver);
        }
        return Ok(SUCCESS);
    };
    if ticket.resource != 1 {
        return Err(NOT_SUPPORTED);
    }
    let id = ticket.allocation;
    if let Some(allocation) = state.allocations.get_mut(&id) {
        if allocation.driver == 0 {
            let raw = ticket::request(&ticket).map_err(|_| INVALID_HANDLE)?;
            call!(
                "cuMemImportFromShareableHandle",
                fn(*mut u64, *mut c_void, u32),
                &mut allocation.driver,
                raw.as_raw_fd() as usize as *mut c_void,
                1
            );
        }
        allocation.shared = true;
        unsafe {
            out.write(state.mint(id)?);
        }
        return Ok(SUCCESS);
    }
    // EXPORT service uses only CACHE, never STATE, so a same-process request
    // can complete while this call holds its allocation metadata lock.
    let raw = ticket::request(&ticket).map_err(|_| INVALID_HANDLE)?;
    let mut driver = 0;
    call!(
        "cuMemImportFromShareableHandle",
        fn(*mut u64, *mut c_void, u32),
        &mut driver,
        raw.as_raw_fd() as usize as *mut c_void,
        1
    );
    let mut properties = std::mem::MaybeUninit::<AllocationProp>::zeroed();
    call!(
        "cuMemGetAllocationPropertiesFromHandle",
        fn(*mut AllocationProp, u64),
        properties.as_mut_ptr(),
        driver
    );
    let logical = state.mint(id)?;
    state.allocations.insert(
        id,
        Allocation {
            id,
            ticket,
            driver,
            size: 0,
            properties: unsafe { properties.assume_init() },
            creator: false,
            shared: true,
            context: context(),
        },
    );
    unsafe {
        out.write(logical);
    }
    Ok(SUCCESS)
}

pub fn cuMemGetAllocationPropertiesFromHandle(
    out: *mut AllocationProp,
    handle: u64,
) -> Result<i32> {
    let state = active()?;
    let driver = match state.handles.get(&handle) {
        Some(id) => state.allocations[id].driver,
        None if handle & HANDLE_MASK == HANDLE_TAG => return Err(INVALID_HANDLE),
        None => handle,
    };
    call!(
        "cuMemGetAllocationPropertiesFromHandle",
        fn(*mut AllocationProp, u64),
        out,
        driver
    );
    Ok(SUCCESS)
}

// Until multicast replay is implemented, preserve the real runtime behavior
// and mark successful creations unsupported. Inspection refuses checkpoint.
// This is an explicit safety gate, not an implementation of multicast parity.
pub fn cuMulticastCreate(out: *mut u64, prop: *const MulticastProp) -> Result<i32> {
    let mut state = get()?;
    call!(
        "cuMulticastCreate",
        fn(*mut u64, *const MulticastProp),
        out,
        prop
    );
    state.unsupported += 1;
    Ok(SUCCESS)
}

macro_rules! multicast_passthrough {
    ($($name:ident($($arg:ident: $ty:ty),*);)*) => { $(
        pub fn $name($($arg: $ty),*) -> Result<i32> {
            call!(stringify!($name), fn($($ty),*), $($arg),*);
            Ok(SUCCESS)
        }
    )* };
}
multicast_passthrough! {
    cuMulticastAddDevice(handle: u64, device: i32);
    cuMulticastBindAddr(handle: u64, offset: usize, address: u64, size: usize, flags: u64);
    cuMulticastBindAddr_v2(handle: u64, device: i32, offset: usize, address: u64, size: usize, flags: u64);
    cuMulticastGetGranularity(out: *mut usize, prop: *const MulticastProp, flags: u32);
    cuMulticastUnbind(handle: u64, device: i32, offset: usize, size: usize);
}

pub fn cuMulticastBindMem(
    handle: u64,
    offset: usize,
    member: u64,
    member_offset: usize,
    size: usize,
    flags: u64,
) -> Result<i32> {
    let state = get()?;
    let member = match state.handles.get(&member) {
        Some(id) => state.allocations[id].driver,
        None => member,
    };
    drop(state);
    call!(
        "cuMulticastBindMem",
        fn(u64, usize, u64, usize, usize, u64),
        handle,
        offset,
        member,
        member_offset,
        size,
        flags
    );
    Ok(SUCCESS)
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
    let state = get()?;
    let member = match state.handles.get(&member) {
        Some(id) => state.allocations[id].driver,
        None => member,
    };
    drop(state);
    call!(
        "cuMulticastBindMem_v2",
        fn(u64, i32, usize, u64, usize, usize, u64),
        handle,
        device,
        offset,
        member,
        member_offset,
        size,
        flags
    );
    Ok(SUCCESS)
}
