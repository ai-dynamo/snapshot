// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The ABI shared by the C frontend and the separately linked Rust core.
//! Rust objects remain in their library. Unwinding must not cross this ABI.

use std::ffi::{c_char, c_ulonglong, c_void};

use cudarc::driver::sys as cuda;

pub const ABI_VERSION: u32 = 1;

/// ABI table that the Rust backend provides to the C frontend. cbindgen generates the
/// matching C declaration.
///
/// The handshake returns an immutable table that remains valid until process exit. It
/// does not start runtime services or call frontend callbacks. Repeated and concurrent
/// registrations of the same frontend must return the same table.
#[repr(C)]
#[derive(Debug)]
#[allow(
    non_snake_case,
    reason = "CUDA callback fields preserve driver API names"
)]
pub struct BackendAbi {
    pub version: u32,
    pub size: u32,
    /// Starts runtime services only after the real cuInit succeeds.
    pub ensure_cuinterpose_initialized: unsafe extern "C" fn() -> cuda::CUresult,
    pub cuCtxDestroy: unsafe extern "C" fn(cuda::CUcontext) -> cuda::CUresult,
    pub cuCtxDestroy_v2: unsafe extern "C" fn(cuda::CUcontext) -> cuda::CUresult,
    pub cuDevicePrimaryCtxRelease: unsafe extern "C" fn(cuda::CUdevice) -> cuda::CUresult,
    pub cuDevicePrimaryCtxRelease_v2: unsafe extern "C" fn(cuda::CUdevice) -> cuda::CUresult,
    pub cuDevicePrimaryCtxReset: unsafe extern "C" fn(cuda::CUdevice) -> cuda::CUresult,
    pub cuDevicePrimaryCtxReset_v2: unsafe extern "C" fn(cuda::CUdevice) -> cuda::CUresult,
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
    pub cuMulticastUnbind: unsafe extern "C" fn(
        cuda::CUmemGenericAllocationHandle,
        cuda::CUdevice,
        usize,
        usize,
    ) -> cuda::CUresult,
}

pub type Resolve = unsafe extern "C" fn(*const c_char) -> *mut c_void;

/// ABI table that the C frontend provides to the Rust backend.
///
/// Both libraries must follow this ABI contract. A matching version and size require a
/// complete table with non-null callbacks of the declared types. The callbacks must
/// remain valid until process exit. A mismatched table needs only the aligned
/// eight-byte prefix. Repeated registrations must use the same `resolve` callback.
#[repr(C)]
#[derive(Clone, Copy, Debug)]
pub struct FrontendAbi {
    pub version: u32,
    pub size: u32,
    pub resolve: Resolve,
}
