// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Context teardown releases that context's converted mallocs but not direct VMM.

use cudarc::runtime::sys as runtime;
use cuinterpose_gpu_tests::process::{pid, run};
use cuinterpose_gpu_tests::sys::{self, CUdeviceptr, CUresult};
use cuinterpose_gpu_tests::{cu, cuda, shim};
use std::ptr::null_mut;

const MALLOC_SIZE: usize = 64 << 20;

#[derive(Clone, Copy)]
enum Teardown {
    /// cuCtxDestroy of a created context.
    Destroy,
    /// cuDevicePrimaryCtxReset of the retained primary context.
    Reset,
    /// The final cuDevicePrimaryCtxRelease, after a nonfinal one.
    Release,
    /// cudaDeviceReset of the primary context the Runtime API started.
    Runtime,
}

#[test]
fn ctx_destroy_frees_its_mallocs_but_keeps_vmm_and_other_contexts() {
    teardown_frees_only_its_mallocs(Teardown::Destroy);
}

#[test]
fn primary_reset_frees_its_mallocs_but_keeps_vmm_and_other_contexts() {
    teardown_frees_only_its_mallocs(Teardown::Reset);
}

#[test]
fn final_primary_release_frees_its_mallocs_but_keeps_vmm_and_other_contexts() {
    teardown_frees_only_its_mallocs(Teardown::Release);
}

#[test]
fn cuda_device_reset_frees_its_mallocs_but_keeps_vmm_and_other_contexts() {
    teardown_frees_only_its_mallocs(Teardown::Runtime);
}

fn teardown_frees_only_its_mallocs(teardown: Teardown) {
    run(move || {
        cu!(cuInit(0));
        let mut device = 0;
        cu!(cuDeviceGet(&mut device, 0));
        let observer = cuda::create_context(device);
        let survivor = cuda::malloc(4096);
        cuda::write_bytes(survivor, b"other context");
        let created = matches!(teardown, Teardown::Destroy).then(|| cuda::create_context(device));
        match teardown {
            Teardown::Destroy => {}
            Teardown::Runtime => {
                // Initialize through the Runtime API without an application call to retain
                // the primary context.
                cu!(cuCtxSetCurrent(null_mut()));
                check_runtime(unsafe { runtime::cudaSetDevice(0) }, "cudaSetDevice");
            }
            Teardown::Reset | Teardown::Release => {
                cuda::make_primary_current(device);
            }
        }
        let converted = match teardown {
            Teardown::Runtime => {
                let mut pointer = null_mut();
                let status = unsafe { runtime::cudaMalloc(&mut pointer, MALLOC_SIZE) };
                check_runtime(status, "cudaMalloc");
                pointer as CUdeviceptr
            }
            _ => cuda::malloc(MALLOC_SIZE),
        };
        let handle = cuda::retain(converted);
        cuda::assert_handle_namespace(handle, true, "converted malloc");
        cu!(cuMemRelease(handle));
        cuda::write_bytes(converted, b"converted malloc");
        let properties = cuda::allocation_properties(0, false);
        let (handle, address, size) = cuda::create_mapped(&properties, None);
        cuda::assert_handle_namespace(handle, true, "direct VMM");
        cuda::write_bytes(address, b"direct VMM");
        cuda::mark_shared(handle);

        match teardown {
            Teardown::Destroy => cu!(cuCtxDestroy_v2(created.unwrap())),
            Teardown::Reset => cu!(cuDevicePrimaryCtxReset_v2(device)),
            Teardown::Release => {
                // A nonfinal release leaves the context and its mallocs alive.
                let mut primary = null_mut();
                cu!(cuDevicePrimaryCtxRetain(&mut primary, device));
                cu!(cuDevicePrimaryCtxRelease_v2(device));
                assert_eq!(cuda::address_range(converted), (converted, MALLOC_SIZE));
                cuda::assert_bytes(converted, b"converted malloc", "nonfinal release");
                cu!(cuDevicePrimaryCtxRelease_v2(device));
            }
            Teardown::Runtime => {
                check_runtime(unsafe { runtime::cudaDeviceReset() }, "cudaDeviceReset");
            }
        }

        cu!(cuCtxSetCurrent(observer));
        // The shim answers this lookup from its malloc records, so a stale record would
        // succeed.
        let (mut base, mut extent) = (0, 0);
        let status = unsafe { sys::cuMemGetAddressRange_v2(&mut base, &mut extent, converted) };
        assert!(
            matches!(
                status,
                CUresult::CUDA_ERROR_INVALID_VALUE | CUresult::CUDA_ERROR_NOT_FOUND
            ),
            "{status:?}"
        );
        let mut retained = 0;
        let status =
            unsafe { sys::cuMemRetainAllocationHandle(&mut retained, converted as *mut _) };
        assert_ne!(
            status,
            CUresult::CUDA_SUCCESS,
            "malloc mapping survived teardown"
        );
        cuda::assert_bytes(survivor, b"other context", "other live context");
        cuda::assert_bytes(address, b"direct VMM", "after teardown");
        // The allocation's recorded context is gone, so the carrier copies must fall back
        // to the primary context.
        shim::checkpoint(&[pid()], false);
        cuda::assert_bytes(address, b"direct VMM", "after reconstruction");
        // Unmapping and releasing check the restored mapping and handle.
        cuda::unmap(address, size);
        cu!(cuMemRelease(handle));
        if let Teardown::Reset = teardown {
            // Reset keeps the application's retain, so the shim's internal retains must not
            // consume it.
            cu!(cuDevicePrimaryCtxRelease_v2(device));
        }
    });
}

fn check_runtime(status: runtime::cudaError_t, call: &str) {
    assert_eq!(status, runtime::cudaError_t::cudaSuccess, "{call} failed");
}
