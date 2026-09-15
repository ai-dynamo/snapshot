// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#![allow(non_snake_case)]
mod elf;
mod loader;
mod process;

use cuinterpose_abi::*;
use std::ffi::{CStr, c_char, c_void};

#[unsafe(no_mangle)]
pub static cuinterpose_build_info: BuildInfo = BuildInfo {
    cuda_version: CUDA_VERSION,
    protocol_version: 2,
};

macro_rules! wrappers {
    ($($name:ident($($arg:ident: $ty:ty),*);)*) => {
        $(
            #[unsafe(no_mangle)]
            pub unsafe extern "C" fn $name($($arg: $ty),*) -> i32 {
                boundary(&loader::FAILED, UNKNOWN, || {
                    let Some(_guard) = process::Guard::enter() else { return NOT_SUPPORTED; };
                    let Some(core) = loader::core() else { return NOT_INITIALIZED; };
                    unsafe { (core.$name)($($arg),*) }
                })
            }
        )*

        fn memory_wrapper(name: &[u8]) -> *mut c_void {
            match name {
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
    boundary(&loader::FAILED, std::ptr::null_mut(), || {
        if name.is_null() {
            return std::ptr::null_mut();
        }
        let name = unsafe { CStr::from_ptr(name) };
        let original = loader::proxy(handle, name, caller);
        if original.is_null() {
            return original;
        }
        let wrapper = replacement(name.to_bytes());
        if wrapper.is_null() {
            return original;
        }
        loader::retain_provider(original);
        wrapper
    })
}

/// Use CUDA's returned function identity, not our header version, to select
/// the wrapper ABI. Unknown pointers remain untouched; no ABI is invented.
unsafe fn substitute(output: *mut *mut c_void) {
    if output.is_null() || unsafe { (*output).is_null() } {
        return;
    }
    let address = unsafe { *output };
    let mut info: libc::Dl_info = unsafe { std::mem::zeroed() };
    if unsafe { libc::dladdr(address, &mut info) } == 0
        || info.dli_sname.is_null()
        || info.dli_saddr != address
    {
        return;
    }
    let wrapper = replacement(unsafe { CStr::from_ptr(info.dli_sname) }.to_bytes());
    if !wrapper.is_null() {
        loader::retain_provider(address);
        unsafe {
            *output = wrapper;
        }
    }
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuGetProcAddress(
    name: *const c_char,
    out: *mut *mut c_void,
    version: i32,
    flags: u64,
) -> i32 {
    boundary(&loader::FAILED, UNKNOWN, || {
        let Some(_guard) = process::Guard::enter() else {
            return NOT_SUPPORTED;
        };
        let address = unsafe { loader::resolve(c"cuGetProcAddress".as_ptr()) };
        if address.is_null() {
            return NOT_INITIALIZED;
        }
        let function: unsafe extern "C" fn(*const c_char, *mut *mut c_void, i32, u64) -> i32 =
            unsafe { std::mem::transmute(address) };
        let result = unsafe { function(name, out, version, flags) };
        if result == SUCCESS {
            unsafe {
                substitute(out);
            }
        }
        result
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuGetProcAddress_v2(
    name: *const c_char,
    out: *mut *mut c_void,
    version: i32,
    flags: u64,
    status: *mut i32,
) -> i32 {
    boundary(&loader::FAILED, UNKNOWN, || {
        let Some(_guard) = process::Guard::enter() else {
            return NOT_SUPPORTED;
        };
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
            unsafe {
                substitute(out);
            }
        }
        result
    })
}

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
    ($name:ident) => {
        #[unsafe(no_mangle)]
        pub unsafe extern "C" fn $name(
            name: *const c_char,
            out: *mut *mut c_void,
            flags: u64,
            status: *mut i32,
        ) -> i32 {
            boundary(&loader::FAILED, 999, || {
                let Some(_guard) = process::Guard::enter() else {
                    return NOT_SUPPORTED;
                };
                let address =
                    unsafe { loader::resolve(concat!(stringify!($name), "\0").as_ptr().cast()) };
                if address.is_null() {
                    return 3;
                }
                let function: unsafe extern "C" fn(
                    *const c_char,
                    *mut *mut c_void,
                    u64,
                    *mut i32,
                ) -> i32 = unsafe { std::mem::transmute(address) };
                let result = unsafe { function(name, out, flags, status) };
                if result == SUCCESS && (status.is_null() || unsafe { *status } == 0) {
                    unsafe {
                        substitute(out);
                    }
                }
                result
            })
        }
    };
    ($name:ident, versioned) => {
        #[unsafe(no_mangle)]
        pub unsafe extern "C" fn $name(
            name: *const c_char,
            out: *mut *mut c_void,
            version: u32,
            flags: u64,
            status: *mut i32,
        ) -> i32 {
            boundary(&loader::FAILED, 999, || {
                let Some(_guard) = process::Guard::enter() else {
                    return NOT_SUPPORTED;
                };
                let address =
                    unsafe { loader::resolve(concat!(stringify!($name), "\0").as_ptr().cast()) };
                if address.is_null() {
                    return 3;
                }
                let function: unsafe extern "C" fn(
                    *const c_char,
                    *mut *mut c_void,
                    u32,
                    u64,
                    *mut i32,
                ) -> i32 = unsafe { std::mem::transmute(address) };
                let result = unsafe { function(name, out, version, flags, status) };
                if result == SUCCESS && (status.is_null() || unsafe { *status } == 0) {
                    unsafe {
                        substitute(out);
                    }
                }
                result
            })
        }
    };
}
runtime_resolver!(cudaGetDriverEntryPoint);
runtime_resolver!(cudaGetDriverEntryPoint_ptsz);
runtime_resolver!(cudaGetDriverEntryPointByVersion, versioned);
runtime_resolver!(cudaGetDriverEntryPointByVersion_ptsz, versioned);

#[unsafe(no_mangle)]
pub unsafe extern "C" fn cuinterpose_debug_stats(output: *mut DebugStats) {
    boundary(&loader::FAILED, (), || {
        let Some(_guard) = process::Guard::enter() else {
            return;
        };
        if !output.is_null() {
            if let Some(core) = loader::core() {
                unsafe {
                    (core.debug_stats)(output);
                }
            }
        }
    });
}
