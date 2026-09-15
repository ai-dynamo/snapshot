// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Provider lifetime and nonblocking lazy-core initialization after ELF bootstrap.
//!
//! Release stores publish resolved pointers and sticky failure; Acquire loads
//! observe them before calling code. The admission flag prevents recursive or
//! concurrent initialization from waiting while a caller holds the loader lock.

use cuinterpose_abi::{ABI_VERSION, Core, Host, Initialize};
use std::ffi::{CStr, CString, c_char, c_void};
use std::sync::atomic::{AtomicBool, AtomicPtr, Ordering};
use std::sync::{Mutex, OnceLock};

pub static G_FAILED: AtomicBool = AtomicBool::new(false);
static G_REAL_DLSYM: AtomicPtr<c_void> = AtomicPtr::new(std::ptr::null_mut());
type Dlsym = unsafe extern "C" fn(*mut c_void, *const c_char) -> *mut c_void;
static G_PROVIDERS: Mutex<Vec<usize>> = Mutex::new(Vec::new());
static G_CORE: OnceLock<Option<usize>> = OnceLock::new();
static G_INITIALIZING_CORE: AtomicBool = AtomicBool::new(false);

thread_local! {
    static G_FORK_PROVIDERS: std::cell::RefCell<Option<std::sync::MutexGuard<'static, Vec<usize>>>> =
        const { std::cell::RefCell::new(None) };
}

pub fn fork_prepare() {
    G_FORK_PROVIDERS.with(|slot| {
        *slot.borrow_mut() = Some(G_PROVIDERS.lock().unwrap_or_else(|e| e.into_inner()));
    });
}

pub fn fork_unlock() {
    // This guard was acquired by the surviving fork thread. Unlock it rather
    // than overwriting an inherited Rust mutex. Provider references stay valid.
    G_FORK_PROVIDERS.with(|slot| drop(slot.borrow_mut().take()));
}

/// Like run-ai, anchor on dladdr's provider and discover its defined dlsym
/// without asking the intercepted dlsym. The image must be ELF64 little-endian.
pub fn real() -> Option<Dlsym> {
    let mut address = G_REAL_DLSYM.load(Ordering::Acquire);
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
        G_REAL_DLSYM.store(address, Ordering::Release);
    }
    // SAFETY: the checked ELF entry is glibc's dlsym with this public signature.
    Some(unsafe { std::mem::transmute::<*mut c_void, Dlsym>(address) })
}

// Public glibc link_map prefix, used only for a valid handle returned by dlopen.
// Unlike ELF file parsing this reads loader-owned, naturally aligned objects.
#[repr(C)]
struct LinkMap {
    load_bias: usize,
    name: *const c_char,
}

#[derive(PartialEq, Eq)]
enum Provider {
    Driver,
    Runtime,
}

fn provider(path: &CStr) -> Option<Provider> {
    let base = path.to_bytes().rsplit(|c| *c == b'/').next()?;
    for (stem, family) in [
        (b"libcuda.so".as_slice(), Provider::Driver),
        (b"libcudart.so".as_slice(), Provider::Runtime),
    ] {
        if base == stem
            || base.strip_prefix(stem).is_some_and(|suffix| {
                suffix.first() == Some(&b'.')
                    && suffix.len() > 1
                    && suffix[1..]
                        .split(|b| *b == b'.')
                        .all(|part| !part.is_empty() && part.iter().all(u8::is_ascii_digit))
            })
        {
            return Some(family);
        }
    }
    None
}

/// Qualify and retain one CUDA provider before substituting its pointer.
/// Concrete handles must select that provider family in the base namespace.
/// Handles intentionally live until exit; no dlclose races with CUDA replay.
pub fn retain_provider(selected: *mut c_void, address: *mut c_void) -> Result<bool, ()> {
    if address.is_null() {
        return Ok(false);
    }
    let mut info: libc::Dl_info = unsafe { std::mem::zeroed() };
    if unsafe { libc::dladdr(address, &mut info) } == 0 || info.dli_fname.is_null() {
        return Ok(false);
    }
    let path = unsafe { CStr::from_ptr(info.dli_fname) };
    let Some(family) = provider(path) else {
        return Ok(false);
    };
    if selected != libc::RTLD_DEFAULT && selected != libc::RTLD_NEXT {
        let mut map: *mut LinkMap = std::ptr::null_mut();
        let mut namespace: libc::c_long = -1;
        if unsafe {
            libc::dlinfo(
                selected,
                libc::RTLD_DI_LINKMAP,
                (&mut map as *mut *mut LinkMap).cast(),
            )
        } != 0
            || unsafe {
                libc::dlinfo(
                    selected,
                    libc::RTLD_DI_LMID,
                    (&mut namespace as *mut libc::c_long).cast(),
                )
            } != 0
            || namespace != 0
            || map.is_null()
        {
            return Ok(false);
        }
        let name = unsafe { (*map).name };
        if name.is_null() || provider(unsafe { CStr::from_ptr(name) }) != Some(family) {
            return Ok(false);
        }
    }
    // Never hold a Rust lock across dlopen: constructors can reenter the shim.
    let handle = unsafe { libc::dlopen(path.as_ptr(), libc::RTLD_LAZY | libc::RTLD_NOLOAD) };
    if handle.is_null() {
        G_FAILED.store(true, Ordering::Release);
        return Err(());
    }
    let mut retained: *mut LinkMap = std::ptr::null_mut();
    if unsafe {
        libc::dlinfo(
            handle,
            libc::RTLD_DI_LINKMAP,
            (&mut retained as *mut *mut LinkMap).cast(),
        )
    } != 0
        || retained.is_null()
        || unsafe { (*retained).load_bias } != info.dli_fbase as usize
    {
        // The same path may be loaded in another dlmopen namespace. Retaining
        // the base-namespace copy does not keep that returned pointer alive.
        unsafe {
            libc::dlclose(handle);
        }
        return Ok(false);
    }
    let mut providers = G_PROVIDERS.lock().unwrap_or_else(|e| e.into_inner());
    if providers.contains(&(handle as usize)) {
        drop(providers);
        unsafe {
            libc::dlclose(handle);
        }
    } else {
        providers.push(handle as usize);
    }
    Ok(true)
}

pub fn proxy(handle: *mut c_void, name: &CStr, caller: *const c_void) -> *mut c_void {
    if handle == libc::RTLD_NEXT {
        // This traversal already visits the shim when (and only when) it is
        // after the original caller. Never substitute a wrapper afterward:
        // that could jump backwards into us from a following interceptor.
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
    cuinterpose_abi::boundary(&G_FAILED, std::ptr::null_mut(), || {
        if name.is_null() {
            return std::ptr::null_mut();
        }
        let name = unsafe { CStr::from_ptr(name) };
        let Some(real) = real() else {
            return std::ptr::null_mut();
        };
        let mut address = unsafe { real(libc::RTLD_NEXT, name.as_ptr()) };
        if address.is_null() {
            let handles = G_PROVIDERS
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .clone();
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
                if retain_provider(handle, address) != Ok(true) {
                    address = std::ptr::null_mut();
                }
                unsafe {
                    libc::dlclose(handle);
                }
            }
        } else {
            // A next interceptor need not itself be CUDA. Keep that chain;
            // qualification controls public substitution, not delegation.
            if retain_provider(libc::RTLD_DEFAULT, address).is_err() {
                return std::ptr::null_mut();
            }
        }
        address
    })
}

pub fn core() -> Option<&'static Core> {
    if let Some(pointer) = G_CORE.get() {
        return pointer.map(|address| unsafe { &*(address as *const Core) });
    }
    // Another initializer may be waiting for the loader lock held by this
    // caller's constructor. Never wait, even across threads. A transient
    // NOT_INITIALIZED reply does not poison or publish a failed core.
    if G_INITIALIZING_CORE
        .compare_exchange(false, true, Ordering::Acquire, Ordering::Relaxed)
        .is_err()
    {
        return None;
    }
    struct Initializing;
    impl Drop for Initializing {
        fn drop(&mut self) {
            G_INITIALIZING_CORE.store(false, Ordering::Release);
        }
    }
    let _initializing = Initializing;
    // Only the nonblocking admission winner can initialize this OnceLock.
    let pointer = G_CORE.get_or_init(|| {
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
            origin_pid: super::process::G_ORIGIN_PID.load(Ordering::Acquire),
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
