// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! One typed boundary for CUDA entry points, resolved through the frontend.
//! Signatures follow NVIDIA cuda.h/cudaTypedefs.h; optional symbols stay lazy.
//! Raw calls retain driver-written outputs even on failure.
//! cudarc supplies CUDA types/constants, not its loader or safe resource owners.
//! Integer results preserve driver codes absent from cudarc's CUresult enum.

use cuinterpose_abi::{
    Access, AllocationHandle, AllocationProp, Device, DevicePtr, MulticastProp, POSIX_FD,
};
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

pub fn import_posix(fd: BorrowedFd<'_>) -> Result<AllocationHandle> {
    let mut handle = 0;
    unsafe {
        cuMemImportFromShareableHandle(
            &mut handle,
            fd.as_raw_fd() as usize as *mut c_void,
            POSIX_FD,
        )
    }?;
    Ok(handle)
}

pub fn export_posix(handle: AllocationHandle) -> Result<OwnedFd> {
    let mut fd = -1;
    unsafe { cuMemExportToShareableHandle((&mut fd as *mut i32).cast(), handle, POSIX_FD, 0) }?;
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
    cuDeviceGetUuid(uuid: *mut cudarc::driver::sys::CUuuid, device: Device);
    cuCtxGetCurrent(context: *mut *mut c_void);
    cuCtxGetDevice(device: *mut Device);
    cuCtxSetCurrent(context: *mut c_void);
    cuDevicePrimaryCtxRelease_v2(device: Device);
    cuDevicePrimaryCtxRetain(context: *mut *mut c_void, device: Device);
    cuMemAddressFree(address: DevicePtr, size: usize);
    cuMemAddressReserve(address: *mut DevicePtr, size: usize, alignment: usize, requested: DevicePtr, flags: u64);
    cuMemCreate(handle: *mut AllocationHandle, size: usize, properties: *const AllocationProp, flags: u64);
    cuMemGetAllocationGranularity(size: *mut usize, properties: *const AllocationProp, flags: u32);
    cuMemExportToShareableHandle(output: *mut c_void, handle: AllocationHandle, handle_type: u32, flags: u64);
    cuMemGetAllocationPropertiesFromHandle(properties: *mut AllocationProp, handle: AllocationHandle);
    cuMemHostRegister_v2(address: *mut c_void, size: usize, flags: u32);
    cuMemHostGetFlags(flags: *mut u32, address: *mut c_void);
    cuMemHostUnregister(address: *mut c_void);
    cuMemImportFromShareableHandle(handle: *mut AllocationHandle, shareable: *mut c_void, handle_type: u32);
    cuMemMap(address: DevicePtr, size: usize, offset: usize, handle: AllocationHandle, flags: u64);
    cuMemRelease(handle: AllocationHandle);
    cuMemRetainAllocationHandle(handle: *mut AllocationHandle, address: *mut c_void);
    cuMemSetAccess(address: DevicePtr, size: usize, access: *const Access, count: usize);
    cuMemUnmap(address: DevicePtr, size: usize);
    cuMemcpyDtoHAsync_v2(host: *mut c_void, device: DevicePtr, size: usize, stream: *mut c_void);
    cuMemcpyHtoDAsync_v2(device: DevicePtr, host: *const c_void, size: usize, stream: *mut c_void);
    cuMulticastAddDevice(group: AllocationHandle, device: Device);
    cuMulticastBindAddr(group: AllocationHandle, offset: usize, address: DevicePtr, size: usize, flags: u64);
    cuMulticastBindAddr_v2(group: AllocationHandle, device: Device, offset: usize, address: DevicePtr, size: usize, flags: u64);
    cuMulticastBindMem(group: AllocationHandle, offset: usize, member: AllocationHandle, member_offset: usize, size: usize, flags: u64);
    cuMulticastBindMem_v2(group: AllocationHandle, device: Device, offset: usize, member: AllocationHandle, member_offset: usize, size: usize, flags: u64);
    cuMulticastCreate(group: *mut AllocationHandle, properties: *const MulticastProp);
    cuMulticastGetGranularity(granularity: *mut usize, properties: *const MulticastProp, flags: u32);
    cuMulticastUnbind(group: AllocationHandle, device: Device, offset: usize, size: usize);
    cuStreamCreate(stream: *mut *mut c_void, flags: u32);
    cuStreamDestroy_v2(stream: *mut c_void);
    cuStreamSynchronize(stream: *mut c_void);
}
