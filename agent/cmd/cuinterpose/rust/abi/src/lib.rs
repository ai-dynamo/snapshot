// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The only ABI shared by the two independently linked Rust libraries.
//! No Rust-owned object or unwinding crosses this boundary.

use std::ffi::{c_char, c_void};
use std::sync::atomic::{AtomicBool, Ordering};

pub const ABI_VERSION: u32 = 3;
pub const CUDA_VERSION: u32 = 13010;
pub const SUCCESS: i32 = 0;
pub const INVALID_VALUE: i32 = 1;
pub const OUT_OF_MEMORY: i32 = 2;
pub const NOT_INITIALIZED: i32 = 3;
pub const INVALID_HANDLE: i32 = 400;
pub const NOT_READY: i32 = 600;
pub const NOT_SUPPORTED: i32 = 801;
pub const UNKNOWN: i32 = 999;
pub const HANDLE_TAG: u64 = 0xd94d_0000_0000_0000;
pub const HANDLE_MASK: u64 = 0xffff_0000_0000_0000;

#[repr(C)]
#[derive(Clone, Copy, Default, Debug)]
pub struct Location {
    pub kind: i32,
    pub id: i32,
}

macro_rules! core_api {
    ($($name:ident($($arg:ident: $ty:ty),*);)*) => {
        /// Versioned, typed dispatch table. Its field order is generated from
        /// the same signature inventory used to declare both sides of the ABI.
        #[repr(C)]
        #[allow(non_snake_case)]
        pub struct Core {
            pub version: u32,
            pub size: u32,
            pub debug_stats: unsafe extern "C" fn(*mut DebugStats),
            pub fork_prepare: unsafe extern "C" fn(),
            pub fork_parent: unsafe extern "C" fn(),
            pub fork_child: unsafe extern "C" fn(),
            $(pub $name: unsafe extern "C" fn($($ty),*) -> i32,)*
        }
    };
}
memory_api!(core_api);

#[repr(C)]
#[derive(Clone, Copy, Default, Debug)]
pub struct AllocationFlags {
    pub compression: u8,
    pub rdma: u8,
    pub usage: u16,
    pub reserved: [u8; 4],
}

#[repr(C)]
#[derive(Clone, Copy, Debug)]
pub struct AllocationProp {
    pub kind: i32,
    pub handle_types: u32,
    pub location: Location,
    pub win32_metadata: *mut c_void,
    pub flags: AllocationFlags,
}

#[repr(C)]
#[derive(Clone, Copy, Default, Debug)]
pub struct Access {
    pub location: Location,
    pub flags: u32,
}

#[repr(C)]
#[derive(Clone, Copy, Default, Debug)]
pub struct MulticastProp {
    pub devices: u32,
    pub size: usize,
    pub handle_types: u64,
    pub flags: u64,
}

#[repr(C)]
#[derive(Default)]
pub struct DebugStats {
    pub allocations: u64,
    pub handles: u64,
    pub mappings: u64,
    pub multicasts: u64,
    pub cached_exports: u64,
    pub live_raw_imports: u64,
    pub unsupported_exportable_creations: u64,
    pub phase: u32,
}

#[repr(C)]
pub struct BuildInfo {
    pub cuda_version: u32,
    pub protocol_version: u32,
}

pub type Resolve = unsafe extern "C" fn(*const c_char) -> *mut c_void;

/// This is a trusted sibling-library ABI, not an untrusted byte decoder.
/// Matching version/size promises a fully initialized table of non-null
/// callbacks with the declared signatures and process-lifetime validity.
/// A mismatched table need only provide the aligned eight-byte prefix.
#[repr(C)]
#[derive(Clone, Copy)]
pub struct Host {
    pub version: u32,
    pub size: u32,
    pub resolve: Resolve,
    pub enter: unsafe extern "C" fn() -> i32,
    pub leave: unsafe extern "C" fn(),
    pub origin_pid: i32,
}

pub type Initialize = unsafe extern "C" fn(*const Host, *mut *const Core) -> i32;

/// Panics are bugs, not recoverable CUDA failures. Do not continue to mutate
/// state after one. Forget the payload because its destructor may itself panic.
/// This does not intercept the process panic hook or catch aborts/foreign throws.
pub fn boundary<T: Copy>(failed: &AtomicBool, error: T, operation: impl FnOnce() -> T) -> T {
    if failed.load(Ordering::Acquire) {
        return error;
    }
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(operation)) {
        Ok(value) => value,
        Err(payload) => {
            failed.store(true, Ordering::Release);
            std::mem::forget(payload);
            error
        }
    }
}

/// One signature inventory generates both front-end exports and core dispatch.
/// The arguments are C ABI types, never Rust enums or references.
#[macro_export]
macro_rules! memory_api {
    ($emit:ident) => {
        $emit! {
            cuMemCreate(out: *mut u64, size: usize, prop: *const $crate::AllocationProp, flags: u64);
            cuMemRelease(handle: u64);
            cuMemRetainAllocationHandle(out: *mut u64, address: *mut std::ffi::c_void);
            cuMemMap(address: u64, size: usize, offset: usize, handle: u64, flags: u64);
            cuMemUnmap(address: u64, size: usize);
            cuMemSetAccess(address: u64, size: usize, access: *const $crate::Access, count: usize);
            cuMemExportToShareableHandle(out: *mut std::ffi::c_void, handle: u64, kind: u32, flags: u64);
            cuMemImportFromShareableHandle(out: *mut u64, fd: *mut std::ffi::c_void, kind: u32);
            cuMemGetAllocationPropertiesFromHandle(out: *mut $crate::AllocationProp, handle: u64);
            cuMulticastCreate(out: *mut u64, prop: *const $crate::MulticastProp);
            cuMulticastAddDevice(handle: u64, device: i32);
            cuMulticastBindMem(handle: u64, offset: usize, member: u64, member_offset: usize, size: usize, flags: u64);
            cuMulticastBindMem_v2(handle: u64, device: i32, offset: usize, member: u64, member_offset: usize, size: usize, flags: u64);
            cuMulticastBindAddr(handle: u64, offset: usize, address: u64, size: usize, flags: u64);
            cuMulticastBindAddr_v2(handle: u64, device: i32, offset: usize, address: u64, size: usize, flags: u64);
            cuMulticastGetGranularity(out: *mut usize, prop: *const $crate::MulticastProp, flags: u32);
            cuMulticastUnbind(handle: u64, device: i32, offset: usize, size: usize);
        }
    };
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn panic_poisoning_is_sticky() {
        let failed = AtomicBool::new(false);
        assert_eq!(boundary(&failed, UNKNOWN, || panic!("injected")), UNKNOWN);
        assert_eq!(boundary(&failed, UNKNOWN, || SUCCESS), UNKNOWN);
    }

    #[test]
    fn cuda_abi_layouts() {
        assert_eq!(size_of::<AllocationProp>(), 32);
        assert_eq!(std::mem::offset_of!(AllocationProp, flags), 24);
        assert_eq!(size_of::<MulticastProp>(), 32);
        assert_eq!(std::mem::offset_of!(MulticastProp, size), 8);
        assert_eq!(size_of::<Access>(), 12);
    }
}
