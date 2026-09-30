// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use cuinterpose_abi::{ABI_VERSION, FrontendAbi};
use std::ffi::{CStr, c_char};
use std::os::fd::AsRawFd;
use std::sync::atomic::{AtomicUsize, Ordering};

static CREATES: AtomicUsize = AtomicUsize::new(0);
static IMPORTS: AtomicUsize = AtomicUsize::new(0);
static LIVE_HANDLES: AtomicUsize = AtomicUsize::new(0);
static IMPORT_LOCATION: AtomicUsize = AtomicUsize::new(0);

fn properties(location: CUmemLocationType, kind: CUmemAllocationHandleType) -> CUmemAllocationProp {
    CUmemAllocationProp {
        type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
        requestedHandleTypes: kind,
        location: CUmemLocation {
            type_: location,
            id: 0,
        },
        ..unsafe { std::mem::zeroed() }
    }
}

unsafe extern "C" fn current(output: *mut *mut c_void) -> CUresult {
    unsafe { output.write(std::ptr::dangling_mut::<c_void>()) };
    CUDA_SUCCESS
}

unsafe extern "C" fn create(
    output: *mut u64,
    _: usize,
    _: *const CUmemAllocationProp,
    _: u64,
) -> CUresult {
    CREATES.fetch_add(1, Ordering::Relaxed);
    LIVE_HANDLES.fetch_add(1, Ordering::Relaxed);
    unsafe { output.write(42) };
    CUDA_SUCCESS
}

unsafe extern "C" fn import(
    output: *mut u64,
    _: *mut c_void,
    _: CUmemAllocationHandleType,
) -> CUresult {
    IMPORTS.fetch_add(1, Ordering::Relaxed);
    LIVE_HANDLES.fetch_add(1, Ordering::Relaxed);
    unsafe { output.write(42) };
    CUDA_SUCCESS
}

unsafe extern "C" fn release(_: u64) -> CUresult {
    LIVE_HANDLES.fetch_sub(1, Ordering::Relaxed);
    CUDA_SUCCESS
}

unsafe extern "C" fn get_properties(output: *mut CUmemAllocationProp, _: u64) -> CUresult {
    let location = match IMPORT_LOCATION.load(Ordering::Relaxed) {
        0 => CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
        1 => CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA,
        _ => CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST,
    };
    unsafe {
        output.write(properties(
            location,
            CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
        ));
    }
    CUDA_SUCCESS
}

unsafe extern "C" fn resolve(name: *const c_char) -> *mut c_void {
    match unsafe { CStr::from_ptr(name) }.to_bytes() {
        b"cuCtxGetCurrent" => current as *const () as *mut c_void,
        b"cuMemCreate" => create as *const () as *mut c_void,
        b"cuMemRelease" => release as *const () as *mut c_void,
        b"cuMemImportFromShareableHandle" => import as *const () as *mut c_void,
        b"cuMemGetAllocationPropertiesFromHandle" => get_properties as *const () as *mut c_void,
        _ => std::ptr::null_mut(),
    }
}

#[test]
fn unicast_admission_accepts_host_numa_and_rejects_unsupported_backing_without_leaks() {
    // ABI registration and runtime startup are process-lifetime state. Exercise
    // the actual backend table in a child, isolated from other fake resolvers.
    if std::env::var_os("CUINTERPOSE_ADMISSION_UNIT_CHILD").is_none() {
        let directory =
            std::env::temp_dir().join(format!("cuinterpose-admission-{}", std::process::id()));
        std::fs::create_dir(&directory).unwrap();
        let status = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "handlers::tests::unicast_admission_accepts_host_numa_and_rejects_unsupported_backing_without_leaks",
            ])
            .env("CUINTERPOSE_ADMISSION_UNIT_CHILD", "1")
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
        CUDA_SUCCESS
    );
    let backend = unsafe { &*backend };
    assert_eq!(
        unsafe { (backend.ensure_cuinterpose_initialized)() },
        CUDA_SUCCESS
    );
    let kinds = [
        CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_NONE,
        CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
    ];
    let mut invalid_type = properties(
        CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA,
        CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
    );
    invalid_type.type_ = CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_INVALID;
    for properties in [
        properties(
            CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST,
            CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
        ),
        invalid_type,
    ] {
        let mut handle = 99;
        assert_eq!(
            unsafe { (backend.cuMemCreate)(&mut handle, 4096, &properties, 0) },
            CUDA_ERROR_NOT_SUPPORTED
        );
        assert_eq!(handle, 99, "rejection changed the caller's output");
        assert_eq!(CREATES.load(Ordering::Relaxed), 0);
        assert_eq!(LIVE_HANDLES.load(Ordering::Relaxed), 0);
        assert!(active().unwrap().memblocks.is_empty());
        assert!(active().unwrap().virtual_allocation_handles.is_empty());
    }
    for location in [
        CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
        CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA,
    ] {
        for kind in kinds {
            let properties = properties(location, kind);
            let mut handle = 0;
            assert_eq!(
                unsafe { (backend.cuMemCreate)(&mut handle, 4096, &properties, 0) },
                CUDA_SUCCESS
            );
            assert_eq!(
                VirtualAllocationHandle::from_raw(handle).is_some(),
                kind == CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
            );
            assert_eq!(LIVE_HANDLES.load(Ordering::Relaxed), 1);
            assert_eq!(unsafe { (backend.cuMemRelease)(handle) }, CUDA_SUCCESS);
            assert_eq!(LIVE_HANDLES.load(Ordering::Relaxed), 0);
            assert!(active().unwrap().memblocks.is_empty());
            assert!(active().unwrap().virtual_allocation_handles.is_empty());
        }
    }
    assert_eq!(CREATES.load(Ordering::Relaxed), 4);
    for location in 0..3 {
        IMPORT_LOCATION.store(location, Ordering::Relaxed);
        let reference = active().unwrap().new_reference().unwrap();
        // Serve a foreign-version peer's descriptor through the real transport;
        // the fake driver supplies the allocation properties after import.
        runtime::export_cache()
            .unwrap()
            .insert(
                reference.id,
                std::fs::File::open("/dev/zero").unwrap().into(),
                None,
            )
            .unwrap();
        let fd = sharing::create(reference).unwrap();
        let mut handle = 99;
        assert_eq!(
            unsafe {
                (backend.cuMemImportFromShareableHandle)(
                    &mut handle,
                    fd.as_raw_fd() as usize as *mut c_void,
                    CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
                )
            },
            if location < 2 {
                CUDA_SUCCESS
            } else {
                CUDA_ERROR_NOT_SUPPORTED
            }
        );
        if location < 2 {
            assert!(VirtualAllocationHandle::from_raw(handle).is_some());
            assert_eq!(LIVE_HANDLES.load(Ordering::Relaxed), 1);
            assert_eq!(unsafe { (backend.cuMemRelease)(handle) }, CUDA_SUCCESS);
        } else {
            assert_eq!(handle, 99);
        }
        assert_eq!(LIVE_HANDLES.load(Ordering::Relaxed), 0);
        assert!(active().unwrap().memblocks.is_empty());
        assert!(active().unwrap().virtual_allocation_handles.is_empty());
        runtime::export_cache()
            .unwrap()
            .remove(&reference.id)
            .unwrap();
    }
    assert_eq!(IMPORTS.load(Ordering::Relaxed), 3);
}
