// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Canonical bytes in CRIU-captured memory. Unpublished backing is rolled back
//! explicitly; CUDA cleanup never runs from Drop or in a fork child.

use super::state::{Allocation, Result, call, invoke};
use cuinterpose_abi::*;
use cuinterpose_protocol::AllocationId;
use std::collections::BTreeMap;
use std::ffi::c_void;
use std::time::{Duration, Instant};

#[derive(Default)]
pub struct Transfer {
    pub bytes: u64,
    pub copy_us: u32,
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::{CStr, c_char};
    use std::sync::atomic::{AtomicUsize, Ordering};

    static G_REGISTERED: AtomicUsize = AtomicUsize::new(0);
    static G_REGISTER_CALLS: AtomicUsize = AtomicUsize::new(0);
    static G_RELEASED: AtomicUsize = AtomicUsize::new(0);
    static G_SWITCHED: AtomicUsize = AtomicUsize::new(0);

    unsafe extern "C" fn current(output: *mut *mut c_void) -> i32 {
        unsafe {
            output.write(std::ptr::dangling_mut::<c_void>());
        }
        SUCCESS
    }
    unsafe extern "C" fn switch(_: *mut c_void) -> i32 {
        G_SWITCHED.fetch_add(1, Ordering::Relaxed);
        711
    }
    unsafe extern "C" fn retain(output: *mut *mut c_void, _: i32) -> i32 {
        unsafe {
            output.write(2usize as *mut c_void);
        }
        SUCCESS
    }
    unsafe extern "C" fn release(_: i32) -> i32 {
        G_RELEASED.fetch_add(1, Ordering::Relaxed);
        712
    }
    unsafe extern "C" fn register(_: *mut c_void, _: usize, _: u32) -> i32 {
        G_REGISTERED.fetch_add(1, Ordering::Relaxed);
        G_REGISTER_CALLS.fetch_add(1, Ordering::Relaxed);
        SUCCESS
    }
    unsafe extern "C" fn unregister(_: *mut c_void) -> i32 {
        G_REGISTERED.fetch_sub(1, Ordering::Relaxed);
        SUCCESS
    }
    unsafe extern "C" fn create(_: *mut u64, _: usize, _: *const AllocationProp, _: u64) -> i32 {
        713
    }
    unsafe extern "C" fn resolve(name: *const c_char) -> *mut c_void {
        match unsafe { CStr::from_ptr(name) }.to_bytes() {
            b"cuCtxGetCurrent" => current as *const () as *mut c_void,
            b"cuCtxSetCurrent" => switch as *const () as *mut c_void,
            b"cuDevicePrimaryCtxRetain" => retain as *const () as *mut c_void,
            b"cuDevicePrimaryCtxRelease_v2" => release as *const () as *mut c_void,
            b"cuMemHostRegister_v2" => register as *const () as *mut c_void,
            b"cuMemHostUnregister" => unregister as *const () as *mut c_void,
            b"cuMemCreate" => create as *const () as *mut c_void,
            // Deliberately absent, not merely a driver error.
            b"cuMemHostGetFlags" => std::ptr::null_mut(),
            _ => std::ptr::null_mut(),
        }
    }

    #[test]
    fn missing_registration_query_and_primary_cleanup_preserve_ownership() {
        // HOST is process-lifetime production state. Keep this fake resolver
        // out of the parallel ABI-prefix tests, which require an unset HOST.
        if std::env::var_os("CUINTERPOSE_CARRIER_UNIT_CHILD").is_none() {
            let status = std::process::Command::new(std::env::current_exe().unwrap())
                .args(["--exact", "host_carrier::tests::missing_registration_query_and_primary_cleanup_preserve_ownership"])
                .env("CUINTERPOSE_CARRIER_UNIT_CHILD", "1")
                .status().unwrap();
            assert!(status.success());
            return;
        }
        assert!(
            crate::G_HOST
                .set(Host {
                    version: ABI_VERSION,
                    size: size_of::<Host>() as u32,
                    resolve,
                    origin_pid: unsafe { libc::getpid() },
                })
                .is_ok()
        );
        Context::enter(1, 0).unwrap().leave().unwrap();
        assert_eq!(G_SWITCHED.load(Ordering::Relaxed), 0);
        assert_eq!(Context::enter(0, 0).err(), Some(711));
        assert_eq!(G_RELEASED.load(Ordering::Relaxed), 1);
        let id = AllocationId([1; 16]);
        let arena = Arena {
            base: 0x1000,
            size: 4096,
            context: 1,
            device: 0,
            offsets: BTreeMap::from([(id, 0)]),
        };
        let mut allocations = [Allocation {
            id,
            driver: None,
            size: 4096,
            properties: AllocationProp {
                kind: 1,
                handle_types: 1,
                location: Location { kind: 1, id: 0 },
                win32_metadata: std::ptr::null_mut(),
                flags: AllocationFlags::default(),
            },
            ticket: cuinterpose_protocol::Ticket {
                creator: cuinterpose_protocol::ParticipantId::default(),
                allocation: id,
                endpoint: "/test".into(),
                resource: cuinterpose_protocol::Resource::Unicast,
            },
            creator: true,
            shared: true,
            context: 1,
            checkpointed: true,
            host_checkpointed: true,
            pins: 0,
        }];
        assert_eq!(arena.load(&mut allocations), Err(713));
        assert_eq!(G_REGISTER_CALLS.load(Ordering::Relaxed), 1);
        assert_eq!(G_REGISTERED.load(Ordering::Relaxed), 0);
        assert_eq!(allocations[0].driver, None);
    }
}

pub struct Context {
    previous: *mut c_void,
    primary: Option<i32>,
    changed: bool,
}

impl Context {
    pub fn enter(context: usize, device: i32) -> Result<Self> {
        let mut previous = std::ptr::null_mut();
        call!("cuCtxGetCurrent", fn(*mut *mut c_void), &mut previous);
        let mut target = context as *mut c_void;
        let mut primary = None;
        if target.is_null() {
            call!(
                "cuDevicePrimaryCtxRetain",
                fn(*mut *mut c_void, i32),
                &mut target,
                device
            );
            primary = Some(device);
        }
        let changed = target != previous;
        if changed && let Err(error) = invoke!("cuCtxSetCurrent", fn(*mut c_void), target) {
            if let Some(device) = primary {
                let _ = invoke!("cuDevicePrimaryCtxRelease_v2", fn(i32), device);
            }
            return Err(error);
        }
        Ok(Self {
            previous,
            primary,
            changed,
        })
    }

    pub fn leave(self) -> Result<()> {
        let mut result = Ok(());
        if self.changed {
            result = invoke!("cuCtxSetCurrent", fn(*mut c_void), self.previous);
        }
        if let Some(device) = self.primary {
            result = result.and(invoke!("cuDevicePrimaryCtxRelease_v2", fn(i32), device));
        }
        result
    }
}

#[derive(Clone)]
pub struct Arena {
    pub(super) base: usize,
    pub(super) size: usize,
    context: usize,
    device: i32,
    offsets: BTreeMap<AllocationId, usize>,
}

impl Arena {
    pub fn save(allocations: &[Allocation]) -> Result<(Option<Self>, u32)> {
        if allocations.is_empty() {
            return Ok((None, 0));
        }
        let mut offsets = BTreeMap::new();
        let mut size = 0usize;
        for allocation in allocations {
            if allocation.driver.is_none()
                || allocation.size == 0
                || offsets.insert(allocation.id, size).is_some()
            {
                return Err(INVALID_VALUE);
            }
            size = size.checked_add(allocation.size).ok_or(OUT_OF_MEMORY)?;
        }
        let base = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                size,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        if base == libc::MAP_FAILED {
            return Err(OUT_OF_MEMORY);
        }
        let first = &allocations[0];
        let arena = Self {
            base: base as usize,
            size,
            context: first.context,
            device: first.properties.location.id,
            offsets,
        };
        let context = match Context::enter(arena.context, arena.device) {
            Ok(context) => context,
            Err(error) => {
                unsafe {
                    libc::munmap(base, size);
                }
                return Err(error);
            }
        };
        let registered = invoke!(
            "cuMemHostRegister_v2",
            fn(*mut c_void, usize, u32),
            base,
            size,
            1
        );
        let left = context.leave();
        if let Err(error) = registered {
            unsafe {
                libc::munmap(base, size);
            }
            return Err(error);
        }
        if let Err(error) = left {
            let _ = arena.release();
            return Err(error);
        }
        match arena.copy(allocations, false) {
            Ok(elapsed) => Ok((Some(arena), elapsed)),
            Err(error) => {
                let _ = arena.release();
                Err(error)
            }
        }
    }

    /// Keep every fresh handle private until all copies and staging cleanup
    /// succeed. A handle value of zero is valid; None alone means no ownership.
    pub fn load(&self, allocations: &mut [Allocation]) -> Result<u32> {
        let mut fresh = allocations.to_vec();
        let mut size = 0usize;
        for allocation in &fresh {
            if allocation.driver.is_some() || self.offsets.get(&allocation.id) != Some(&size) {
                return Err(INVALID_VALUE);
            }
            size = size.checked_add(allocation.size).ok_or(INVALID_VALUE)?;
        }
        if size != self.size {
            return Err(INVALID_VALUE);
        }
        let mut registered = false;
        let loaded = (|| -> Result<u32> {
            let context = Context::enter(self.context, self.device)?;
            let registration = (|| -> Result<()> {
                let mut flags = 0u32;
                let address = super::driver(c"cuMemHostGetFlags");
                let valid = if address.is_null() {
                    false
                } else {
                    let get_flags: unsafe extern "C" fn(*mut u32, *mut c_void) -> i32 =
                        unsafe { std::mem::transmute(address) };
                    unsafe { get_flags(&mut flags, self.base as *mut c_void) == SUCCESS }
                };
                if !valid {
                    call!(
                        "cuMemHostRegister_v2",
                        fn(*mut c_void, usize, u32),
                        self.base as *mut c_void,
                        self.size,
                        1
                    );
                    registered = true;
                }
                Ok(())
            })();
            let left = context.leave();
            registration.and(left)?;
            for allocation in &mut fresh {
                let context =
                    Context::enter(allocation.context, allocation.properties.location.id)?;
                let mut driver = 0;
                let created = invoke!(
                    "cuMemCreate",
                    fn(*mut u64, usize, *const AllocationProp, u64),
                    &mut driver,
                    allocation.size,
                    &allocation.properties,
                    0
                );
                if created.is_ok() {
                    allocation.driver = Some(driver);
                }
                let left = context.leave();
                created.and(left)?;
            }
            self.copy(&fresh, true)
        })();
        match loaded {
            Ok(elapsed) => {
                for (allocation, fresh) in allocations.iter_mut().zip(fresh) {
                    allocation.driver = fresh.driver;
                }
                Ok(elapsed)
            }
            Err(error) => {
                for allocation in fresh {
                    if let Some(driver) = allocation.driver
                        && let Ok(context) =
                            Context::enter(allocation.context, allocation.properties.location.id)
                    {
                        let _ = invoke!("cuMemRelease", fn(u64), driver);
                        let _ = context.leave();
                    }
                }
                if registered && let Ok(context) = Context::enter(self.context, self.device) {
                    let _ = invoke!(
                        "cuMemHostUnregister",
                        fn(*mut c_void),
                        self.base as *mut c_void
                    );
                    let _ = context.leave();
                }
                Err(error)
            }
        }
    }

    fn copy(&self, allocations: &[Allocation], load: bool) -> Result<u32> {
        let mut groups: BTreeMap<(usize, i32), Vec<&Allocation>> = BTreeMap::new();
        for allocation in allocations {
            groups
                .entry((allocation.context, allocation.properties.location.id))
                .or_default()
                .push(allocation);
        }
        let mut elapsed = Duration::ZERO;
        for ((context, device), group) in groups {
            let total = group.iter().try_fold(0usize, |sum, a| {
                sum.checked_add(a.size).ok_or(OUT_OF_MEMORY)
            })?;
            let mut mapped = Vec::new();
            mapped
                .try_reserve_exact(group.len())
                .map_err(|_| OUT_OF_MEMORY)?;
            let context = Context::enter(context, device)?;
            let mut reserved = None;
            let mut stream = None;
            let mut synchronized = false;
            let transfer = (|| -> Result<()> {
                let mut base = 0u64;
                call!(
                    "cuMemAddressReserve",
                    fn(*mut u64, usize, usize, u64, u64),
                    &mut base,
                    total,
                    0,
                    0,
                    0
                );
                reserved = Some(base);
                let mut offset = 0usize;
                for allocation in &group {
                    let address = base.checked_add(offset as u64).ok_or(INVALID_VALUE)?;
                    call!(
                        "cuMemMap",
                        fn(u64, usize, usize, u64, u64),
                        address,
                        allocation.size,
                        0,
                        allocation.driver.ok_or(INVALID_HANDLE)?,
                        0
                    );
                    mapped.push((address, allocation.size));
                    let access = Access {
                        location: allocation.properties.location,
                        flags: 3,
                    };
                    call!(
                        "cuMemSetAccess",
                        fn(u64, usize, *const Access, usize),
                        address,
                        allocation.size,
                        &access,
                        1
                    );
                    offset += allocation.size;
                }
                let mut raw_stream = std::ptr::null_mut::<c_void>();
                call!(
                    "cuStreamCreate",
                    fn(*mut *mut c_void, u32),
                    &mut raw_stream,
                    1
                );
                stream = Some(raw_stream);
                // Staging/context/allocation work is deliberately outside the
                // copy metric, matching the coordinator's copy-throughput label.
                let started = Instant::now();
                let copies = (|| -> Result<()> {
                    for (allocation, (address, _)) in group.iter().zip(&mapped) {
                        let offset = *self.offsets.get(&allocation.id).ok_or(INVALID_HANDLE)?;
                        let host =
                            self.base.checked_add(offset).ok_or(INVALID_VALUE)? as *mut c_void;
                        if load {
                            call!(
                                "cuMemcpyHtoDAsync_v2",
                                fn(u64, *const c_void, usize, *mut c_void),
                                *address,
                                host,
                                allocation.size,
                                raw_stream
                            );
                        } else {
                            call!(
                                "cuMemcpyDtoHAsync_v2",
                                fn(*mut c_void, u64, usize, *mut c_void),
                                host,
                                *address,
                                allocation.size,
                                raw_stream
                            );
                        }
                    }
                    call!("cuStreamSynchronize", fn(*mut c_void), raw_stream);
                    synchronized = true;
                    Ok(())
                })();
                elapsed = elapsed.saturating_add(started.elapsed());
                copies
            })();
            // Evaluate every cleanup even if an earlier one failed. Preserve
            // the original operation error; cleanup failures still fail-stop.
            let mut result = transfer;
            if let Some(stream) = stream {
                if !synchronized {
                    let drained = invoke!("cuStreamSynchronize", fn(*mut c_void), stream);
                    if drained.is_err() {
                        // Completion is unknown: neither rollback nor returning
                        // to a caller may free DMA-referenced memory. Fail-stop
                        // the process without running Rust/CUDA cleanup.
                        let message = b"cuinterpose: CUDA copy completion unknown; terminating without cleanup\n";
                        unsafe {
                            libc::write(
                                libc::STDERR_FILENO,
                                message.as_ptr().cast(),
                                message.len(),
                            );
                            libc::_exit(127);
                        }
                    }
                }
                result = result.and(invoke!("cuStreamDestroy_v2", fn(*mut c_void), stream));
            }
            for (address, size) in mapped {
                result = result.and(invoke!("cuMemUnmap", fn(u64, usize), address, size));
            }
            if let Some(address) = reserved {
                result = result.and(invoke!("cuMemAddressFree", fn(u64, usize), address, total));
            }
            result = result.and(context.leave());
            result?;
        }
        Ok(elapsed.as_micros().min(u128::from(u32::MAX)) as u32)
    }

    pub fn release(self) -> Result<()> {
        let context = Context::enter(self.context, self.device)?;
        let unregistered = invoke!(
            "cuMemHostUnregister",
            fn(*mut c_void),
            self.base as *mut c_void
        );
        let left = context.leave();
        // Do not unmap an arena still registered with CUDA. If unregister
        // succeeded, a context-restoration error must not prevent CPU cleanup.
        let unmapped = if unregistered.is_ok() {
            if unsafe { libc::munmap(self.base as *mut c_void, self.size) } == 0 {
                Ok(())
            } else {
                Err(UNKNOWN)
            }
        } else {
            Ok(())
        };
        unregistered.and(left).and(unmapped)
    }
}
