// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! One typed boundary for CUDA entry points, resolved through the frontend.
//! Signatures follow NVIDIA cuda.h/cudaTypedefs.h; missing optional symbols fail on use.
//! Raw calls retain driver-written outputs even on failure.
//! cudarc supplies CUDA types/constants, not its loader or safe resource owners.

use crate::error::{Error, Result};
use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_NOT_INITIALIZED, CUDA_SUCCESS,
};
use cudarc::driver::sys::{
    CUcontext, CUdevice, CUdevice_attribute, CUdeviceptr, CUmemAccessDesc,
    CUmemAllocationGranularity_flags, CUmemAllocationHandleType, CUmemAllocationProp,
    CUmemGenericAllocationHandle, CUmulticastObjectProp, CUresult, CUstream_flags,
};
use std::ffi::c_void;
use std::os::fd::{AsRawFd, BorrowedFd, FromRawFd, OwnedFd};

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
        return Err(Error::from(CUDA_ERROR_INVALID_HANDLE));
    }
    // CUDA transfers ownership of a fresh descriptor on successful POSIX export.
    let fd = unsafe { OwnedFd::from_raw_fd(fd) };
    rustix::io::fcntl_setfd(&fd, rustix::io::FdFlags::CLOEXEC)
        .map_err(|error| Error::io("mark CUDA export close-on-exec", error))?;
    Ok(fd)
}
pub fn result(code: CUresult) -> Result<()> {
    if code == CUDA_SUCCESS {
        Ok(())
    } else {
        Err(Error::Cuda(code))
    }
}

macro_rules! functions {
    (required { $($required:ident($($rarg:ident: $rty:ty),*);)* }
     optional { $($optional:ident($($oarg:ident: $oty:ty),*);)* }) => {
        functions!(@define $($required($($rarg: $rty),*);)* $($optional($($oarg: $oty),*);)*);

        pub(crate) fn initialize() -> Result<()> {
            let symbols = Symbols::resolve();
            $(if symbols.$required.is_none() {
                return Err(Error::Startup(concat!("missing required CUDA symbol ", stringify!($required))));
            })*
            let _ = SYMBOLS.set(symbols);
            Ok(())
        }
    };
    (@define $($name:ident($($arg:ident: $ty:ty),*);)*) => {
        struct Symbols {
            $($name: Option<unsafe extern "C" fn($($ty),*) -> CUresult>,)*
        }
        static SYMBOLS: std::sync::OnceLock<Symbols> = std::sync::OnceLock::new();

        impl Symbols {
            fn resolve() -> Self {
                // Resolve privately: neither a state mutex nor OnceLock initialization
                // may be held while the frontend enters the dynamic loader. Concurrent
                // runtime candidates can publish equivalent tables without waiting.
                Self {
                    $($name: {
                        let address = crate::driver(
                            std::ffi::CStr::from_bytes_with_nul(
                                concat!(stringify!($name), "\0").as_bytes()).expect("static CUDA symbol"));
                        if address.is_null() {
                            None
                        } else {
                            // The frontend resolves this exact NVIDIA driver signature.
                            Some(unsafe { std::mem::transmute::<*mut c_void,
                                unsafe extern "C" fn($($ty),*) -> CUresult>(address) })
                        }
                    },)*
                }
            }
        }

        pub mod symbols {
            use super::*;
            $(
                pub fn $name() -> Result<unsafe extern "C" fn($($ty),*) -> CUresult> {
                    SYMBOLS.get().and_then(|symbols| symbols.$name)
                        .ok_or(Error::from(CUDA_ERROR_NOT_INITIALIZED))
                }
            )*
        }
        $(
        pub unsafe fn $name($($arg: $ty),*) -> Result<()> {
            let function = symbols::$name()?;
            result(unsafe { function($($arg),*) })
        }
    )*};
}

// VMM, contexts, and host-copy primitives are required at startup. Multicast
// is optional so drivers without those entry points can run unicast workloads.
functions! {
    required {
        cuDeviceGetAttribute(value: *mut i32, attribute: CUdevice_attribute, device: CUdevice);
        cuCtxDestroy(context: CUcontext);
        cuCtxDestroy_v2(context: CUcontext);
        cuCtxSynchronize();
        cuMemFree_v2(address: CUdeviceptr);
        cuMemGetAddressRange_v2(base: *mut CUdeviceptr, size: *mut usize, address: CUdeviceptr);
        cuCtxGetCurrent(context: *mut *mut c_void);
        cuCtxGetDevice(device: *mut CUdevice);
        cuCtxSetCurrent(context: *mut c_void);
        cuDevicePrimaryCtxRelease(device: CUdevice);
        cuDevicePrimaryCtxRelease_v2(device: CUdevice);
        cuDevicePrimaryCtxReset(device: CUdevice);
        cuDevicePrimaryCtxReset_v2(device: CUdevice);
        cuDevicePrimaryCtxGetState(device: CUdevice, flags: *mut u32, active: *mut i32);
        cuDevicePrimaryCtxRetain(context: *mut *mut c_void, device: CUdevice);
        cuMemAddressFree(address: CUdeviceptr, size: usize);
        cuMemAddressReserve(address: *mut CUdeviceptr, size: usize, alignment: usize, requested: CUdeviceptr, flags: u64);
        cuMemCreate(handle: *mut CUmemGenericAllocationHandle, size: usize, properties: *const CUmemAllocationProp, flags: u64);
        cuMemGetAllocationGranularity(size: *mut usize, properties: *const CUmemAllocationProp, flags: CUmemAllocationGranularity_flags);
        cuMemExportToShareableHandle(output: *mut c_void, handle: CUmemGenericAllocationHandle, handle_type: CUmemAllocationHandleType, flags: u64);
        cuMemGetAllocationPropertiesFromHandle(properties: *mut CUmemAllocationProp, handle: CUmemGenericAllocationHandle);
        cuMemHostRegister_v2(address: *mut c_void, size: usize, flags: u32);
        cuMemHostUnregister(address: *mut c_void);
        cuMemImportFromShareableHandle(handle: *mut CUmemGenericAllocationHandle, shareable: *mut c_void, handle_type: CUmemAllocationHandleType);
        cuMemMap(address: CUdeviceptr, size: usize, offset: usize, handle: CUmemGenericAllocationHandle, flags: u64);
        cuMemRelease(handle: CUmemGenericAllocationHandle);
        cuMemRetainAllocationHandle(handle: *mut CUmemGenericAllocationHandle, address: *mut c_void);
        cuMemSetAccess(address: CUdeviceptr, size: usize, access: *const CUmemAccessDesc, count: usize);
        cuMemUnmap(address: CUdeviceptr, size: usize);
        cuMemcpyDtoHAsync_v2(host: *mut c_void, device: CUdeviceptr, size: usize, stream: *mut c_void);
        cuMemcpyHtoDAsync_v2(device: CUdeviceptr, host: *const c_void, size: usize, stream: *mut c_void);
        cuStreamCreate(stream: *mut *mut c_void, flags: CUstream_flags);
        cuStreamDestroy_v2(stream: *mut c_void);
        cuStreamSynchronize(stream: *mut c_void);
    }
    optional {
    }
}

pub(super) fn context() -> Result<usize> {
    let mut context = std::ptr::null_mut::<c_void>();
    unsafe { crate::driver::cuCtxGetCurrent(&mut context) }?;
    Ok(context as usize)
}

pub struct Context {
    previous: *mut c_void,
    primary: Option<i32>,
    changed: bool,
}

impl Context {
    /// Runs in the recorded context. Only when `context` is zero does `device`
    /// select a primary context to retain for the duration of the call.
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

#[cfg(test)]
mod tests {
    use super::*;
    use cuinterpose_abi::{ABI_VERSION, FrontendAbi};
    use std::ffi::{CStr, c_char};
    use std::sync::atomic::{AtomicBool, Ordering};

    #[test]
    fn required_symbols_fail_at_startup() {
        crate::tests::in_child_process("driver::tests::required_symbols_fail_at_startup", || {
            static MISSING_CREATE: AtomicBool = AtomicBool::new(true);
            unsafe extern "C" fn resolve(name: *const c_char) -> *mut c_void {
                let name = unsafe { CStr::from_ptr(name) }.to_bytes();
                if name.starts_with(b"cuMulticast")
                    || (name == b"cuMemCreate" && MISSING_CREATE.load(Ordering::Relaxed))
                {
                    std::ptr::null_mut()
                } else {
                    crate::tests::unused_driver_symbol()
                }
            }
            let frontend = FrontendAbi {
                version: ABI_VERSION,
                size: size_of::<FrontendAbi>() as u32,
                resolve,
            };
            let mut output = std::ptr::null();
            assert_eq!(
                unsafe { crate::cuinterpose_core_init(&frontend, &mut output) },
                CUDA_SUCCESS
            );
            assert!(matches!(
                initialize(),
                Err(Error::Startup("missing required CUDA symbol cuMemCreate"))
            ));
            assert!(SYMBOLS.get().is_none());
            MISSING_CREATE.store(false, Ordering::Relaxed);
            initialize().unwrap();
            assert!(symbols::cuMemCreate().is_ok());
        });
    }
}
