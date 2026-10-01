// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use cuinterpose_abi::{ABI_VERSION, BackendAbi, FrontendAbi};
use std::ffi::{CStr, c_char};
use std::os::fd::AsRawFd;
use std::sync::atomic::{AtomicUsize, Ordering};

static CREATE_FAILS: AtomicUsize = AtomicUsize::new(0);
static CREATES: AtomicUsize = AtomicUsize::new(0);
static IMPORTS: AtomicUsize = AtomicUsize::new(0);
static LIVE_HANDLES: AtomicUsize = AtomicUsize::new(0);
static DEVICE: AtomicUsize = AtomicUsize::new(0);
static GRANULARITY_QUERIES: AtomicUsize = AtomicUsize::new(0);
static ATTRIBUTE_QUERIES: AtomicUsize = AtomicUsize::new(0);
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
    if CREATE_FAILS.load(Ordering::Relaxed) != 0 {
        return CUDA_ERROR_OUT_OF_MEMORY;
    }
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
        2 => CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST,
        _ => return CUDA_ERROR_INVALID_VALUE,
    };
    unsafe {
        output.write(properties(
            location,
            CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
        ));
    }
    CUDA_SUCCESS
}

unsafe extern "C" fn device(output: *mut i32) -> CUresult {
    unsafe { output.write(DEVICE.load(Ordering::Relaxed) as i32) };
    CUDA_SUCCESS
}

unsafe extern "C" fn attribute(
    output: *mut i32,
    attribute: CUdevice_attribute,
    device: i32,
) -> CUresult {
    ATTRIBUTE_QUERIES.fetch_add(1, Ordering::Relaxed);
    let value = i32::from(
        device == 0
            || attribute == CUdevice_attribute::CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_SUPPORTED,
    );
    unsafe { output.write(value) };
    CUDA_SUCCESS
}

unsafe extern "C" fn granularity(
    output: *mut usize,
    _: *const CUmemAllocationProp,
    _: CUmemAllocationGranularity_flags,
) -> CUresult {
    GRANULARITY_QUERIES.fetch_add(1, Ordering::Relaxed);
    unsafe { output.write(4096) };
    CUDA_SUCCESS
}

unsafe extern "C" fn map(_: u64, _: usize, _: usize, _: u64, _: u64) -> CUresult {
    CUDA_SUCCESS
}

unsafe extern "C" fn unmap(_: u64, _: usize) -> CUresult {
    CUDA_SUCCESS
}

unsafe extern "C" fn retain(output: *mut u64, _: *mut c_void) -> CUresult {
    LIVE_HANDLES.fetch_add(1, Ordering::Relaxed);
    unsafe { output.write(42) };
    CUDA_SUCCESS
}

unsafe extern "C" fn resolve(name: *const c_char) -> *mut c_void {
    match unsafe { CStr::from_ptr(name) }.to_bytes() {
        b"cuCtxGetCurrent" => current as *const () as *mut c_void,
        b"cuCtxGetDevice" => device as *const () as *mut c_void,
        b"cuDeviceGetAttribute" => attribute as *const () as *mut c_void,
        b"cuMemGetAllocationGranularity" => granularity as *const () as *mut c_void,
        b"cuMemMap" => map as *const () as *mut c_void,
        b"cuMemUnmap" => unmap as *const () as *mut c_void,
        b"cuMemRetainAllocationHandle" => retain as *const () as *mut c_void,
        b"cuMemCreate" => create as *const () as *mut c_void,
        b"cuMemRelease" => release as *const () as *mut c_void,
        b"cuMemImportFromShareableHandle" => import as *const () as *mut c_void,
        b"cuMemGetAllocationPropertiesFromHandle" => get_properties as *const () as *mut c_void,
        _ => crate::tests::unused_driver_symbol(),
    }
}

fn backend() -> &'static BackendAbi {
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
    backend
}

#[test]
fn unicast_create_rejects_unsupported_backing() {
    crate::tests::in_child_process(
        "handlers::tests::unicast_create_rejects_unsupported_backing",
        || {
            let backend = backend();
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
        },
    );
}

#[test]
fn unicast_create_release_preserves_handle_ownership() {
    crate::tests::in_child_process(
        "handlers::tests::unicast_create_release_preserves_handle_ownership",
        || {
            let backend = backend();
            let kinds = [
                CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_NONE,
                CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
            ];
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
        },
    );
}

#[test]
fn unicast_import_releases_unpublished_handles_on_failure() {
    crate::tests::in_child_process(
        "handlers::tests::unicast_import_releases_unpublished_handles_on_failure",
        || {
            let backend = backend();
            for location in 0..4 {
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
                        if location == 2 {
                            CUDA_ERROR_NOT_SUPPORTED
                        } else {
                            CUDA_ERROR_INVALID_VALUE
                        }
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
            assert_eq!(IMPORTS.load(Ordering::Relaxed), 4);
        },
    );
}

#[test]
fn unicast_failed_create_leaves_output_untouched() {
    crate::tests::in_child_process(
        "handlers::tests::unicast_failed_create_leaves_output_untouched",
        || {
            let backend = backend();
            CREATE_FAILS.store(1, Ordering::Relaxed);
            let properties = properties(
                CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
            );
            let mut handle = 99;
            assert_eq!(
                unsafe { (backend.cuMemCreate)(&mut handle, 4096, &properties, 0) },
                CUDA_ERROR_OUT_OF_MEMORY
            );
            assert_eq!(handle, 99);
            assert!(active().unwrap().memblocks.is_empty());
        },
    );
}

#[test]
fn backing_survives_aliases_and_mapping_retains() {
    crate::tests::in_child_process(
        "handlers::tests::backing_survives_aliases_and_mapping_retains",
        || {
            let backend = backend();
            let properties = properties(
                CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
            );
            let mut handle = 0;
            assert_eq!(
                unsafe { (backend.cuMemCreate)(&mut handle, 4096, &properties, 0) },
                CUDA_SUCCESS
            );
            assert_eq!(
                unsafe { (backend.cuMemMap)(0x10000, 4096, 0, handle, 0) },
                CUDA_SUCCESS
            );
            let mut retained = 0;
            assert_eq!(
                unsafe {
                    (backend.cuMemRetainAllocationHandle)(
                        &mut retained,
                        0x10000usize as *mut c_void,
                    )
                },
                CUDA_SUCCESS
            );
            assert_eq!(retained, handle);
            let reference = {
                let state = active().unwrap();
                let id = state.resolve_virtual_handle(handle).unwrap().unwrap();
                state.memblocks[&id].reference()
            };
            let ticket = sharing::create(reference).unwrap();
            let mut alias = 0;
            assert_eq!(
                unsafe {
                    (backend.cuMemImportFromShareableHandle)(
                        &mut alias,
                        ticket.as_raw_fd() as usize as *mut c_void,
                        CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
                    )
                },
                CUDA_SUCCESS
            );
            assert_ne!(alias, handle);
            for released in [alias, handle] {
                assert_eq!(unsafe { (backend.cuMemRelease)(released) }, CUDA_SUCCESS);
                assert_eq!(LIVE_HANDLES.load(Ordering::Relaxed), 1);
            }
            assert_eq!(unsafe { (backend.cuMemRelease)(handle) }, CUDA_SUCCESS);
            assert_eq!(LIVE_HANDLES.load(Ordering::Relaxed), 0);
            assert_eq!(
                unsafe {
                    (backend.cuMemRetainAllocationHandle)(
                        &mut retained,
                        0x10000usize as *mut c_void,
                    )
                },
                CUDA_SUCCESS
            );
            assert_eq!(
                retained, handle,
                "a surviving mapping restores its original handle"
            );
            assert_eq!(LIVE_HANDLES.load(Ordering::Relaxed), 1);
            assert_eq!(unsafe { (backend.cuMemRelease)(retained) }, CUDA_SUCCESS);
            assert_eq!(unsafe { (backend.cuMemUnmap)(0x10000, 4096) }, CUDA_SUCCESS);
            assert_eq!(LIVE_HANDLES.load(Ordering::Relaxed), 0);
            let state = active().unwrap();
            assert!(state.memblocks.is_empty());
            assert!(state.virtual_allocation_handles.is_empty());
        },
    );
}

#[test]
fn malloc_layout_caches_granularity_and_rdma_capability_per_device() {
    crate::tests::in_child_process(
        "handlers::tests::malloc_layout_caches_granularity_and_rdma_capability_per_device",
        || {
            backend();
            let mut state = active().unwrap();
            for device in 0..2 {
                DEVICE.store(device, Ordering::Relaxed);
                for size in [1, 4097] {
                    let (properties, extent) = state.allocation_layout(size).unwrap();
                    assert_eq!(
                        properties.allocFlags.gpuDirectRDMACapable,
                        u8::from(device == 0)
                    );
                    assert_eq!(extent, size.next_multiple_of(4096));
                }
            }
            assert_eq!(GRANULARITY_QUERIES.load(Ordering::Relaxed), 2);
            assert_eq!(ATTRIBUTE_QUERIES.load(Ordering::Relaxed), 4);
        },
    );
}
