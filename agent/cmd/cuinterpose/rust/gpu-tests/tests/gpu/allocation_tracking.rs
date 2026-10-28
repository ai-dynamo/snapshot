// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Tracked VMM handles, converted mallocs, and rejected allocation calls.

use cuinterpose_gpu_tests::cuda::{self, POSIX_FD};
use cuinterpose_gpu_tests::process::{pid, run};
use cuinterpose_gpu_tests::sys::{
    self, CUdevice_attribute, CUmemAllocationHandleType, CUmemAllocationType, CUresult,
};
use cuinterpose_gpu_tests::{cu, shim};

#[test]
fn mapped_backing_outlives_released_handles_and_can_be_retained_and_reimported() {
    run(|| {
        cuda::primary_contexts(1);
        let properties = cuda::allocation_properties(0, false);
        let size = cuda::granularity(&properties);
        let mut handle = 0;
        cu!(cuMemCreate(&mut handle, size, &properties, 0));
        cuda::assert_handle_namespace(handle, true, "created backing");
        let mut addresses: Vec<_> = (0..2)
            .map(|_| cuda::map(handle, size, cuda::device(0)))
            .collect();
        cuda::write_bytes(addresses[0], b"shared allocation");
        let retained = cuda::retain(addresses[0]);
        cu!(cuMemRelease(handle));
        cu!(cuMemRelease(retained));
        // Mappings keep the backing after every application handle is released.
        for &address in &addresses {
            cuda::assert_bytes(address, b"shared allocation", "released handles");
        }
        let retained = cuda::retain(addresses[0]);
        let imported = cuda::import_fd(&cuda::export_fd(retained));
        cuda::assert_handle_namespace(imported, true, "imported alias");
        addresses.push(cuda::map(imported, size, cuda::device(0)));
        cu!(cuMemRelease(imported));
        cu!(cuMemRelease(retained));
        cuda::write_bytes(addresses[2], b"updated allocation");
        for &address in &addresses {
            cuda::assert_bytes(
                address,
                b"updated allocation",
                "retained and imported aliases",
            );
            cuda::unmap(address, size);
        }
    });
}

#[test]
fn allocation_created_without_context_maps_and_restores() {
    run(|| {
        cu!(cuInit(0));
        let mut current = std::ptr::null_mut();
        cu!(cuCtxGetCurrent(&mut current));
        assert!(current.is_null());
        // The driver permits cuMemCreate without a context, so the shim must as well.
        let properties = cuda::allocation_properties(0, false);
        let size = cuda::granularity(&properties);
        let mut handle = 0;
        cu!(cuMemCreate(&mut handle, size, &properties, 0));
        cuda::assert_handle_namespace(handle, true, "contextless create");
        cuda::primary_contexts(1);
        let address = cuda::map(handle, size, cuda::device(0));
        cuda::write_bytes(address, b"contextless create");
        cuda::mark_shared(handle);
        shim::checkpoint(&[pid()], false);
        cuda::assert_bytes(address, b"contextless create", "after reconstruction");
    });
}

#[test]
fn cumemalloc_returns_tracked_vmm_with_native_bounds_and_rdma_flag() {
    run(|| {
        let contexts = cuda::primary_contexts(2);
        // Returning to GPU 0 checks that per-device malloc layouts are cached correctly.
        for device in [0, 1, 0] {
            cu!(cuCtxSetCurrent(contexts[device as usize]));
            let rdma = [
                CUdevice_attribute::CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_SUPPORTED,
                CUdevice_attribute::CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_WITH_CUDA_VMM_SUPPORTED,
            ]
            .into_iter()
            .all(|attribute| cuda::device_attribute(attribute, device) != 0);
            for size in [1, 65537, (2 << 20) + 1] {
                let address = cuda::malloc(size);
                assert_eq!(
                    cuda::address_range(address + size as u64 - 1),
                    (address, size)
                );
                assert_eq!(shim::mapped_location(pid(), address).id, device);
                cuda::assert_malloc_semantics(address, false, "converted malloc");
                cuda::assert_malloc_semantics(
                    address + size as u64 - 1,
                    false,
                    "converted malloc end",
                );
                let mut capable = 0u32;
                let attribute =
                    sys::CUpointer_attribute::CU_POINTER_ATTRIBUTE_IS_GPU_DIRECT_RDMA_CAPABLE;
                cu!(cuPointerGetAttribute(
                    (&raw mut capable).cast(),
                    attribute,
                    address
                ));
                assert_eq!(capable != 0, rdma);
                let last = address + size as u64 - 1;
                cuda::write_bytes(last, b"x");
                cuda::assert_bytes(last, b"x", "malloc bounds");
                cu!(cuMemFree_v2(address));
            }
            // Pitched allocations stay native, so this exercises the range fallback.
            let (mut address, mut pitch) = (0, 0);
            cu!(cuMemAllocPitch_v2(&mut address, &mut pitch, 16, 4, 4));
            let (base, extent) = cuda::address_range(address + pitch as u64 * 4 - 1);
            assert!(base == address && extent >= pitch * 4);
            cu!(cuMemFree_v2(address));
        }
    });
}

#[test]
fn rejected_create_or_export_leaves_output_and_records_unchanged() {
    run(|| {
        cuda::primary_contexts(1);
        let fabric = CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_FABRIC;
        let valid = cuda::allocation_properties(0, false);
        let granule = cuda::granularity(&valid);
        let mut invalid_type = valid;
        invalid_type.type_ = CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_INVALID;
        let mut fabric_create = valid;
        fabric_create.requestedHandleTypes = fabric;
        for (case, properties, size, expected) in [
            ("zero size", valid, 0, CUresult::CUDA_ERROR_INVALID_VALUE),
            (
                "invalid type",
                invalid_type,
                granule,
                CUresult::CUDA_ERROR_NOT_SUPPORTED,
            ),
            (
                "fabric create",
                fabric_create,
                granule,
                CUresult::CUDA_ERROR_NOT_SUPPORTED,
            ),
        ] {
            let before = shim::records(pid());
            let mut output = 99;
            let status = unsafe { sys::cuMemCreate(&mut output, size, &properties, 0) };
            assert_eq!(status, expected, "{case}");
            assert_eq!(
                output, 99,
                "{case}: failed creation changed the caller's output"
            );
            assert_eq!(shim::records(pid()), before, "{case}");
        }
        let mut handle = 0;
        cu!(cuMemCreate(&mut handle, granule, &valid, 0));
        for (case, kind, flags) in [("fabric export", fabric, 0), ("nonzero flags", POSIX_FD, 1)] {
            let before = shim::records(pid());
            let mut output: i32 = -99;
            let status = unsafe {
                sys::cuMemExportToShareableHandle((&raw mut output).cast(), handle, kind, flags)
            };
            assert_eq!(status, CUresult::CUDA_ERROR_INVALID_VALUE, "{case}");
            assert_eq!(
                output, -99,
                "{case}: failed export changed the caller's output"
            );
            // A rejected export must not mark the allocation shared.
            assert_eq!(shim::records(pid()), before, "{case}");
        }
        // A valid export still succeeds afterwards.
        cuda::mark_shared(handle);
    });
}
