// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! In-process CUDA sharing state, lifecycle operations, and the private frontend ABI.
//!
//! Driver calls execute in the owning workload process. Rust-owned records,
//! locks, allocation storage, and panic state never cross the library boundary.

#![allow(non_snake_case, reason = "CUDA dispatch mirrors the NVIDIA ABI names")]
mod boundary;
mod control;
mod driver;
mod export_cache;
mod host_carrier;
mod legacy_ipc;
mod logical_handle;
mod multicast;
mod process;
mod state;
mod ticket;

use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_INITIALIZED, CUDA_ERROR_NOT_READY, CUDA_ERROR_UNKNOWN,
    CUDA_SUCCESS,
};
use cudarc::driver::sys::{
    CUdevice, CUdeviceptr, CUipcMemHandle, CUmemAccessDesc, CUmemAllocationGranularity_flags,
    CUmemAllocationHandleType, CUmemAllocationProp, CUmemGenericAllocationHandle,
    CUmulticastGranularity_flags, CUmulticastObjectProp, CUresult,
};
use cuinterpose_abi::*;
use std::ffi::{CStr, c_void};
use std::sync::OnceLock;
use std::sync::atomic::AtomicBool;

static G_FRONTEND_ABI: OnceLock<FrontendAbi> = OnceLock::new();
static G_FAILED: AtomicBool = AtomicBool::new(false);

fn driver(name: &CStr) -> *mut c_void {
    match G_FRONTEND_ABI.get() {
        Some(frontend) => unsafe { (frontend.resolve)(name.as_ptr()) },
        None => std::ptr::null_mut(),
    }
}

macro_rules! exports {
    ($($name:ident($($arg:ident: $ty:ty),*);)*) => {
        $(
            unsafe extern "C" fn $name($($arg: $ty),*) -> CUresult {
                if G_FAILED.load(std::sync::atomic::Ordering::Acquire) {
                    return CUDA_ERROR_NOT_READY;
                }
                boundary::call(&G_FAILED, CUDA_ERROR_UNKNOWN, || {
                    if let Err(code) = state::initialize() { return code.0; }
                    let result = state::$name($($arg),*);
                    result.map_or_else(|code| code.0, |()| CUDA_SUCCESS)
                })
            }
        )*
        static G_BACKEND_ABI: BackendAbi = BackendAbi {
            version: ABI_VERSION, size: size_of::<BackendAbi>() as u32,
            fork_prepare: process::prepare,
            fork_parent: process::parent,
            fork_child: process::child,
            ensure_cuinterpose_initialized,
            $($name,)*
        };
    };
}
// Initializing BackendAbi checks these adapters against the canonical signatures.
exports! {
    cuMemAlloc_v2(out: *mut CUdeviceptr, size: usize);
    cuMemFree_v2(address: CUdeviceptr);
    cuMemGetAddressRange_v2(base: *mut CUdeviceptr, size: *mut usize, address: CUdeviceptr);
    cuIpcGetMemHandle(out: *mut CUipcMemHandle, address: CUdeviceptr);
    cuIpcOpenMemHandle(out: *mut CUdeviceptr, handle: CUipcMemHandle, flags: u32);
    cuIpcOpenMemHandle_v2(out: *mut CUdeviceptr, handle: CUipcMemHandle, flags: u32);
    cuIpcCloseMemHandle(address: CUdeviceptr);
    cuMemCreate(out: *mut CUmemGenericAllocationHandle, size: usize, prop: *const CUmemAllocationProp, flags: u64);
    cuMemGetAllocationGranularity(out: *mut usize, prop: *const CUmemAllocationProp, flags: CUmemAllocationGranularity_flags);
    cuMemRelease(handle: CUmemGenericAllocationHandle);
    cuMemRetainAllocationHandle(out: *mut CUmemGenericAllocationHandle, address: *mut c_void);
    cuMemMap(address: CUdeviceptr, size: usize, offset: usize, handle: CUmemGenericAllocationHandle, flags: u64);
    cuMemUnmap(address: CUdeviceptr, size: usize);
    cuMemSetAccess(address: CUdeviceptr, size: usize, access: *const CUmemAccessDesc, count: usize);
    cuMemExportToShareableHandle(out: *mut c_void, handle: CUmemGenericAllocationHandle, kind: CUmemAllocationHandleType, flags: u64);
    cuMemImportFromShareableHandle(out: *mut CUmemGenericAllocationHandle, fd: *mut c_void, kind: CUmemAllocationHandleType);
    cuMemGetAllocationPropertiesFromHandle(out: *mut CUmemAllocationProp, handle: CUmemGenericAllocationHandle);
    cuMulticastCreate(out: *mut CUmemGenericAllocationHandle, prop: *const CUmulticastObjectProp);
    cuMulticastAddDevice(handle: CUmemGenericAllocationHandle, device: CUdevice);
    cuMulticastBindMem(handle: CUmemGenericAllocationHandle, offset: usize, member: CUmemGenericAllocationHandle, member_offset: usize, size: usize, flags: u64);
    cuMulticastBindMem_v2(handle: CUmemGenericAllocationHandle, device: CUdevice, offset: usize, member: CUmemGenericAllocationHandle, member_offset: usize, size: usize, flags: u64);
    cuMulticastBindAddr(handle: CUmemGenericAllocationHandle, offset: usize, address: CUdeviceptr, size: usize, flags: u64);
    cuMulticastBindAddr_v2(handle: CUmemGenericAllocationHandle, device: CUdevice, offset: usize, address: CUdeviceptr, size: usize, flags: u64);
    cuMulticastGetGranularity(out: *mut usize, prop: *const CUmulticastObjectProp, flags: CUmulticastGranularity_flags);
    cuMulticastUnbind(handle: CUmemGenericAllocationHandle, device: CUdevice, offset: usize, size: usize);
}

unsafe extern "C" fn ensure_cuinterpose_initialized() -> CUresult {
    boundary::call(&G_FAILED, CUDA_ERROR_NOT_INITIALIZED, || {
        state::initialize().map_or_else(|error| error.0, |()| CUDA_SUCCESS)
    })
}

/// Initializes this core generation and returns its process-lifetime dispatch table.
///
/// # Safety
/// `frontend` must expose an aligned readable version/size prefix. A matching
/// prefix promises a complete `FrontendAbi` with a valid C resolver callback that
/// remains callable for the process lifetime and never unwinds into Rust.
/// `output` must be writable pointer storage. The returned table is borrowed:
/// callers must not free it or unload this library while using its callbacks.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuinterpose_core_init(
    frontend: *const FrontendAbi,
    output: *mut *const BackendAbi,
) -> CUresult {
    boundary::call(&G_FAILED, CUDA_ERROR_UNKNOWN, || {
        if frontend.is_null() || output.is_null() {
            return CUDA_ERROR_INVALID_VALUE;
        }
        // A mismatched frontend may supply only the version/size prefix. Check it
        // before reading the resolver field or copying the full structure.
        let version = unsafe { std::ptr::addr_of!((*frontend).version).read() };
        let size = unsafe { std::ptr::addr_of!((*frontend).size).read() };
        if version != ABI_VERSION || size as usize != size_of::<FrontendAbi>() {
            return CUDA_ERROR_INVALID_VALUE;
        }
        let frontend = unsafe { *frontend };
        if let Some(existing) = G_FRONTEND_ABI.get() {
            if existing.resolve as usize != frontend.resolve as usize {
                return CUDA_ERROR_INVALID_VALUE;
            }
        } else if G_FRONTEND_ABI.set(frontend).is_err() {
            return CUDA_ERROR_NOT_READY;
        }
        if let Err(error) = state::initialize() {
            return error.0;
        }
        unsafe {
            *output = &G_BACKEND_ABI;
        }
        CUDA_SUCCESS
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn incompatible_frontend_prefix_is_rejected_before_reading_callbacks() {
        let page_size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
        assert!(page_size > 0);
        let page_size = page_size as usize;
        let mapping = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                page_size * 2,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        assert_ne!(mapping, libc::MAP_FAILED);
        let guard_page = unsafe { mapping.cast::<u8>().add(page_size) };
        assert_eq!(
            unsafe { libc::mprotect(guard_page.cast(), page_size, libc::PROT_NONE) },
            0
        );
        let prefix = unsafe { guard_page.sub(8).cast::<u32>() };
        for (version, size) in [(999, size_of::<FrontendAbi>() as u32), (ABI_VERSION, 8)] {
            unsafe {
                prefix.write(version);
                prefix.add(1).write(size);
            }
            let mut output = std::ptr::null();
            assert_eq!(
                unsafe { cuinterpose_core_init(prefix.cast(), &mut output) },
                CUDA_ERROR_INVALID_VALUE
            );
            assert!(output.is_null());
            assert!(
                G_FRONTEND_ABI.get().is_none(),
                "invalid prefix initialized core state"
            );
        }
        assert_eq!(unsafe { libc::munmap(mapping, page_size * 2) }, 0);
    }
}
