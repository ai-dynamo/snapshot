// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! In-process CUDA sharing state, lifecycle operations, and the private frontend ABI.
//!
//! Driver calls execute in the owning workload process. Rust-owned records,
//! locks, allocation storage, and panic state never cross the library boundary.

#![allow(non_snake_case, reason = "CUDA dispatch mirrors the NVIDIA ABI names")]
mod control;
mod driver;
mod export_cache;
mod host_carrier;
mod multicast;
mod process;
mod state;
mod ticket;

use cuinterpose_abi::*;
use std::ffi::{CStr, c_void};
use std::sync::OnceLock;
use std::sync::atomic::AtomicBool;

static G_HOST: OnceLock<Host> = OnceLock::new();
static G_FAILED: AtomicBool = AtomicBool::new(false);
static G_ABI_FAILED: AtomicBool = AtomicBool::new(false);

fn driver(name: &CStr) -> *mut c_void {
    match G_HOST.get() {
        Some(host) => unsafe { (host.resolve)(name.as_ptr()) },
        None => std::ptr::null_mut(),
    }
}

macro_rules! exports {
    ($($name:ident($($arg:ident: $ty:ty),*);)*) => {
        $(
            unsafe extern "C" fn $name($($arg: $ty),*) -> i32 {
                if G_FAILED.load(std::sync::atomic::Ordering::Acquire) { return NOT_READY; }
                boundary(&G_FAILED, UNKNOWN, || {
                    if let Err(code) = state::initialize() { return code.0; }
                    let result = state::$name($($arg),*);
                    result.map_or_else(|code| code.0, |()| SUCCESS)
                })
            }
        )*
        static G_API: Core = Core {
            version: ABI_VERSION, size: size_of::<Core>() as u32, debug_stats,
            fork_prepare: process::prepare,
            fork_parent: process::parent,
            fork_child: process::child,
            ensure_ready,
            $($name,)*
        };
    };
}
// Initializing Core checks these adapters against the canonical ABI signatures.
exports! {
    cuMemCreate(out: *mut u64, size: usize, prop: *const AllocationProp, flags: u64);
    cuMemRelease(handle: u64);
    cuMemRetainAllocationHandle(out: *mut u64, address: *mut c_void);
    cuMemMap(address: u64, size: usize, offset: usize, handle: u64, flags: u64);
    cuMemUnmap(address: u64, size: usize);
    cuMemSetAccess(address: u64, size: usize, access: *const Access, count: usize);
    cuMemExportToShareableHandle(out: *mut c_void, handle: u64, kind: u32, flags: u64);
    cuMemImportFromShareableHandle(out: *mut u64, fd: *mut c_void, kind: u32);
    cuMemGetAllocationPropertiesFromHandle(out: *mut AllocationProp, handle: u64);
    cuMulticastCreate(out: *mut u64, prop: *const MulticastProp);
    cuMulticastAddDevice(handle: u64, device: i32);
    cuMulticastBindMem(handle: u64, offset: usize, member: u64, member_offset: usize, size: usize, flags: u64);
    cuMulticastBindMem_v2(handle: u64, device: i32, offset: usize, member: u64, member_offset: usize, size: usize, flags: u64);
    cuMulticastBindAddr(handle: u64, offset: usize, address: u64, size: usize, flags: u64);
    cuMulticastBindAddr_v2(handle: u64, device: i32, offset: usize, address: u64, size: usize, flags: u64);
    cuMulticastGetGranularity(out: *mut usize, prop: *const MulticastProp, flags: u32);
    cuMulticastUnbind(handle: u64, device: i32, offset: usize, size: usize);
}

unsafe extern "C" fn ensure_ready() -> i32 {
    boundary(&G_FAILED, NOT_INITIALIZED, || {
        state::initialize().map_or_else(|error| error.0, |()| SUCCESS)
    })
}

unsafe extern "C" fn debug_stats(output: *mut DebugStats) {
    boundary(&G_ABI_FAILED, (), || {
        if state::initialize().is_err() {
            return;
        }
        if !output.is_null() {
            if G_FAILED.load(std::sync::atomic::Ordering::Acquire) {
                unsafe {
                    output.write(DebugStats {
                        phase: DebugPhase::Failed as u32,
                        ..DebugStats::default()
                    });
                }
                return;
            }
            if let Ok(state) = state::get() {
                unsafe {
                    output.write(state.stats());
                }
            }
        }
    });
}

/// Initializes this core generation and returns its process-lifetime dispatch table.
///
/// # Safety
/// `host` must expose an aligned readable version/size prefix. A matching
/// prefix promises a complete `Host` with a valid C resolver callback that
/// remains callable for the process lifetime and never unwinds into Rust.
/// `output` must be writable pointer storage. The returned table is borrowed:
/// callers must not free it or unload this library while using its callbacks.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuinterpose_core_init(host: *const Host, output: *mut *const Core) -> i32 {
    boundary(&G_FAILED, UNKNOWN, || {
        if host.is_null() || output.is_null() {
            return INVALID_VALUE;
        }
        // A mismatched host may supply only the version/size prefix. Check it
        // before reading the resolver field or copying the full structure.
        let version = unsafe { std::ptr::addr_of!((*host).version).read() };
        let size = unsafe { std::ptr::addr_of!((*host).size).read() };
        if version != ABI_VERSION || size as usize != size_of::<Host>() {
            return INVALID_VALUE;
        }
        let host = unsafe { *host };
        if let Some(existing) = G_HOST.get() {
            if existing.resolve as usize != host.resolve as usize {
                return INVALID_VALUE;
            }
        } else if G_HOST.set(host).is_err() {
            return NOT_READY;
        }
        if let Err(error) = state::initialize() {
            return error.0;
        }
        unsafe {
            *output = &G_API;
        }
        SUCCESS
    })
}
