// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! CUDA preload exports and caller-aware symbol lookup; state lives in the lazy core.
//!
//! CUDA pointers and handles obey the corresponding NVIDIA driver/runtime API
//! contracts. The shim borrows them only for the call; it does not own caller
//! buffers. Rust unwinds are contained and poison this library's generation.

#![allow(non_snake_case, reason = "CUDA ABI exports retain NVIDIA symbol names")]
mod elf;
mod loader;
mod process;

use cuinterpose_abi::*;
use std::ffi::{CStr, c_char, c_void};

#[unsafe(no_mangle)]
pub static cuinterpose_build_info: BuildInfo = BuildInfo {
    cuda_version: CUDA_VERSION,
    protocol_version: 4,
};

macro_rules! wrappers {
    ($($name:ident($($arg:ident: $ty:ty),*);)*) => {
        $(
            #[doc = concat!("Intercepts `", stringify!($name), "` through the typed core table.")]
            ///
            /// # Safety
            /// Pointers, sizes, handles, and context lifetime must satisfy the
            /// identically named CUDA API's contract for the entire call.
            #[unsafe(no_mangle)]
            pub unsafe extern "C" fn $name($($arg: $ty),*) -> i32 {
                boundary(&loader::G_FAILED, UNKNOWN, || {
                    let Some(core) = loader::core() else { return NOT_INITIALIZED; };
                    unsafe { (core.$name)($($arg),*) }
                })
            }
        )*

        fn memory_wrapper(name: &[u8]) -> *mut c_void {
            match name {
        b"cuInit" => cuInit as *const () as *mut c_void,
                $(x if x == stringify!($name).as_bytes() => $name as *const () as *mut c_void,)*
                _ => std::ptr::null_mut(),
            }
        }
    }
}
memory_api!(wrappers);

fn replacement(name: &[u8]) -> *mut c_void {
    let memory = memory_wrapper(name);
    if !memory.is_null() {
        return memory;
    }
    match name {
        b"cuGetProcAddress" => cuGetProcAddress as *const () as *mut c_void,
        b"cuGetProcAddress_v2" => cuGetProcAddress_v2 as *const () as *mut c_void,
        b"cuGetProcAddress_v2_ptsz" => cuGetProcAddress_v2_ptsz as *const () as *mut c_void,
        b"cudaGetDriverEntryPoint" => cudaGetDriverEntryPoint as *const () as *mut c_void,
        b"cudaGetDriverEntryPoint_ptsz" => cudaGetDriverEntryPoint_ptsz as *const () as *mut c_void,
        b"cudaGetDriverEntryPointByVersion" => {
            cudaGetDriverEntryPointByVersion as *const () as *mut c_void
        }
        b"cudaGetDriverEntryPointByVersion_ptsz" => {
            cudaGetDriverEntryPointByVersion_ptsz as *const () as *mut c_void
        }
        _ => std::ptr::null_mut(),
    }
}

/// Resolves a symbol while preserving the original caller's lookup scope.
///
/// # Safety
/// `handle` must be a live dlopen handle or a supported RTLD pseudo-handle;
/// `name` must point to a readable NUL-terminated string for this call.
// Rust's stable ABI has no __builtin_return_address equivalent. Capture it
// before creating a Rust frame, then tail-enter the ordinary C-ABI dispatcher.
// This is caller identification for run-ai-style lookup, not a glibc trampoline.
#[unsafe(naked)]
#[unsafe(no_mangle)]
pub unsafe extern "C" fn dlsym(_handle: *mut c_void, _name: *const c_char) -> *mut c_void {
    std::arch::naked_asm!("mov rdx, [rsp]", "jmp {}", sym lookup);
}

unsafe extern "C" fn lookup(
    handle: *mut c_void,
    name: *const c_char,
    caller: *const c_void,
) -> *mut c_void {
    boundary(&loader::G_FAILED, std::ptr::null_mut(), || {
        if name.is_null() {
            return std::ptr::null_mut();
        }
        let name = unsafe { CStr::from_ptr(name) };
        let original = loader::proxy(handle, name, caller);
        if original.is_null() || handle == libc::RTLD_NEXT {
            return original;
        }
        let wrapper = replacement(name.to_bytes());
        if wrapper.is_null() || original == wrapper {
            return original;
        }
        match loader::retain_provider(handle, original) {
            Ok(true) => wrapper,
            Ok(false) => original,
            Err(()) => std::ptr::null_mut(),
        }
    })
}

/// A successful tracked query must identify a supported ABI in that API's
/// family. Anonymous stubs and unrelated aliases cannot silently escape
/// tracking. The real resolver's failures never enter this path.
unsafe fn finish_query(name: *const c_char, output: *mut *mut c_void) -> i32 {
    if name.is_null() || output.is_null() {
        return INVALID_VALUE;
    }
    let requested = unsafe { CStr::from_ptr(name) }.to_bytes();
    let address = unsafe { *output };
    if !replacement(requested).is_null() {
        let mut info: libc::Dl_info = unsafe { std::mem::zeroed() };
        let mut wrapper = std::ptr::null_mut();
        if !address.is_null()
            && unsafe { libc::dladdr(address, &mut info) } != 0
            && !info.dli_sname.is_null()
            && info.dli_saddr == address
        {
            let actual = unsafe { CStr::from_ptr(info.dli_sname) }.to_bytes();
            let same_api = actual == requested
                || matches!(
                    (requested, actual),
                    (
                        b"cuGetProcAddress",
                        b"cuGetProcAddress_v2" | b"cuGetProcAddress_v2_ptsz"
                    ) | (b"cuGetProcAddress_v2", b"cuGetProcAddress_v2_ptsz")
                        | (b"cuMulticastBindMem", b"cuMulticastBindMem_v2")
                        | (b"cuMulticastBindAddr", b"cuMulticastBindAddr_v2")
                );
            if same_api {
                let candidate = replacement(actual);
                // libcudart may delegate to an already-intercepted driver
                // resolver. Accept only the exact known wrapper address, not
                // arbitrary symbols from a CUDA- or cuinterpose-named DSO.
                if !candidate.is_null()
                    && (address == candidate
                        || loader::retain_provider(libc::RTLD_DEFAULT, address) == Ok(true))
                {
                    wrapper = candidate;
                }
            }
        }
        if wrapper.is_null() {
            unsafe {
                *output = std::ptr::null_mut();
            }
            return NOT_SUPPORTED;
        }
        unsafe {
            *output = wrapper;
        }
    }
    let result = match loader::core() {
        Some(core) => unsafe { (core.ensure_ready)() },
        None => NOT_INITIALIZED,
    };
    if result != SUCCESS {
        unsafe {
            *output = std::ptr::null_mut();
        }
    }
    result
}

/// Initializes CUDA and makes this process's shim endpoint ready.
///
/// # Safety
/// The installed CUDA provider must expose the documented `cuInit` C ABI.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuInit(flags: u32) -> i32 {
    boundary(&loader::G_FAILED, UNKNOWN, || {
        let address = unsafe { loader::resolve(c"cuInit".as_ptr()) };
        if address.is_null() {
            return NOT_INITIALIZED;
        }
        let function: unsafe extern "C" fn(u32) -> i32 = unsafe { std::mem::transmute(address) };
        let result = unsafe { function(flags) };
        if result != SUCCESS {
            return result;
        }
        match loader::core() {
            Some(core) => unsafe { (core.ensure_ready)() },
            None => NOT_INITIALIZED,
        }
    })
}

/// Resolves a driver entry point and substitutes a supported shim wrapper.
///
/// # Safety
/// `name` is a readable NUL-terminated string; `out` is writable pointer storage.
/// The caller must invoke the returned pointer with the driver-selected ABI.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuGetProcAddress(
    name: *const c_char,
    out: *mut *mut c_void,
    version: i32,
    flags: u64,
) -> i32 {
    boundary(&loader::G_FAILED, UNKNOWN, || {
        let address = unsafe { loader::resolve(c"cuGetProcAddress".as_ptr()) };
        if address.is_null() {
            return NOT_INITIALIZED;
        }
        let function: unsafe extern "C" fn(*const c_char, *mut *mut c_void, i32, u64) -> i32 =
            unsafe { std::mem::transmute(address) };
        let result = unsafe { function(name, out, version, flags) };
        if result == SUCCESS {
            return unsafe { finish_query(name, out) };
        }
        result
    })
}

/// Resolves a driver entry point with the v2 query-status output.
///
/// # Safety
/// The `cuGetProcAddress` pointer contract applies; non-null `status` must
/// additionally point to writable query-status storage.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuGetProcAddress_v2(
    name: *const c_char,
    out: *mut *mut c_void,
    version: i32,
    flags: u64,
    status: *mut i32,
) -> i32 {
    boundary(&loader::G_FAILED, UNKNOWN, || {
        let address = unsafe { loader::resolve(c"cuGetProcAddress_v2".as_ptr()) };
        if address.is_null() {
            return NOT_INITIALIZED;
        }
        let function: unsafe extern "C" fn(
            *const c_char,
            *mut *mut c_void,
            i32,
            u64,
            *mut i32,
        ) -> i32 = unsafe { std::mem::transmute(address) };
        let result = unsafe { function(name, out, version, flags, status) };
        if result == SUCCESS && (status.is_null() || unsafe { *status } == 0) {
            return unsafe { finish_query(name, out) };
        }
        result
    })
}

/// Resolves a v2 entry point with the default per-thread stream flag.
///
/// # Safety
/// All pointer and returned-function requirements of `cuGetProcAddress_v2` apply.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuGetProcAddress_v2_ptsz(
    name: *const c_char,
    out: *mut *mut c_void,
    version: i32,
    mut flags: u64,
    status: *mut i32,
) -> i32 {
    if flags & 3 == 0 {
        flags |= 2;
    }
    unsafe { cuGetProcAddress_v2(name, out, version, flags, status) }
}

macro_rules! runtime_resolver {
    ($name:ident($($version:ident: $version_ty:ty)?)) => {
        /// Resolves a runtime-selected driver entry point through the shim.
        ///
        /// # Safety
        /// `name` is NUL-terminated; `out` and non-null `status` are writable.
        /// The returned function must be called using its CUDA-selected ABI.
        #[unsafe(no_mangle)]
        pub unsafe extern "C" fn $name(
            name: *const c_char,
            out: *mut *mut c_void,
            $($version: $version_ty,)?
            flags: u64,
            status: *mut i32,
        ) -> i32 {
            boundary(&loader::G_FAILED, UNKNOWN, || {
                let address =
                    unsafe { loader::resolve(concat!(stringify!($name), "\0").as_ptr().cast()) };
                if address.is_null() {
                    return NOT_INITIALIZED;
                }
                let function: unsafe extern "C" fn(
                    *const c_char,
                    *mut *mut c_void,
                    $($version_ty,)?
                    u64,
                    *mut i32,
                ) -> i32 = unsafe { std::mem::transmute(address) };
                let result = unsafe { function(name, out, $($version,)? flags, status) };
                if result == SUCCESS && (status.is_null() || unsafe { *status } == 0) {
                    return unsafe { finish_query(name, out) };
                }
                result
            })
        }
    };
}
runtime_resolver!(cudaGetDriverEntryPoint());
runtime_resolver!(cudaGetDriverEntryPoint_ptsz());
runtime_resolver!(cudaGetDriverEntryPointByVersion(version: u32));
runtime_resolver!(cudaGetDriverEntryPointByVersion_ptsz(version: u32));

/// Writes diagnostic counters without exposing Rust-owned state.
///
/// # Safety
/// Non-null `output` must be aligned, writable storage for one `DebugStats`.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuinterpose_debug_stats(output: *mut DebugStats) {
    boundary(&loader::G_FAILED, (), || {
        if !output.is_null()
            && let Some(core) = loader::core()
        {
            unsafe {
                (core.debug_stats)(output);
            }
        }
    });
}
