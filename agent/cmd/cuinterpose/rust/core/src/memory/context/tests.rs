// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::memory::checkpoint::Phase;
use cudarc::driver::sys::*;
use cuinterpose_abi::{ABI_VERSION, FrontendAbi};
use std::ffi::{CStr, c_char, c_void};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering::Relaxed};

static CURRENT: AtomicUsize = AtomicUsize::new(1);
static NEXT_ADDRESS: AtomicUsize = AtomicUsize::new(0x10000);
static HANDLES: AtomicUsize = AtomicUsize::new(0);
static MAPPINGS: AtomicUsize = AtomicUsize::new(0);
static RESERVATIONS: AtomicUsize = AtomicUsize::new(0);
static PRIMARY_REFS: AtomicUsize = AtomicUsize::new(0);
static PRIMARY_ACTIVE: AtomicBool = AtomicBool::new(false);
static LOOKUP_RETAIN: AtomicBool = AtomicBool::new(false);
static FAIL: AtomicBool = AtomicBool::new(false);
static LAST_OPERATION: AtomicUsize = AtomicUsize::new(0);

fn operation(kind: usize) -> CUresult {
    LAST_OPERATION.store(kind, Relaxed);
    let mut state = runtime::get().unwrap();
    assert_eq!(state.unlocked_driver_calls, 1);
    assert_eq!(state.phase, Phase::Active);
    assert!(matches!(
        state.begin_checkpoint(),
        Err(crate::error::Error::Cuda(CUresult::CUDA_ERROR_NOT_READY))
    ));
    if FAIL.load(Relaxed) {
        CUresult::CUDA_ERROR_INVALID_CONTEXT
    } else {
        CUresult::CUDA_SUCCESS
    }
}

unsafe extern "C" fn destroy(_: CUcontext) -> CUresult {
    operation(1)
}

unsafe extern "C" fn destroy_v2(_: CUcontext) -> CUresult {
    operation(2)
}

unsafe extern "C" fn current(out: *mut *mut c_void) -> CUresult {
    unsafe { out.write(CURRENT.load(Relaxed) as *mut c_void) };
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn device(out: *mut CUdevice) -> CUresult {
    unsafe { out.write(0) };
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn retain(out: *mut *mut c_void, _: CUdevice) -> CUresult {
    PRIMARY_REFS.fetch_add(1, Relaxed);
    PRIMARY_ACTIVE.store(true, Relaxed);
    LOOKUP_RETAIN.store(true, Relaxed);
    unsafe { out.write(std::ptr::dangling_mut::<c_void>()) };
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn primary_state(_: CUdevice, flags: *mut u32, active: *mut i32) -> CUresult {
    unsafe {
        flags.write(0);
        active.write(i32::from(PRIMARY_ACTIVE.load(Relaxed)));
    }
    CUresult::CUDA_SUCCESS
}

fn release_primary(kind: usize) -> CUresult {
    if !LOOKUP_RETAIN.swap(false, Relaxed) {
        let result = operation(kind);
        if result != CUresult::CUDA_SUCCESS {
            return result;
        }
    }
    let refs = PRIMARY_REFS.load(Relaxed);
    if refs == 0 {
        return CUresult::CUDA_ERROR_INVALID_CONTEXT;
    }
    PRIMARY_REFS.store(refs - 1, Relaxed);
    if refs == 1 {
        PRIMARY_ACTIVE.store(false, Relaxed);
    }
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn primary_release(_: CUdevice) -> CUresult {
    release_primary(3)
}

unsafe extern "C" fn primary_release_v2(_: CUdevice) -> CUresult {
    release_primary(4)
}

fn reset_primary(kind: usize) -> CUresult {
    let result = operation(kind);
    if result == CUresult::CUDA_SUCCESS {
        PRIMARY_ACTIVE.store(false, Relaxed);
        if kind == 5 {
            PRIMARY_REFS.store(PRIMARY_REFS.load(Relaxed).saturating_sub(1), Relaxed);
        }
    }
    result
}

unsafe extern "C" fn primary_reset(_: CUdevice) -> CUresult {
    reset_primary(5)
}

unsafe extern "C" fn primary_reset_v2(_: CUdevice) -> CUresult {
    reset_primary(6)
}

unsafe extern "C" fn reserve(out: *mut u64, _: usize, _: usize, _: u64, _: u64) -> CUresult {
    RESERVATIONS.fetch_add(1, Relaxed);
    unsafe { out.write(NEXT_ADDRESS.fetch_add(4096, Relaxed) as u64) };
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn map(_: u64, _: usize, _: usize, _: u64, _: u64) -> CUresult {
    MAPPINGS.fetch_add(1, Relaxed);
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn access(_: u64, _: usize, _: *const CUmemAccessDesc, _: usize) -> CUresult {
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn unmap(_: u64, _: usize) -> CUresult {
    MAPPINGS.fetch_sub(1, Relaxed);
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn free_address(_: u64, _: usize) -> CUresult {
    RESERVATIONS.fetch_sub(1, Relaxed);
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn release(_: u64) -> CUresult {
    HANDLES.fetch_sub(1, Relaxed);
    CUresult::CUDA_SUCCESS
}

unsafe extern "C" fn resolve(name: *const c_char) -> *mut c_void {
    match unsafe { CStr::from_ptr(name) }.to_bytes() {
        b"cuCtxDestroy" => destroy as *const () as *mut c_void,
        b"cuCtxDestroy_v2" => destroy_v2 as *const () as *mut c_void,
        b"cuCtxGetCurrent" => current as *const () as *mut c_void,
        b"cuCtxGetDevice" => device as *const () as *mut c_void,
        b"cuDevicePrimaryCtxRetain" => retain as *const () as *mut c_void,
        b"cuDevicePrimaryCtxGetState" => primary_state as *const () as *mut c_void,
        b"cuDevicePrimaryCtxRelease" => primary_release as *const () as *mut c_void,
        b"cuDevicePrimaryCtxRelease_v2" => primary_release_v2 as *const () as *mut c_void,
        b"cuDevicePrimaryCtxReset" => primary_reset as *const () as *mut c_void,
        b"cuDevicePrimaryCtxReset_v2" => primary_reset_v2 as *const () as *mut c_void,
        b"cuMemAddressReserve" => reserve as *const () as *mut c_void,
        b"cuMemMap" => map as *const () as *mut c_void,
        b"cuMemSetAccess" => access as *const () as *mut c_void,
        b"cuMemUnmap" => unmap as *const () as *mut c_void,
        b"cuMemAddressFree" => free_address as *const () as *mut c_void,
        b"cuMemRelease" => release as *const () as *mut c_void,
        _ => crate::tests::unused_driver_symbol(),
    }
}

fn allocation(context: usize) -> u64 {
    let mut state = runtime::active().unwrap();
    let reference = state.new_reference().unwrap();
    let mut properties: CUmemAllocationProp = unsafe { std::mem::zeroed() };
    properties.type_ = CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED;
    properties.location.type_ = CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE;
    properties.requestedHandleTypes =
        CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
    HANDLES.fetch_add(1, Relaxed);
    state
        .adopt_unicast(crate::memory::vmm::Allocation {
            reference,
            refcounts: Default::default(),
            driver: Some(42),
            size: 4096,
            properties,
            shared: false,
            context,
        })
        .unwrap()
}

fn malloc(context: usize, allocation_context: usize, opens: usize) -> u64 {
    let handle = allocation(allocation_context);
    CURRENT.store(context, Relaxed);
    let mut state = runtime::active().unwrap();
    if opens != 0 {
        let id = state.resolve_virtual_handle(handle).unwrap().unwrap();
        state
            .memblocks
            .get_mut(&id)
            .unwrap()
            .unicast_mut()
            .unwrap()
            .reference
            .creator_pid += 1;
    }
    state
        .map_malloc(handle, 100, 4096, opens, context, 0)
        .unwrap()
}

fn counts(expected: (usize, usize, usize)) {
    assert_eq!(
        (
            HANDLES.load(Relaxed),
            MAPPINGS.load(Relaxed),
            RESERVATIONS.load(Relaxed)
        ),
        expected
    );
    assert_eq!(runtime::active().unwrap().unlocked_driver_calls, 0);
}

fn assert_surviving_context(handle: u64, expected: usize) {
    let state = runtime::active().unwrap();
    let id = state.resolve_virtual_handle(handle).unwrap().unwrap();
    let context = match &state.memblocks[&id] {
        Memblock::Unicast(allocation) => allocation.context,
        Memblock::Multicast(object) => object.context,
    };
    assert_eq!(context, expected);
}

#[test]
fn context_lifetimes_reclaim_only_owned_mallocs() {
    // The real ABI/runtime are process-lifetime state; isolate this driver.
    if std::env::var_os("CUINTERPOSE_CONTEXT_UNIT_CHILD").is_none() {
        let directory =
            std::env::temp_dir().join(format!("cuinterpose-context-{}", std::process::id()));
        std::fs::create_dir(&directory).unwrap();
        let status = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "memory::context::tests::context_lifetimes_reclaim_only_owned_mallocs",
            ])
            .env("CUINTERPOSE_CONTEXT_UNIT_CHILD", "1")
            .env("SNAPSHOT_CONTROL_DIR", &directory)
            .status()
            .unwrap();
        std::fs::remove_dir_all(directory).unwrap();
        assert!(status.success());
        return;
    }
    let frontend = FrontendAbi {
        version: ABI_VERSION,
        size: size_of::<FrontendAbi>() as u32,
        resolve,
    };
    let mut backend = std::ptr::null();
    assert_eq!(
        unsafe { crate::cuinterpose_core_init(&frontend, &mut backend) },
        CUresult::CUDA_SUCCESS
    );
    let backend = unsafe { &*backend };
    assert_eq!(
        unsafe { (backend.ensure_cuinterpose_initialized)() },
        CUresult::CUDA_SUCCESS
    );

    for (kind, destroy) in [(1, backend.cuCtxDestroy), (2, backend.cuCtxDestroy_v2)] {
        let owned = malloc(1, 1, 0);
        let other = malloc(2, 2, 0);
        // IPC opens belong to their importing context, independently of the
        // backing's cached context. Repeated opens still own one mapping/handle.
        let imported = malloc(1, 2, 3);
        let direct = allocation(1);
        let multicast = {
            let mut state = runtime::active().unwrap();
            let id = state.new_reference().unwrap().id;
            HANDLES.fetch_add(1, Relaxed);
            state
                .adopt_multicast(id, 43, unsafe { std::mem::zeroed() }, 1)
                .unwrap()
        };
        counts((5, 3, 3));
        FAIL.store(true, Relaxed);
        assert_eq!(
            unsafe { destroy(std::ptr::dangling_mut()) },
            CUresult::CUDA_ERROR_INVALID_CONTEXT
        );
        counts((5, 3, 3));
        assert_surviving_context(direct, 1);
        FAIL.store(false, Relaxed);
        assert_eq!(
            unsafe { destroy(std::ptr::dangling_mut()) },
            CUresult::CUDA_SUCCESS
        );
        assert_eq!(LAST_OPERATION.load(Relaxed), kind);
        counts((3, 1, 1));
        let state = runtime::active().unwrap();
        assert!(!state.malloc_regions.contains_key(&owned));
        assert!(!state.malloc_regions.contains_key(&imported));
        assert!(state.malloc_regions.contains_key(&other));
        drop(state);
        assert_surviving_context(direct, 0);
        assert_surviving_context(multicast, 0);
        assert_eq!(
            unsafe { destroy(2usize as CUcontext) },
            CUresult::CUDA_SUCCESS
        );
        crate::handlers::cuMemRelease(direct).unwrap();
        crate::handlers::cuMemRelease(multicast).unwrap();
        counts((0, 0, 0));
    }

    for (kind, reset, operation) in [
        (3, false, backend.cuDevicePrimaryCtxRelease),
        (4, false, backend.cuDevicePrimaryCtxRelease_v2),
        (5, true, backend.cuDevicePrimaryCtxReset),
        (6, true, backend.cuDevicePrimaryCtxReset_v2),
    ] {
        // Simulate implicit Runtime ownership: no intercepted retain occurred.
        PRIMARY_REFS.store(2, Relaxed);
        PRIMARY_ACTIVE.store(true, Relaxed);
        let owned = malloc(1, 1, 0);
        let other = malloc(2, 2, 0);
        let direct = allocation(1);
        let imported = malloc(1, 2, 3);
        FAIL.store(true, Relaxed);
        assert_eq!(
            unsafe { operation(0) },
            CUresult::CUDA_ERROR_INVALID_CONTEXT
        );
        counts((4, 3, 3));
        assert_eq!(PRIMARY_REFS.load(Relaxed), 2);
        FAIL.store(false, Relaxed);
        assert_eq!(unsafe { operation(0) }, CUresult::CUDA_SUCCESS);
        assert_eq!(LAST_OPERATION.load(Relaxed), kind);
        if !reset {
            counts((4, 3, 3));
            assert_surviving_context(direct, 1);
            assert_eq!(PRIMARY_REFS.load(Relaxed), 1);
            assert_eq!(unsafe { operation(0) }, CUresult::CUDA_SUCCESS);
        }
        counts((2, 1, 1));
        assert_surviving_context(direct, 0);
        let state = runtime::active().unwrap();
        assert!(!state.malloc_regions.contains_key(&owned));
        assert!(!state.malloc_regions.contains_key(&imported));
        assert!(state.malloc_regions.contains_key(&other));
        drop(state);
        assert_eq!(
            PRIMARY_REFS.load(Relaxed),
            match kind {
                5 => 1,
                6 => 2,
                _ => 0,
            }
        );
        assert_eq!(
            unsafe { (backend.cuCtxDestroy_v2)(2usize as CUcontext) },
            CUresult::CUDA_SUCCESS
        );
        crate::handlers::cuMemRelease(direct).unwrap();
        counts((0, 0, 0));
    }

    PRIMARY_REFS.store(0, Relaxed);
    PRIMARY_ACTIVE.store(false, Relaxed);
    assert_eq!(
        unsafe { (backend.cuDevicePrimaryCtxRelease_v2)(0) },
        CUresult::CUDA_ERROR_INVALID_CONTEXT
    );
    assert_eq!(
        PRIMARY_REFS.load(Relaxed),
        0,
        "lookup activated an inactive primary context"
    );
    assert_eq!(
        unsafe { (backend.cuDevicePrimaryCtxReset_v2)(0) },
        CUresult::CUDA_SUCCESS
    );
    counts((0, 0, 0));

    // A snapshot must not remove a new region if CUDA reuses a context and VA.
    let old = malloc(1, 1, 0);
    let resources = ContextResources::capture(&runtime::active().unwrap(), 1);
    runtime::active().unwrap().unmap_malloc(old).unwrap();
    NEXT_ADDRESS.store(old as usize, Relaxed);
    let new = malloc(1, 1, 0);
    assert_eq!(old, new);
    resources.release(&mut runtime::active().unwrap()).unwrap();
    counts((1, 1, 1));
    runtime::active().unwrap().unmap_malloc(new).unwrap();
    counts((0, 0, 0));
}
