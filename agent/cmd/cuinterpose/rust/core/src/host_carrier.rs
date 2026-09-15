// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Canonical allocation bytes in CRIU-captured anonymous memory. CUDA cleanup
//! is explicit because a failed unmap/unregister is part of lifecycle failure.

use super::state::{Allocation, Result, call};
use cuinterpose_abi::*;
use cuinterpose_protocol::AllocationId;
use std::collections::BTreeMap;
use std::ffi::c_void;

pub struct Context {
    previous: *mut c_void,
    primary: Option<i32>,
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
        let result = (|| -> Result<()> {
            call!("cuCtxSetCurrent", fn(*mut c_void), target);
            Ok(())
        })();
        if let Err(error) = result {
            if let Some(device) = primary {
                call!("cuDevicePrimaryCtxRelease_v2", fn(i32), device);
            }
            return Err(error);
        }
        Ok(Self { previous, primary })
    }

    pub fn leave(self) -> Result<()> {
        let result = (|| -> Result<()> {
            call!("cuCtxSetCurrent", fn(*mut c_void), self.previous);
            Ok(())
        })();
        if let Some(device) = self.primary {
            call!("cuDevicePrimaryCtxRelease_v2", fn(i32), device);
        }
        result
    }
}

pub struct Arena {
    pub(super) base: usize,
    pub(super) size: usize,
    context: usize,
    device: i32,
    offsets: BTreeMap<AllocationId, usize>,
}

impl Arena {
    pub fn save(allocations: &[Allocation]) -> Result<Option<Self>> {
        if allocations.is_empty() {
            return Ok(None);
        }
        let mut offsets = BTreeMap::new();
        let mut size = 0usize;
        for allocation in allocations {
            offsets.insert(allocation.id, size);
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
        let registered = (|| -> Result<()> {
            call!(
                "cuMemHostRegister_v2",
                fn(*mut c_void, usize, u32),
                base,
                size,
                1
            );
            Ok(())
        })();
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
        if let Err(error) = arena.copy(allocations, false) {
            let _ = arena.release();
            return Err(error);
        }
        Ok(Some(arena))
    }

    pub fn load(&self, allocations: &mut [Allocation]) -> Result<()> {
        let context = Context::enter(self.context, self.device)?;
        let registration = (|| -> Result<()> {
            let mut flags = 0u32;
            let address = super::driver(c"cuMemHostGetFlags");
            if address.is_null() {
                return Err(NOT_INITIALIZED);
            }
            let get_flags: unsafe extern "C" fn(*mut u32, *mut c_void) -> i32 =
                unsafe { std::mem::transmute(address) };
            if unsafe { get_flags(&mut flags, self.base as *mut c_void) } != SUCCESS {
                call!(
                    "cuMemHostRegister_v2",
                    fn(*mut c_void, usize, u32),
                    self.base as *mut c_void,
                    self.size,
                    1
                );
            }
            Ok(())
        })();
        let left = context.leave();
        registration?;
        left?;
        for allocation in allocations.iter_mut() {
            let context = Context::enter(allocation.context, allocation.properties.location.id)?;
            let created = (|| -> Result<()> {
                call!(
                    "cuMemCreate",
                    fn(*mut u64, usize, *const AllocationProp, u64),
                    &mut allocation.driver,
                    allocation.size,
                    &allocation.properties,
                    0
                );
                Ok(())
            })();
            let left = context.leave();
            created?;
            left?;
        }
        self.copy(allocations, true)
    }

    fn copy(&self, allocations: &[Allocation], load: bool) -> Result<()> {
        let mut groups: BTreeMap<(usize, i32), Vec<&Allocation>> = BTreeMap::new();
        for allocation in allocations {
            groups
                .entry((allocation.context, allocation.properties.location.id))
                .or_default()
                .push(allocation);
        }
        for ((context, device), group) in groups {
            let context = Context::enter(context, device)?;
            let mut virtual_address = 0u64;
            let mut stream = std::ptr::null_mut::<c_void>();
            let mut mapped = Vec::new();
            let total = group.iter().try_fold(0usize, |sum, a| {
                sum.checked_add(a.size).ok_or(OUT_OF_MEMORY)
            })?;
            let transfer = (|| -> Result<()> {
                call!(
                    "cuMemAddressReserve",
                    fn(*mut u64, usize, usize, u64, u64),
                    &mut virtual_address,
                    total,
                    0,
                    0,
                    0
                );
                call!("cuStreamCreate", fn(*mut *mut c_void, u32), &mut stream, 1);
                let mut offset = 0usize;
                for allocation in group {
                    let address = virtual_address + offset as u64;
                    call!(
                        "cuMemMap",
                        fn(u64, usize, usize, u64, u64),
                        address,
                        allocation.size,
                        0,
                        allocation.driver,
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
                    let host = (self.base
                        + self.offsets.get(&allocation.id).ok_or(INVALID_HANDLE)?)
                        as *mut c_void;
                    if load {
                        call!(
                            "cuMemcpyHtoDAsync_v2",
                            fn(u64, *const c_void, usize, *mut c_void),
                            address,
                            host,
                            allocation.size,
                            stream
                        );
                    } else {
                        call!(
                            "cuMemcpyDtoHAsync_v2",
                            fn(*mut c_void, u64, usize, *mut c_void),
                            host,
                            address,
                            allocation.size,
                            stream
                        );
                    }
                    offset += allocation.size;
                }
                call!("cuStreamSynchronize", fn(*mut c_void), stream);
                Ok(())
            })();
            // Synchronize even after an enqueue failure, before unmapping pages
            // potentially still referenced by an earlier asynchronous copy.
            let cleanup = (|| -> Result<()> {
                if !stream.is_null() {
                    call!("cuStreamSynchronize", fn(*mut c_void), stream);
                    call!("cuStreamDestroy_v2", fn(*mut c_void), stream);
                }
                for (address, size) in mapped {
                    call!("cuMemUnmap", fn(u64, usize), address, size);
                }
                if virtual_address != 0 {
                    call!("cuMemAddressFree", fn(u64, usize), virtual_address, total);
                }
                Ok(())
            })();
            let left = context.leave();
            transfer?;
            cleanup?;
            left?;
        }
        Ok(())
    }

    pub fn release(self) -> Result<()> {
        let context = Context::enter(self.context, self.device)?;
        let unregistered = (|| -> Result<()> {
            call!(
                "cuMemHostUnregister",
                fn(*mut c_void),
                self.base as *mut c_void
            );
            Ok(())
        })();
        let left = context.leave();
        unregistered?;
        left?;
        if unsafe { libc::munmap(self.base as *mut c_void, self.size) } != 0 {
            return Err(UNKNOWN);
        }
        Ok(())
    }
}
