// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Dynamic loader calls from the test binary, which glibc treats as the calling object.
//! With the frontend preloaded, [`dlsym`] reaches the frontend's dlsym as an application's
//! lookups do.

#![allow(
    clippy::not_unsafe_ptr_arg_deref,
    reason = "every handle comes from dlopen or is an RTLD pseudo-handle"
)]

use crate::shim::artifacts;
use crate::sys::CUresult;
use std::ffi::{CStr, CString, c_char, c_int, c_void};
use std::os::unix::ffi::OsStrExt;
use std::path::Path;

// The old and new query entry points coexist in the installed driver.
pub type Query = unsafe extern "C" fn(*const c_char, *mut *mut c_void, c_int, u64) -> CUresult;
pub type QueryV2 =
    unsafe extern "C" fn(*const c_char, *mut *mut c_void, c_int, u64, *mut c_int) -> CUresult;
type Dlsym = unsafe extern "C" fn(*mut c_void, *const c_char) -> *mut c_void;

pub fn dlopen(name: &CStr, flags: c_int) -> *mut c_void {
    let handle = unsafe { libc::dlopen(name.as_ptr(), flags) };
    assert!(
        !handle.is_null(),
        "cannot load {name:?}: {:?}",
        take_dlerror()
    );
    handle
}

pub fn open(path: &Path, flags: c_int) -> *mut c_void {
    dlopen(&CString::new(path.as_os_str().as_bytes()).unwrap(), flags)
}

/// Looks up `name` as the application does.
pub fn dlsym(handle: *mut c_void, name: &CStr) -> *mut c_void {
    unsafe { libc::dlsym(handle, name.as_ptr()) }
}

pub fn symbol(handle: *mut c_void, name: &CStr) -> *mut c_void {
    let address = dlsym(handle, name);
    assert!(
        !address.is_null(),
        "missing required symbol {name:?}: {:?}",
        take_dlerror()
    );
    address
}

/// Resolves `name` in the global scope, where the preloaded frontend comes first.
pub fn global<F: Copy>(name: &CStr) -> F {
    cast(symbol(libc::RTLD_DEFAULT, name))
}

/// Looks up `name` with glibc's dlsym, which bypasses the frontend.
pub fn original(handle: *mut c_void, name: &CStr) -> *mut c_void {
    let version = c"GLIBC_2.34".as_ptr();
    let glibc: Dlsym = cast(unsafe { libc::dlvsym(libc::RTLD_NEXT, c"dlsym".as_ptr(), version) });
    unsafe { glibc(handle, name.as_ptr()) }
}

pub fn take_dlerror() -> Option<String> {
    let message = unsafe { libc::dlerror() };
    (!message.is_null()).then(|| {
        unsafe { CStr::from_ptr(message) }
            .to_string_lossy()
            .into_owned()
    })
}

/// Converts a symbol address to a function of type `F`.
pub fn cast<F: Copy>(address: *mut c_void) -> F {
    assert!(!address.is_null());
    // SAFETY: callers name a function with the signature F.
    unsafe { std::mem::transmute_copy::<*mut c_void, F>(&address) }
}

/// The address that a cuGetProcAddress variant reports for `name`.
pub fn proc_address(query: &CStr, name: &CStr) -> *mut c_void {
    let mut address = std::ptr::null_mut();
    let status = if query == c"cuGetProcAddress" {
        unsafe { global::<Query>(query)(name.as_ptr(), &mut address, 13010, 0) }
    } else {
        let mut found = 0;
        unsafe { global::<QueryV2>(query)(name.as_ptr(), &mut address, 13010, 0, &mut found) }
    };
    assert_eq!(status, CUresult::CUDA_SUCCESS, "{query:?}({name:?})");
    address
}

/// Looks up `name` in the loaded frontend.
pub fn frontend_address(name: &CStr) -> *mut c_void {
    let frontend = open(&artifacts().frontend, libc::RTLD_NOW | libc::RTLD_NOLOAD);
    dlsym(frontend, name)
}

/// Requires `address` to be the frontend's definition of `name`.
pub fn in_frontend(address: *mut c_void, name: &CStr) {
    let (file, symbol) = locate(address);
    assert!(
        file.contains("libcuinterpose.so"),
        "{name:?} resolved into {file}"
    );
    assert_eq!(symbol.as_deref(), Some(name), "{file}");
}

/// A query result must be the wrapper for the entry point the driver returned, which can
/// be a newer variant of the requested name, such as _v2.
pub fn wraps(actual: *mut c_void, expected: *mut c_void) {
    assert!(!expected.is_null());
    let (file, name) = locate(expected);
    in_frontend(
        actual,
        &name.unwrap_or_else(|| panic!("unnamed entry point in {file}")),
    );
}

/// The file that contains `address` and the name of its symbol.
fn locate(address: *mut c_void) -> (String, Option<CString>) {
    // SAFETY: Dl_info is plain data.
    let mut info: libc::Dl_info = unsafe { std::mem::zeroed() };
    assert_ne!(
        unsafe { libc::dladdr(address, &mut info) },
        0,
        "dladdr({address:?})"
    );
    let file = unsafe { CStr::from_ptr(info.dli_fname) };
    let name = (!info.dli_sname.is_null()).then(|| unsafe { CStr::from_ptr(info.dli_sname) });
    (
        file.to_string_lossy().into_owned(),
        name.map(CStr::to_owned),
    )
}
