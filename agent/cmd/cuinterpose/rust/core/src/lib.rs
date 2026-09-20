// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! In-process CUDA sharing state, lifecycle operations, and the private frontend ABI.
//!
//! Driver calls execute in the owning workload process. Rust-owned records,
//! locks, allocation storage, and panic state never cross the library boundary.

#![allow(non_snake_case, reason = "CUDA dispatch mirrors the NVIDIA ABI names")]
mod boundary;
mod driver;
mod handlers;
mod memory;
mod runtime;

use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_INITIALIZED, CUDA_ERROR_UNKNOWN, CUDA_SUCCESS,
};
use cudarc::driver::sys::{
    CUdevice, CUdeviceptr, CUipcMemHandle, CUmemAccessDesc, CUmemAllocationGranularity_flags,
    CUmemAllocationHandleType, CUmemAllocationProp, CUmemGenericAllocationHandle,
    CUmulticastGranularity_flags, CUmulticastObjectProp, CUresult,
};
use cuinterpose_abi::*;
use std::ffi::{CStr, c_void};
use std::sync::OnceLock;

static G_FRONTEND_ABI: OnceLock<FrontendAbi> = OnceLock::new();
use runtime::RUNTIME_FAILED;

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
                    return error.0;
                }
                boundary::call(&RUNTIME_FAILED, CUDA_ERROR_UNKNOWN, || {
                    let result = handlers::$name($($arg),*);
                    result.map_or_else(|code| code.0, |()| CUDA_SUCCESS)
                })
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
}

unsafe extern "C" fn ensure_cuinterpose_initialized() -> CUresult {
    boundary::call(&RUNTIME_FAILED, CUDA_ERROR_NOT_INITIALIZED, || {
        runtime::initialize().map_or_else(|error| error.0, |()| CUDA_SUCCESS)
    })
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
    boundary::call(&RUNTIME_FAILED, CUDA_ERROR_UNKNOWN, || {
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
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::c_char;

    #[test]
    fn frontend_registration_is_validated_and_idempotent() {
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

        unsafe extern "C" fn resolve(_: *const c_char) -> *mut c_void {
            panic!("ABI registration must not invoke the resolver");
        }
        unsafe extern "C" fn other_resolve(_: *const c_char) -> *mut c_void {
            std::ptr::null_mut()
        }
        let frontend = FrontendAbi {
            version: ABI_VERSION,
            size: size_of::<FrontendAbi>() as u32,
            resolve,
        };
        let barrier = std::sync::Barrier::new(32);
        std::thread::scope(|scope| {
            for _ in 0..32 {
                scope.spawn(|| {
                    barrier.wait();
                    for _ in 0..2 {
                        let mut output = std::ptr::null();
                        assert_eq!(
                            unsafe { cuinterpose_core_init(&frontend, &mut output) },
                            CUDA_SUCCESS
                        );
                        assert!(std::ptr::eq(output, &G_BACKEND_ABI));
                    }
                });
            }
        });
        let incompatible = FrontendAbi {
            resolve: other_resolve,
            ..frontend
        };
        let mut output = std::ptr::null();
        assert_eq!(
            unsafe { cuinterpose_core_init(&incompatible, &mut output) },
            CUDA_ERROR_INVALID_VALUE
        );
        assert!(output.is_null());
    }
}
