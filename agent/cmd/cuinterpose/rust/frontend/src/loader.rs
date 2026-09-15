// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cuinterpose_abi::{ABI_VERSION, Core, Host, Initialize};
use std::ffi::{CStr, CString, c_char, c_void};
use std::sync::atomic::{AtomicBool, AtomicPtr, Ordering};
use std::sync::{Mutex, OnceLock};

pub static FAILED: AtomicBool = AtomicBool::new(false);
static REAL_DLSYM: AtomicPtr<c_void> = AtomicPtr::new(std::ptr::null_mut());
type Dlsym = unsafe extern "C" fn(*mut c_void, *const c_char) -> *mut c_void;
static PROVIDERS: Mutex<Vec<usize>> = Mutex::new(Vec::new());
static CORE: OnceLock<Option<usize>> = OnceLock::new();
static INITIALIZING_CORE: AtomicBool = AtomicBool::new(false);

/// Like run-ai, anchor on dladdr's provider and discover its defined dlsym
/// without asking the intercepted dlsym. The image must be ELF64 little-endian.
pub fn real() -> Option<Dlsym> {
    let mut address = REAL_DLSYM.load(Ordering::Acquire);
    if address.is_null() {
        let anchor = libc::dladdr as *const () as *const c_void;
        let mut info: libc::Dl_info = unsafe { std::mem::zeroed() };
        if unsafe { libc::dladdr(anchor, &mut info) } == 0 || info.dli_fname.is_null() {
            return None;
        }
        let path = unsafe { CStr::from_ptr(info.dli_fname) };
        let image = super::elf::Image::open(path)?;
        let symbol = image.symbol(b"dladdr")?;
        if symbol.indirect
            || (info.dli_fbase as usize).checked_add(symbol.value)? != anchor as usize
        {
            return None;
        }
        let symbol = image.symbol(b"dlsym")?;
        if symbol.indirect {
            return None;
        }
        address = (info.dli_fbase as usize).checked_add(symbol.value)? as *mut c_void;
        REAL_DLSYM.store(address, Ordering::Release);
    }
    // SAFETY: the checked ELF entry is glibc's dlsym with this public signature.
    Some(unsafe { std::mem::transmute::<*mut c_void, Dlsym>(address) })
}

/// Retain CUDA providers before a pointer can be cached by the core. Handles
/// intentionally live until process exit; no dlclose races with CUDA replay.
pub fn retain_provider(address: *mut c_void) {
    let Some(_guard) = super::process::Guard::enter() else {
        return;
    };
    if address.is_null() {
        return;
    }
    let mut info: libc::Dl_info = unsafe { std::mem::zeroed() };
    if unsafe { libc::dladdr(address, &mut info) } == 0 || info.dli_fname.is_null() {
        return;
    }
    let path = unsafe { CStr::from_ptr(info.dli_fname) };
    let base = path
        .to_bytes()
        .rsplit(|c| *c == b'/')
        .next()
        .unwrap_or_default();
    if !base.starts_with(b"libcuda.so") && !base.starts_with(b"libcudart.so") {
        return;
    }
    // Never hold a Rust lock across dlopen: constructors can reenter the shim.
    let handle = unsafe { libc::dlopen(path.as_ptr(), libc::RTLD_LAZY | libc::RTLD_NOLOAD) };
    if handle.is_null() {
        FAILED.store(true, Ordering::Release);
        return;
    }
    let mut providers = PROVIDERS.lock().unwrap_or_else(|e| e.into_inner());
    if providers.contains(&(handle as usize)) {
        drop(providers);
        unsafe {
            libc::dlclose(handle);
        }
    } else {
        providers.push(handle as usize);
    }
}

pub fn proxy(handle: *mut c_void, name: &CStr, caller: *const c_void) -> *mut c_void {
    if handle == libc::RTLD_NEXT {
        return super::elf::after(caller, name);
    }
    // The next dlsym may belong to a subsequent interceptor. Calling it lets
    // that interceptor participate, as run-ai's resolver override does.
    let next = super::elf::after(proxy as *const () as *const c_void, c"dlsym");
    let resolver = if next.is_null() {
        match real() {
            Some(real) => real,
            None => return std::ptr::null_mut(),
        }
    } else {
        unsafe { std::mem::transmute::<*mut c_void, Dlsym>(next) }
    };
    unsafe { resolver(handle, name.as_ptr()) }
}

pub unsafe extern "C" fn resolve(name: *const c_char) -> *mut c_void {
    cuinterpose_abi::boundary(&FAILED, std::ptr::null_mut(), || {
        let Some(_guard) = super::process::Guard::enter() else {
            return std::ptr::null_mut();
        };
        if name.is_null() {
            return std::ptr::null_mut();
        }
        let name = unsafe { CStr::from_ptr(name) };
        let Some(real) = real() else {
            return std::ptr::null_mut();
        };
        let mut address = unsafe { real(libc::RTLD_NEXT, name.as_ptr()) };
        if address.is_null() {
            let handles = PROVIDERS.lock().unwrap_or_else(|e| e.into_inner()).clone();
            for handle in handles {
                address = unsafe { real(handle as *mut c_void, name.as_ptr()) };
                if !address.is_null() {
                    break;
                }
            }
        }
        if address.is_null() {
            // Own a provider even when CUDA was loaded RTLD_LOCAL without a
            // preceding intercepted dlsym. Do not acquire a Rust lock here.
            let library = if name.to_bytes().starts_with(b"cuda") {
                c"libcudart.so.13"
            } else {
                c"libcuda.so.1"
            };
            let handle =
                unsafe { libc::dlopen(library.as_ptr(), libc::RTLD_LAZY | libc::RTLD_LOCAL) };
            if !handle.is_null() {
                address = unsafe { real(handle, name.as_ptr()) };
                retain_provider(address);
                unsafe {
                    libc::dlclose(handle);
                }
            }
        } else {
            retain_provider(address);
        }
        address
    })
}

pub fn core() -> Option<&'static Core> {
    if let Some(pointer) = CORE.get() {
        return pointer.map(|address| unsafe { &*(address as *const Core) });
    }
    // Another initializer may be waiting for the loader lock held by this
    // caller's constructor. Never wait, even across threads. A transient
    // NOT_INITIALIZED reply does not poison or publish a failed core.
    if INITIALIZING_CORE
        .compare_exchange(false, true, Ordering::Acquire, Ordering::Relaxed)
        .is_err()
    {
        return None;
    }
    struct Initializing;
    impl Drop for Initializing {
        fn drop(&mut self) {
            INITIALIZING_CORE.store(false, Ordering::Release);
        }
    }
    let _initializing = Initializing;
    // Only the nonblocking admission winner can initialize this OnceLock.
    let pointer = CORE.get_or_init(|| {
        let real = real()?;
        let mut info: libc::Dl_info = unsafe { std::mem::zeroed() };
        if unsafe { libc::dladdr(core as *const () as *const c_void, &mut info) } == 0 {
            return None;
        }
        use std::os::unix::ffi::OsStrExt;
        let own = unsafe { CStr::from_ptr(info.dli_fname) };
        let own = std::path::Path::new(std::ffi::OsStr::from_bytes(own.to_bytes()));
        let path = own.parent()?.join("libcuinterpose_core.so");
        let path = CString::new(path.as_os_str().as_bytes()).ok()?;
        let handle = unsafe { libc::dlopen(path.as_ptr(), libc::RTLD_LAZY | libc::RTLD_LOCAL) };
        if handle.is_null() {
            return None;
        }
        let init = unsafe { real(handle, c"cuinterpose_core_init".as_ptr()) };
        if init.is_null() {
            unsafe {
                libc::dlclose(handle);
            }
            return None;
        }
        let init: Initialize = unsafe { std::mem::transmute(init) };
        let host = Host {
            version: ABI_VERSION,
            size: size_of::<Host>() as u32,
            resolve,
            enter: super::process::enter,
            leave: super::process::leave,
            origin_pid: super::process::ORIGIN_PID.load(Ordering::Acquire),
        };
        let mut output = std::ptr::null();
        let result = unsafe { init(&host, &mut output) };
        if result != 0 || output.is_null() {
            unsafe {
                libc::dlclose(handle);
            }
            return None;
        }
        // Check the fixed ABI prefix before creating a reference to the full
        // table. A different version may have allocated a shorter structure.
        let version = unsafe { std::ptr::addr_of!((*output).version).read() };
        let size = unsafe { std::ptr::addr_of!((*output).size).read() };
        if version != ABI_VERSION || size as usize != size_of::<Core>() {
            unsafe {
                libc::dlclose(handle);
            }
            return None;
        }
        // The sibling core is trusted to supply a fully initialized, non-null
        // callback table after this match; prefix checks do not validate an
        // arbitrary foreign table or its function-pointer targets.
        // The core owns process state and exported function pointers. Keep
        // its loader reference; neither normal shutdown nor fork drops it.
        super::process::publish(output);
        Some(output as usize)
    });
    pointer.map(|address| unsafe { &*(address as *const Core) })
}
