// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The ABI shared by the C frontend and independently linked Rust core.
//! No Rust-owned object or unwinding crosses this boundary.

use std::ffi::{c_char, c_void};
use std::sync::atomic::{AtomicBool, Ordering};

use cudarc::driver::sys::{self as cuda, CUresult};
pub use cudarc::driver::sys::{
    CU_MEMHOSTREGISTER_PORTABLE, CUdevice as Device, CUdeviceptr as DevicePtr,
    CUmemAllocationProp_st__bindgen_ty_1 as AllocationFlags,
    CUmemGenericAllocationHandle as AllocationHandle, CUmulticastObjectProp as MulticastProp,
};

pub const ABI_VERSION: u32 = 6;
// Keep the C header independent of cudarc's generated implementation details.
pub const CUDA_VERSION: u32 = 13010;
const _: () = assert!(CUDA_VERSION == cuda::CUDA_VERSION);
pub const SUCCESS: i32 = CUresult::CUDA_SUCCESS as i32;
pub const INVALID_VALUE: i32 = CUresult::CUDA_ERROR_INVALID_VALUE as i32;
pub const OUT_OF_MEMORY: i32 = CUresult::CUDA_ERROR_OUT_OF_MEMORY as i32;
pub const NOT_INITIALIZED: i32 = CUresult::CUDA_ERROR_NOT_INITIALIZED as i32;
pub const INVALID_HANDLE: i32 = CUresult::CUDA_ERROR_INVALID_HANDLE as i32;
pub const NOT_READY: i32 = CUresult::CUDA_ERROR_NOT_READY as i32;
pub const NOT_SUPPORTED: i32 = CUresult::CUDA_ERROR_NOT_SUPPORTED as i32;
pub const UNKNOWN: i32 = CUresult::CUDA_ERROR_UNKNOWN as i32;
pub const POSIX_FD: u32 =
    cuda::CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR.0;
pub const ALLOCATION_PINNED: i32 = cuda::CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED as i32;
pub const LOCATION_DEVICE: i32 = cuda::CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE as i32;
pub const ACCESS_READWRITE: u32 =
    cuda::CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE as u32;
pub const STREAM_NON_BLOCKING: u32 = cuda::CUstream_flags::CU_STREAM_NON_BLOCKING as u32;
pub const HANDLE_TAG: u64 = 0xd94d_0000_0000_0000;
pub const HANDLE_MASK: u64 = 0xffff_0000_0000_0000;

// C callers can supply unknown enum values, and drivers can return unknown
// error codes. Keep those fields/results as integers, not cudarc Rust enums.
// Structures without enum fields are reused directly above.
#[repr(C)]
#[derive(Clone, Copy, Default, Debug)]
pub struct Location {
    pub kind: i32,
    pub id: i32,
}

/// Canonical private ABI. cbindgen emits the C declaration from this table.
#[repr(C)]
#[derive(Debug)]
#[allow(
    non_snake_case,
    reason = "CUDA callback fields preserve driver API names"
)]
pub struct Core {
    pub version: u32,
    pub size: u32,
    pub debug_stats: unsafe extern "C" fn(*mut DebugStats),
    pub fork_prepare: unsafe extern "C" fn(),
    pub fork_parent: unsafe extern "C" fn(),
    pub fork_child: unsafe extern "C" fn(),
    pub ensure_ready: unsafe extern "C" fn() -> i32,
    pub cuMemCreate: unsafe extern "C" fn(*mut u64, usize, *const AllocationProp, u64) -> i32,
    pub cuMemGetAllocationGranularity:
        unsafe extern "C" fn(*mut usize, *const AllocationProp, u32) -> i32,
    pub cuMemRelease: unsafe extern "C" fn(u64) -> i32,
    pub cuMemRetainAllocationHandle: unsafe extern "C" fn(*mut u64, *mut c_void) -> i32,
    pub cuMemMap: unsafe extern "C" fn(u64, usize, usize, u64, u64) -> i32,
    pub cuMemUnmap: unsafe extern "C" fn(u64, usize) -> i32,
    pub cuMemSetAccess: unsafe extern "C" fn(u64, usize, *const Access, usize) -> i32,
    pub cuMemExportToShareableHandle: unsafe extern "C" fn(*mut c_void, u64, u32, u64) -> i32,
    pub cuMemImportFromShareableHandle: unsafe extern "C" fn(*mut u64, *mut c_void, u32) -> i32,
    pub cuMemGetAllocationPropertiesFromHandle:
        unsafe extern "C" fn(*mut AllocationProp, u64) -> i32,
    pub cuMulticastCreate: unsafe extern "C" fn(*mut u64, *const MulticastProp) -> i32,
    pub cuMulticastAddDevice: unsafe extern "C" fn(u64, i32) -> i32,
    pub cuMulticastBindMem: unsafe extern "C" fn(u64, usize, u64, usize, usize, u64) -> i32,
    pub cuMulticastBindMem_v2: unsafe extern "C" fn(u64, i32, usize, u64, usize, usize, u64) -> i32,
    pub cuMulticastBindAddr: unsafe extern "C" fn(u64, usize, u64, usize, u64) -> i32,
    pub cuMulticastBindAddr_v2: unsafe extern "C" fn(u64, i32, usize, u64, usize, u64) -> i32,
    pub cuMulticastGetGranularity:
        unsafe extern "C" fn(*mut usize, *const MulticastProp, u32) -> i32,
    pub cuMulticastUnbind: unsafe extern "C" fn(u64, i32, usize, usize) -> i32,
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

// Check every field of our raw-enum mirrors against the CUDA 13.1 bindings.
// No reference to a cudarc enum-bearing struct is ever made from caller bytes.
const _: () = {
    macro_rules! layout {
        ($raw:ty, $cuda:ty, $($field:ident => $cuda_field:ident),+ $(,)?) => {
            assert!(size_of::<$raw>() == size_of::<$cuda>());
            assert!(align_of::<$raw>() == align_of::<$cuda>());
            $(assert!(std::mem::offset_of!($raw, $field) == std::mem::offset_of!($cuda, $cuda_field));)+
        };
    }
    layout!(Location, cuda::CUmemLocation, kind => type_, id => id);
    layout!(Access, cuda::CUmemAccessDesc, location => location, flags => flags);
    layout!(AllocationProp, cuda::CUmemAllocationProp,
        kind => type_, handle_types => requestedHandleTypes, location => location,
        win32_metadata => win32HandleMetaData, flags => allocFlags);
};

#[repr(C)]
#[derive(Default, Debug)]
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

/// Stable values exposed through DebugStats, not the core's lifecycle state.
#[repr(u32)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DebugPhase {
    Active = 1,
    Preparing = 2,
    Prepared = 3,
    Restoring = 4,
    Failed = 5,
}

#[repr(C)]
#[derive(Debug)]
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
#[derive(Clone, Copy, Debug)]
pub struct Host {
    pub version: u32,
    pub size: u32,
    pub resolve: Resolve,
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
