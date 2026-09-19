// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! One typed boundary for CUDA entry points, resolved through the frontend.
//! Signatures follow NVIDIA cuda.h/cudaTypedefs.h; optional symbols stay lazy.
//! Raw calls retain driver-written outputs even on failure.
//! cudarc supplies CUDA types/constants, not its loader or safe resource owners.

use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_NOT_INITIALIZED, CUDA_SUCCESS,
};
use cudarc::driver::sys::{
    CUdevice, CUdeviceptr, CUmemAccessDesc, CUmemAllocationGranularity_flags,
    CUmemAllocationHandleType, CUmemAllocationProp, CUmemGenericAllocationHandle,
    CUmulticastGranularity_flags, CUmulticastObjectProp, CUresult, CUstream_flags,
};
use std::ffi::c_void;
use std::os::fd::{AsRawFd, BorrowedFd, FromRawFd, OwnedFd};

#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
#[error("CUDA error {0:?}")]
pub struct CudaError(pub CUresult);

impl From<CUresult> for CudaError {
    fn from(code: CUresult) -> Self {
        Self(code)
    }
}

pub fn import_posix(fd: BorrowedFd<'_>) -> Result<CUmemGenericAllocationHandle> {
    let mut handle = 0;
    unsafe {
        cuMemImportFromShareableHandle(
            &mut handle,
            fd.as_raw_fd() as usize as *mut c_void,
            CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
        )
    }?;
    Ok(handle)
}

pub fn export_posix(handle: CUmemGenericAllocationHandle) -> Result<OwnedFd> {
    let mut fd = -1;
    unsafe {
        cuMemExportToShareableHandle(
            (&mut fd as *mut i32).cast(),
            handle,
            CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
            0,
        )
    }?;
    if fd < 0 {
        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
    }
    // CUDA transfers ownership of a fresh descriptor on successful POSIX export.
    Ok(unsafe { OwnedFd::from_raw_fd(fd) })
}
pub type Result<T> = std::result::Result<T, CudaError>;

impl CudaError {
    pub fn result(code: CUresult) -> Result<()> {
        if code == CUDA_SUCCESS {
            Ok(())
        } else {
            Err(Self(code))
        }
    }
}

macro_rules! functions {
    ($($name:ident($($arg:ident: $ty:ty),*);)*) => {
        pub mod symbols {
            use super::*;
            $(
                pub fn $name() -> Result<unsafe extern "C" fn($($ty),*) -> CUresult> {
                    let address = crate::driver(
                        std::ffi::CStr::from_bytes_with_nul(
                            concat!(stringify!($name), "\0").as_bytes()).expect("static CUDA symbol"));
                    if address.is_null() {
                        return Err(CudaError::from(CUDA_ERROR_NOT_INITIALIZED));
                    }
                    // The frontend resolves this exact NVIDIA driver signature.
                    Ok(unsafe { std::mem::transmute::<*mut c_void, unsafe extern "C" fn($($ty),*) -> CUresult>(address) })
                }
            )*
        }
        $(
        pub unsafe fn $name($($arg: $ty),*) -> Result<()> {
            let function = symbols::$name()?;
            CudaError::result(unsafe { function($($arg),*) })
        }
    )*};
}

functions! {
    cuCtxSynchronize();
    cuMemFree_v2(address: CUdeviceptr);
    cuMemGetAddressRange_v2(base: *mut CUdeviceptr, size: *mut usize, address: CUdeviceptr);
    cuCtxGetCurrent(context: *mut *mut c_void);
    cuCtxGetDevice(device: *mut CUdevice);
    cuCtxSetCurrent(context: *mut c_void);
    cuDevicePrimaryCtxRelease_v2(device: CUdevice);
    cuDevicePrimaryCtxRetain(context: *mut *mut c_void, device: CUdevice);
    cuMemAddressFree(address: CUdeviceptr, size: usize);
    cuMemAddressReserve(address: *mut CUdeviceptr, size: usize, alignment: usize, requested: CUdeviceptr, flags: u64);
    cuMemCreate(handle: *mut CUmemGenericAllocationHandle, size: usize, properties: *const CUmemAllocationProp, flags: u64);
    cuMemGetAllocationGranularity(size: *mut usize, properties: *const CUmemAllocationProp, flags: CUmemAllocationGranularity_flags);
    cuMemExportToShareableHandle(output: *mut c_void, handle: CUmemGenericAllocationHandle, handle_type: CUmemAllocationHandleType, flags: u64);
    cuMemGetAllocationPropertiesFromHandle(properties: *mut CUmemAllocationProp, handle: CUmemGenericAllocationHandle);
    cuMemHostRegister_v2(address: *mut c_void, size: usize, flags: u32);
    cuMemHostGetFlags(flags: *mut u32, address: *mut c_void);
    cuMemHostUnregister(address: *mut c_void);
    cuMemImportFromShareableHandle(handle: *mut CUmemGenericAllocationHandle, shareable: *mut c_void, handle_type: CUmemAllocationHandleType);
    cuMemMap(address: CUdeviceptr, size: usize, offset: usize, handle: CUmemGenericAllocationHandle, flags: u64);
    cuMemRelease(handle: CUmemGenericAllocationHandle);
    cuMemRetainAllocationHandle(handle: *mut CUmemGenericAllocationHandle, address: *mut c_void);
    cuMemSetAccess(address: CUdeviceptr, size: usize, access: *const CUmemAccessDesc, count: usize);
    cuMemUnmap(address: CUdeviceptr, size: usize);
    cuMemcpyDtoHAsync_v2(host: *mut c_void, device: CUdeviceptr, size: usize, stream: *mut c_void);
    cuMemcpyHtoDAsync_v2(device: CUdeviceptr, host: *const c_void, size: usize, stream: *mut c_void);
    cuMulticastAddDevice(group: CUmemGenericAllocationHandle, device: CUdevice);
    cuMulticastBindAddr(group: CUmemGenericAllocationHandle, offset: usize, address: CUdeviceptr, size: usize, flags: u64);
    cuMulticastBindAddr_v2(group: CUmemGenericAllocationHandle, device: CUdevice, offset: usize, address: CUdeviceptr, size: usize, flags: u64);
    cuMulticastBindMem(group: CUmemGenericAllocationHandle, offset: usize, member: CUmemGenericAllocationHandle, member_offset: usize, size: usize, flags: u64);
    cuMulticastBindMem_v2(group: CUmemGenericAllocationHandle, device: CUdevice, offset: usize, member: CUmemGenericAllocationHandle, member_offset: usize, size: usize, flags: u64);
    cuMulticastCreate(group: *mut CUmemGenericAllocationHandle, properties: *const CUmulticastObjectProp);
    cuMulticastGetGranularity(granularity: *mut usize, properties: *const CUmulticastObjectProp, flags: CUmulticastGranularity_flags);
    cuMulticastUnbind(group: CUmemGenericAllocationHandle, device: CUdevice, offset: usize, size: usize);
    cuStreamCreate(stream: *mut *mut c_void, flags: CUstream_flags);
    cuStreamDestroy_v2(stream: *mut c_void);
    cuStreamSynchronize(stream: *mut c_void);
}

pub(super) fn context() -> usize {
    let mut context = std::ptr::null_mut::<c_void>();
    if unsafe { crate::driver::cuCtxGetCurrent(&mut context) }.is_err() {
        return 0;
    }
    context as usize
}

pub struct Context {
    previous: *mut c_void,
    primary: Option<i32>,
    changed: bool,
}

impl Context {
    pub fn run<T>(context: usize, device: i32, body: impl FnOnce() -> Result<T>) -> Result<T> {
        let context = Self::enter(context, device)?;
        let result = body();
        let left = context.leave();
        // Evaluate cleanup even when the body failed, preserving its first error.
        let value = result?;
        left?;
        Ok(value)
    }
    pub fn enter(context: usize, device: i32) -> Result<Self> {
        let mut previous = std::ptr::null_mut();
        unsafe { crate::driver::cuCtxGetCurrent(&mut previous) }?;
        let mut target = context as *mut c_void;
        let mut primary = None;
        if target.is_null() {
            unsafe { crate::driver::cuDevicePrimaryCtxRetain(&mut target, device) }?;
            primary = Some(device);
        }
        let changed = target != previous;
        if changed && let Err(error) = unsafe { crate::driver::cuCtxSetCurrent(target) } {
            if let Some(device) = primary {
                let _ = unsafe { crate::driver::cuDevicePrimaryCtxRelease_v2(device) };
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
            result = unsafe { crate::driver::cuCtxSetCurrent(self.previous) };
        }
        if let Some(device) = self.primary {
            result = result.and(unsafe { crate::driver::cuDevicePrimaryCtxRelease_v2(device) });
        }
        result
    }
}
