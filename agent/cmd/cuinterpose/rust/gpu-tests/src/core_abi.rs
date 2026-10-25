// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Registers a test frontend with the core directly, without the preloaded frontend. The
//! test supplies the resolver, so it can hide or wrap the driver symbols the core uses.
//! Tracked calls must then go through the returned backend table.

use crate::shim::artifacts;
use crate::sys::CUresult;
use crate::{cu, loader};
use cuinterpose_abi::{ABI_VERSION, BackendAbi, FrontendAbi, Resolve};
use std::ffi::{CStr, c_char, c_void};
use std::sync::OnceLock;

/// Calls a tracked entry point through a `&BackendAbi` and panics with its name and status
/// unless it succeeds.
#[macro_export]
macro_rules! tracked {
    ($backend:ident.$function:ident($($argument:expr),* $(,)?)) => {{
        // SAFETY: callers pass arguments that are valid for this CUDA entry point.
        let status = unsafe { ($backend.$function)($($argument),*) };
        assert_eq!(
            status,
            $crate::sys::CUresult::CUDA_SUCCESS,
            concat!(stringify!($function), " failed")
        );
    }};
}

pub type Initialize = unsafe extern "C" fn(*const FrontendAbi, *mut *const BackendAbi) -> CUresult;

/// The core's registration entry point.
pub fn initialize() -> Initialize {
    static INITIALIZE: OnceLock<Initialize> = OnceLock::new();
    *INITIALIZE.get_or_init(|| {
        let core = loader::open(&artifacts().core, libc::RTLD_NOW | libc::RTLD_LOCAL);
        loader::cast(loader::symbol(core, c"cuinterpose_core_init"))
    })
}

pub fn frontend(resolve: Resolve) -> FrontendAbi {
    FrontendAbi {
        version: ABI_VERSION,
        size: size_of::<FrontendAbi>() as u32,
        resolve,
    }
}

/// Registers a frontend that resolves symbols with `resolve` and returns the core's
/// backend table.
pub fn register(resolve: Resolve) -> &'static BackendAbi {
    let mut backend = std::ptr::null();
    let status = unsafe { initialize()(&frontend(resolve), &mut backend) };
    assert_eq!(status, CUresult::CUDA_SUCCESS, "register a test frontend");
    // SAFETY: the core's table remains valid until process exit.
    unsafe { &*backend }
}

/// Registers a frontend after the real cuInit, as the preloaded frontend does, and starts
/// the core's runtime. Returns the backend table and the runtime's startup status.
pub fn start(resolve: Resolve) -> (&'static BackendAbi, CUresult) {
    cu!(cuInit(0));
    let backend = register(resolve);
    let status = unsafe { (backend.ensure_cuinterpose_initialized)() };
    (backend, status)
}

/// The real driver's definition of `name`.
pub fn driver_symbol(name: &CStr) -> *mut c_void {
    static DRIVER: OnceLock<usize> = OnceLock::new();
    let driver = *DRIVER.get_or_init(|| {
        loader::dlopen(c"libcuda.so.1", libc::RTLD_NOW | libc::RTLD_LOCAL) as usize
    });
    loader::dlsym(driver as *mut c_void, name)
}

/// A resolver that returns the real driver's definitions.
///
/// # Safety
/// `name` must be a NUL-terminated string.
pub unsafe extern "C" fn resolve_driver(name: *const c_char) -> *mut c_void {
    driver_symbol(unsafe { CStr::from_ptr(name) })
}
