// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Core ABI registration and startup, driven by a test frontend instead of the preload.

use cuinterpose_abi::{ABI_VERSION, BackendAbi, FrontendAbi};
use cuinterpose_gpu_tests::core_abi::{self, driver_symbol, initialize, resolve_driver};
use cuinterpose_gpu_tests::process::{Options, endpoint, run_with};
use cuinterpose_gpu_tests::sys::{self, CUresult};
use cuinterpose_gpu_tests::{cuda, tracked};
use std::ffi::{CStr, c_char, c_void};
use std::ptr::{null, null_mut};
use std::sync::OnceLock;

const SUCCESS: CUresult = CUresult::CUDA_SUCCESS;
const INVALID: CUresult = CUresult::CUDA_ERROR_INVALID_VALUE;
const NOT_INITIALIZED: CUresult = CUresult::CUDA_ERROR_NOT_INITIALIZED;

/// The resolver hides driver symbols that start with this prefix.
static HIDDEN: OnceLock<&str> = OnceLock::new();

unsafe extern "C" fn resolve_hiding(name: *const c_char) -> *mut c_void {
    let name = unsafe { CStr::from_ptr(name) };
    match HIDDEN.get() {
        Some(prefix) if name.to_bytes().starts_with(prefix.as_bytes()) => null_mut(),
        _ => driver_symbol(name),
    }
}

unsafe extern "C" fn resolve_nothing(_name: *const c_char) -> *mut c_void {
    null_mut()
}

#[test]
fn core_init_rejects_bad_abi_header_and_a_second_frontend() {
    run_with(Options::unshimmed(), || {
        let frontend = core_abi::frontend(resolve_driver);
        let mut output = null();
        assert_eq!(unsafe { initialize()(null(), &mut output) }, INVALID);
        assert!(output.is_null());
        assert_eq!(unsafe { initialize()(&frontend, null_mut()) }, INVALID);
        // An invalid prefix must be rejected before reading the rest of the ABI, so
        // place it directly before an inaccessible page.
        let prefix = before_inaccessible_page();
        for header in [
            [ABI_VERSION + 1, size_of::<FrontendAbi>() as u32],
            [ABI_VERSION, size_of::<[u32; 2]>() as u32],
        ] {
            unsafe { prefix.write(header) };
            let status = unsafe { initialize()(prefix.cast(), &mut output) };
            assert_eq!(status, INVALID, "header {header:?}");
        }
        assert!(output.is_null());
        assert_eq!(unsafe { initialize()(&frontend, &mut output) }, SUCCESS);
        let backend = unsafe { output.as_ref() }.expect("no backend table");
        assert_eq!(backend.version, ABI_VERSION);
        assert_eq!(backend.size as usize, size_of::<BackendAbi>());
        let mut again = null();
        assert_eq!(unsafe { initialize()(&frontend, &mut again) }, SUCCESS);
        assert_eq!(again, output);
        let other = core_abi::frontend(resolve_nothing);
        again = null();
        assert_eq!(unsafe { initialize()(&other, &mut again) }, INVALID);
        assert!(again.is_null());
        assert_eq!(unsafe { initialize()(&frontend, &mut again) }, SUCCESS);
        assert_eq!(again, output);
    });
}

#[test]
fn startup_fails_and_names_missing_required_driver_symbol() {
    let stderr = run_with(Options::unshimmed(), || {
        let (_, status) = start_hiding("cuMemCreate");
        assert_eq!(status, NOT_INITIALIZED);
        assert!(!endpoint().exists());
    });
    assert!(
        stderr.contains("missing required CUDA symbol cuMemCreate"),
        "{stderr}"
    );
}

#[test]
fn missing_multicast_symbols_fail_only_multicast_calls() {
    run_with(Options::unshimmed(), || {
        // The runtime starts without optional symbols and fails only the calls that
        // need them.
        let (backend, status) = start_hiding("cuMulticast");
        assert_eq!(status, SUCCESS);
        assert!(endpoint().exists());
        cuda::primary_contexts(1);
        // Only POSIX descriptor groups reach the driver call. Other handle types are
        // unsupported before that.
        let properties = sys::CUmulticastObjectProp {
            numDevices: 1,
            size: 2 << 20,
            handleTypes: u64::from(cuda::POSIX_FD.0),
            flags: 0,
        };
        let mut group = 0;
        let status = unsafe { (backend.cuMulticastCreate)(&mut group, &properties) };
        assert_eq!(status, NOT_INITIALIZED);
        // Tracked allocation and context teardown still work.
        let mut allocation = 0;
        tracked!(backend.cuMemAlloc_v2(&mut allocation, 4096));
        tracked!(backend.cuMemFree_v2(allocation));
        tracked!(backend.cuDevicePrimaryCtxRelease_v2(0));
    });
}

/// Starts the core with a frontend that hides driver symbols starting with `prefix`.
fn start_hiding(prefix: &'static str) -> (&'static BackendAbi, CUresult) {
    HIDDEN.set(prefix).unwrap();
    core_abi::start(resolve_hiding)
}

/// Eight writable bytes that end where a PROT_NONE page starts.
fn before_inaccessible_page() -> *mut [u32; 2] {
    let page = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
    assert!(page > 0);
    let page = page as usize;
    let pages = unsafe {
        libc::mmap(
            null_mut(),
            2 * page,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
            -1,
            0,
        )
    };
    assert_ne!(pages, libc::MAP_FAILED);
    let guard = unsafe { pages.byte_add(page) };
    assert_eq!(unsafe { libc::mprotect(guard, page, libc::PROT_NONE) }, 0);
    unsafe { guard.byte_sub(size_of::<[u32; 2]>()) }.cast()
}
