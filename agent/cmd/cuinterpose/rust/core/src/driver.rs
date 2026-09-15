// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! One typed boundary for CUDA entry points, resolved through the frontend.
//! Signatures follow NVIDIA cuda.h/cudaTypedefs.h; optional symbols stay lazy.
//! Raw calls retain driver-written outputs even on failure.

use cuinterpose_abi::{Access, AllocationProp, MulticastProp};
use std::ffi::c_void;
use std::os::fd::{AsRawFd, BorrowedFd, FromRawFd, OwnedFd};

#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
#[error("CUDA error {0}")]
pub struct CudaError(pub i32);

impl From<i32> for CudaError {
    fn from(code: i32) -> Self {
        Self(code)
    }
}

pub fn import_posix(fd: BorrowedFd<'_>) -> Result<u64> {
    let mut handle = 0;
    unsafe {
        cuMemImportFromShareableHandle(&mut handle, fd.as_raw_fd() as usize as *mut c_void, 1)
    }?;
    Ok(handle)
}

pub fn export_posix(handle: u64) -> Result<OwnedFd> {
    let mut fd = -1;
    unsafe { cuMemExportToShareableHandle((&mut fd as *mut i32).cast(), handle, 1, 0) }?;
    if fd < 0 {
        return Err(CudaError(cuinterpose_abi::INVALID_HANDLE));
    }
    // CUDA transfers ownership of a fresh descriptor on successful POSIX export.
    Ok(unsafe { OwnedFd::from_raw_fd(fd) })
}
pub type Result<T> = std::result::Result<T, CudaError>;

impl CudaError {
    pub fn result(code: i32) -> Result<()> {
        if code == cuinterpose_abi::SUCCESS {
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
                pub fn $name() -> Result<unsafe extern "C" fn($($ty),*) -> i32> {
                    let address = crate::driver(
                        std::ffi::CStr::from_bytes_with_nul(
                            concat!(stringify!($name), "\0").as_bytes()).expect("static CUDA symbol"));
                    if address.is_null() {
                        return Err(CudaError(cuinterpose_abi::NOT_INITIALIZED));
                    }
                    // Host resolves this exact NVIDIA driver signature.
                    Ok(unsafe { std::mem::transmute::<*mut c_void, unsafe extern "C" fn($($ty),*) -> i32>(address) })
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
    cuCtxGetCurrent(context: *mut *mut c_void);
    cuCtxGetDevice(device: *mut i32);
    cuCtxSetCurrent(context: *mut c_void);
    cuDevicePrimaryCtxRelease_v2(device: i32);
    cuDevicePrimaryCtxRetain(context: *mut *mut c_void, device: i32);
    cuMemAddressFree(address: u64, size: usize);
    cuMemAddressReserve(address: *mut u64, size: usize, alignment: usize, requested: u64, flags: u64);
    cuMemCreate(handle: *mut u64, size: usize, properties: *const AllocationProp, flags: u64);
    cuMemExportToShareableHandle(output: *mut c_void, handle: u64, handle_type: u32, flags: u64);
    cuMemGetAllocationPropertiesFromHandle(properties: *mut AllocationProp, handle: u64);
    cuMemHostRegister_v2(address: *mut c_void, size: usize, flags: u32);
    cuMemHostGetFlags(flags: *mut u32, address: *mut c_void);
    cuMemHostUnregister(address: *mut c_void);
    cuMemImportFromShareableHandle(handle: *mut u64, shareable: *mut c_void, handle_type: u32);
    cuMemMap(address: u64, size: usize, offset: usize, handle: u64, flags: u64);
    cuMemRelease(handle: u64);
    cuMemRetainAllocationHandle(handle: *mut u64, address: *mut c_void);
    cuMemSetAccess(address: u64, size: usize, access: *const Access, count: usize);
    cuMemUnmap(address: u64, size: usize);
    cuMemcpyDtoHAsync_v2(host: *mut c_void, device: u64, size: usize, stream: *mut c_void);
    cuMemcpyHtoDAsync_v2(device: u64, host: *const c_void, size: usize, stream: *mut c_void);
    cuMulticastAddDevice(group: u64, device: i32);
    cuMulticastBindAddr(group: u64, offset: usize, address: u64, size: usize, flags: u64);
    cuMulticastBindAddr_v2(group: u64, device: i32, offset: usize, address: u64, size: usize, flags: u64);
    cuMulticastBindMem(group: u64, offset: usize, member: u64, member_offset: usize, size: usize, flags: u64);
    cuMulticastBindMem_v2(group: u64, device: i32, offset: usize, member: u64, member_offset: usize, size: usize, flags: u64);
    cuMulticastCreate(group: *mut u64, properties: *const MulticastProp);
    cuMulticastGetGranularity(granularity: *mut usize, properties: *const MulticastProp, flags: u32);
    cuMulticastUnbind(group: u64, device: i32, offset: usize, size: usize);
    cuStreamCreate(stream: *mut *mut c_void, flags: u32);
    cuStreamDestroy_v2(stream: *mut c_void);
    cuStreamSynchronize(stream: *mut c_void);
}
