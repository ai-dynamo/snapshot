// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! In-process CUDA sharing state, lifecycle operations, and the private frontend ABI.
//!
//! Driver calls execute in the owning workload process. Rust-owned records,
//! locks, allocation storage, and panic state never cross the library boundary.

#![allow(non_snake_case, reason = "CUDA dispatch mirrors the NVIDIA ABI names")]
mod driver;
mod error;
mod handlers;
mod memory;
mod runtime;

use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_INITIALIZED, CUDA_ERROR_NOT_READY,
    CUDA_ERROR_OPERATING_SYSTEM, CUDA_ERROR_UNKNOWN, CUDA_SUCCESS,
};
use cudarc::driver::sys::{
    CUcontext, CUdevice, CUdeviceptr, CUipcMemHandle, CUmemAccessDesc, CUmemAllocationHandleType,
    CUmemAllocationProp, CUmemGenericAllocationHandle, CUmulticastObjectProp, CUresult,
};
use cuinterpose_abi::*;
use error::Error;
use runtime::RUNTIME_FAILED;
use std::ffi::{CStr, c_void};
use std::sync::{OnceLock, atomic::Ordering};

static G_FRONTEND_ABI: OnceLock<FrontendAbi> = OnceLock::new();

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
                if let Err(error) = runtime::ready() {
                    return cuda_error(error, CUDA_ERROR_NOT_READY);
                }
                handlers::$name($($arg),*)
                    .map_or_else(|error| cuda_error(error, CUDA_ERROR_UNKNOWN), |()| CUDA_SUCCESS)
            }
        )*
        static G_BACKEND_ABI: BackendAbi = BackendAbi {
            version: ABI_VERSION, size: size_of::<BackendAbi>() as u32,
            ensure_cuinterpose_initialized,
            $($name,)*
        };
    };
}
// Initializing BackendAbi checks these adapters against the canonical signatures.
exports! {
    cuCtxDestroy(context: CUcontext);
    cuCtxDestroy_v2(context: CUcontext);
    cuDevicePrimaryCtxRelease(device: CUdevice);
    cuDevicePrimaryCtxRelease_v2(device: CUdevice);
    cuDevicePrimaryCtxReset(device: CUdevice);
    cuDevicePrimaryCtxReset_v2(device: CUdevice);
    cuMemAlloc_v2(out: *mut CUdeviceptr, size: usize);
    cuMemFree_v2(address: CUdeviceptr);
    cuMemGetAddressRange_v2(base: *mut CUdeviceptr, size: *mut usize, address: CUdeviceptr);
    cuIpcGetMemHandle(out: *mut CUipcMemHandle, address: CUdeviceptr);
    cuIpcOpenMemHandle(out: *mut CUdeviceptr, handle: CUipcMemHandle, flags: u32);
    cuIpcOpenMemHandle_v2(out: *mut CUdeviceptr, handle: CUipcMemHandle, flags: u32);
    cuIpcCloseMemHandle(address: CUdeviceptr);
    cuMemCreate(out: *mut CUmemGenericAllocationHandle, size: usize, prop: *const CUmemAllocationProp, flags: u64);
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
    cuMulticastUnbind(handle: CUmemGenericAllocationHandle, device: CUdevice, offset: usize, size: usize);
}

unsafe extern "C" fn ensure_cuinterpose_initialized() -> CUresult {
    runtime::initialize().map_or_else(
        |error| cuda_error(error, CUDA_ERROR_NOT_INITIALIZED),
        |()| CUDA_SUCCESS,
    )
}

// Internal errors are reported only after the operation's guards and private
// runtime candidates have dropped. The ABI determines the public fallback code.
fn cuda_error(error: Error, fallback: CUresult) -> CUresult {
    match error {
        Error::Cuda(code) => code,
        Error::RuntimeFailed => fallback,
        error => {
            eprintln!("cuinterpose: {error}");
            match error {
                Error::Startup(_) => CUDA_ERROR_NOT_INITIALIZED,
                Error::Io { .. } => CUDA_ERROR_OPERATING_SYSTEM,
                _ => fallback,
            }
        }
    }
}

/// Registers the frontend and returns the immutable process-lifetime table.
///
/// This idempotent handshake does not resolve CUDA symbols or start runtime
/// services. Those are initialized by the table's initialization callback.
///
/// # Safety
/// `frontend` must expose an aligned readable version/size prefix. A matching
/// prefix promises a complete `FrontendAbi` with a valid C resolver callback that
/// remains callable for the process lifetime and never unwinds into Rust.
/// `output` must be writable pointer storage. The returned table is borrowed:
/// callers must not free it or unload this library while using its callbacks.
/// Repeated registrations must use the same resolver.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuinterpose_core_init(
    frontend: *const FrontendAbi,
    output: *mut *const BackendAbi,
) -> CUresult {
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return CUDA_ERROR_UNKNOWN;
    }
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
    // Only copy the table while initializing OnceLock. Loader operations,
    // callbacks and worker startup here could deadlock a constructor caller.
    let existing = G_FRONTEND_ABI.get_or_init(|| frontend);
    if existing.resolve as usize != frontend.resolve as usize {
        return CUDA_ERROR_INVALID_VALUE;
    }
    unsafe {
        *output = &G_BACKEND_ABI;
    }
    CUDA_SUCCESS
}
