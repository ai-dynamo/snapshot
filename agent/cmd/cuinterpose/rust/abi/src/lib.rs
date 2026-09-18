// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The ABI shared by the C frontend and independently linked Rust core.
//! No Rust-owned object or unwinding crosses this boundary.

use std::ffi::{c_char, c_ulonglong, c_void};

use cudarc::driver::sys as cuda;

pub const ABI_VERSION: u32 = 1;

/// ABI table provided by the Rust backend and consumed by the C frontend.
/// cbindgen emits the corresponding C declaration.
///
/// The handshake returns this immutable process-lifetime table without starting
/// runtime services or calling frontend callbacks. Repeated and concurrent
/// registrations of the same frontend must succeed with the same table.
#[repr(C)]
#[derive(Debug)]
#[allow(
    non_snake_case,
    reason = "CUDA callback fields preserve driver API names"
)]
pub struct BackendAbi {
    pub version: u32,
    pub size: u32,
    pub fork_prepare: unsafe extern "C" fn(),
    pub fork_parent: unsafe extern "C" fn(),
    pub fork_child: unsafe extern "C" fn(),
    /// Starts this process generation's runtime services, as do CUDA callbacks.
    /// Unlike the ABI handshake, runtime startup can return NOT_INITIALIZED
    /// during contention rather than wait while a caller holds the loader lock.
    pub ensure_cuinterpose_initialized: unsafe extern "C" fn() -> cuda::CUresult,
    pub cuMemAlloc_v2: unsafe extern "C" fn(*mut cuda::CUdeviceptr, usize) -> cuda::CUresult,
    pub cuMemFree_v2: unsafe extern "C" fn(cuda::CUdeviceptr) -> cuda::CUresult,
    pub cuMemGetAddressRange_v2: unsafe extern "C" fn(
        *mut cuda::CUdeviceptr,
        *mut usize,
        cuda::CUdeviceptr,
    ) -> cuda::CUresult,
    pub cuIpcGetMemHandle:
        unsafe extern "C" fn(*mut cuda::CUipcMemHandle, cuda::CUdeviceptr) -> cuda::CUresult,
    pub cuIpcOpenMemHandle:
        unsafe extern "C" fn(*mut cuda::CUdeviceptr, cuda::CUipcMemHandle, u32) -> cuda::CUresult,
    pub cuIpcOpenMemHandle_v2:
        unsafe extern "C" fn(*mut cuda::CUdeviceptr, cuda::CUipcMemHandle, u32) -> cuda::CUresult,
    pub cuIpcCloseMemHandle: unsafe extern "C" fn(cuda::CUdeviceptr) -> cuda::CUresult,
    pub cuMemCreate: unsafe extern "C" fn(
        *mut cuda::CUmemGenericAllocationHandle,
        usize,
        *const cuda::CUmemAllocationProp,
        c_ulonglong,
    ) -> cuda::CUresult,
    pub cuMemGetAllocationGranularity: unsafe extern "C" fn(
        *mut usize,
        *const cuda::CUmemAllocationProp,
        cuda::CUmemAllocationGranularity_flags,
    ) -> cuda::CUresult,
    pub cuMemRelease: unsafe extern "C" fn(cuda::CUmemGenericAllocationHandle) -> cuda::CUresult,
    pub cuMemRetainAllocationHandle: unsafe extern "C" fn(
        *mut cuda::CUmemGenericAllocationHandle,
        *mut c_void,
    ) -> cuda::CUresult,
    pub cuMemMap: unsafe extern "C" fn(
        cuda::CUdeviceptr,
        usize,
        usize,
        cuda::CUmemGenericAllocationHandle,
        c_ulonglong,
    ) -> cuda::CUresult,
    pub cuMemUnmap: unsafe extern "C" fn(cuda::CUdeviceptr, usize) -> cuda::CUresult,
    pub cuMemSetAccess: unsafe extern "C" fn(
        cuda::CUdeviceptr,
        usize,
        *const cuda::CUmemAccessDesc,
        usize,
    ) -> cuda::CUresult,
    pub cuMemExportToShareableHandle: unsafe extern "C" fn(
        *mut c_void,
        cuda::CUmemGenericAllocationHandle,
        cuda::CUmemAllocationHandleType,
        c_ulonglong,
    ) -> cuda::CUresult,
    pub cuMemImportFromShareableHandle: unsafe extern "C" fn(
        *mut cuda::CUmemGenericAllocationHandle,
        *mut c_void,
        cuda::CUmemAllocationHandleType,
    ) -> cuda::CUresult,
    pub cuMemGetAllocationPropertiesFromHandle: unsafe extern "C" fn(
        *mut cuda::CUmemAllocationProp,
        cuda::CUmemGenericAllocationHandle,
    ) -> cuda::CUresult,
    pub cuMulticastCreate: unsafe extern "C" fn(
        *mut cuda::CUmemGenericAllocationHandle,
        *const cuda::CUmulticastObjectProp,
    ) -> cuda::CUresult,
    pub cuMulticastAddDevice:
        unsafe extern "C" fn(cuda::CUmemGenericAllocationHandle, cuda::CUdevice) -> cuda::CUresult,
    pub cuMulticastBindMem: unsafe extern "C" fn(
        cuda::CUmemGenericAllocationHandle,
        usize,
        cuda::CUmemGenericAllocationHandle,
        usize,
        usize,
        c_ulonglong,
    ) -> cuda::CUresult,
    pub cuMulticastBindMem_v2: unsafe extern "C" fn(
        cuda::CUmemGenericAllocationHandle,
        cuda::CUdevice,
        usize,
        cuda::CUmemGenericAllocationHandle,
        usize,
        usize,
        c_ulonglong,
    ) -> cuda::CUresult,
    pub cuMulticastBindAddr: unsafe extern "C" fn(
        cuda::CUmemGenericAllocationHandle,
        usize,
        cuda::CUdeviceptr,
        usize,
        c_ulonglong,
    ) -> cuda::CUresult,
    pub cuMulticastBindAddr_v2: unsafe extern "C" fn(
        cuda::CUmemGenericAllocationHandle,
        cuda::CUdevice,
        usize,
        cuda::CUdeviceptr,
        usize,
        c_ulonglong,
    ) -> cuda::CUresult,
    pub cuMulticastGetGranularity: unsafe extern "C" fn(
        *mut usize,
        *const cuda::CUmulticastObjectProp,
        cuda::CUmulticastGranularity_flags,
    ) -> cuda::CUresult,
    pub cuMulticastUnbind: unsafe extern "C" fn(
        cuda::CUmemGenericAllocationHandle,
        cuda::CUdevice,
        usize,
        usize,
    ) -> cuda::CUresult,
}

pub type Resolve = unsafe extern "C" fn(*const c_char) -> *mut c_void;

/// ABI table provided by the C frontend and consumed by the Rust backend.
///
/// This is a trusted sibling-library ABI, not an untrusted byte decoder.
/// Matching version/size promises a fully initialized table of non-null
/// callbacks with the declared signatures and process-lifetime validity.
/// A mismatched table need only provide the aligned eight-byte prefix.
/// Repeated registrations must agree on both `resolve` and `origin_pid`.
#[repr(C)]
#[derive(Clone, Copy, Debug)]
pub struct FrontendAbi {
    pub version: u32,
    pub size: u32,
    pub resolve: Resolve,
    pub origin_pid: i32,
}
