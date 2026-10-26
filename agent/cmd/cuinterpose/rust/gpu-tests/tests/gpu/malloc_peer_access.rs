// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Peer access grants on converted mallocs through enable, failure, disable, and teardown.

use cudarc::runtime::sys::{self as runtime, cudaError};
use cuinterpose_gpu_tests::process::{Options, run, run_with};
use cuinterpose_gpu_tests::sys::{self, CUcontext, CUdeviceptr, CUresult};
use cuinterpose_gpu_tests::{cu, cuda, fixtures, loader, shim};

fn zero(address: CUdeviceptr) {
    cuda::write_bytes(address, &0u32.to_le_bytes());
}

/// Initializes CUDA and creates a context on each GPU.
fn created_contexts() -> [CUcontext; 2] {
    cu!(cuInit(0));
    [0, 1].map(cuda::create_context)
}

#[test]
fn cudart_peer_enable_grants_access_to_existing_mallocs() {
    run(|| {
        cuda::primary_contexts(1);
        let address = cuda::malloc(4);
        zero(address);
        // PyTorch enables peer access through cudart, which must dispatch into the shim.
        assert_eq!(unsafe { runtime::cudaSetDevice(1) }, cudaError::cudaSuccess);
        assert_eq!(
            unsafe { runtime::cudaDeviceEnablePeerAccess(0, 0) },
            cudaError::cudaSuccess
        );
        let mut device = 0;
        cu!(cuCtxGetDevice(&mut device));
        assert_eq!(device, 1, "the increment would not run on GPU 1");
        cuda::increment(address);
        assert_eq!(cuda::read_u32(address), 1, "runtime peer access");
    });
}

#[test]
fn mallocs_after_failed_peer_enable_get_no_peer_access() {
    run(|| {
        let contexts = created_contexts();
        cu!(cuCtxSetCurrent(contexts[1]));
        let status = unsafe { sys::cuCtxEnablePeerAccess(contexts[0], 1) };
        assert_eq!(status, CUresult::CUDA_ERROR_INVALID_VALUE);
        cu!(cuCtxSetCurrent(contexts[0]));
        let address = cuda::malloc(4);
        assert_eq!(cuda::get_access(address, 1), cuda::NONE);
    });
}

#[test]
fn mallocs_after_disabled_peer_enable_get_no_peer_access() {
    run(|| {
        let contexts = created_contexts();
        cu!(cuCtxSetCurrent(contexts[1]));
        cu!(cuCtxEnablePeerAccess(contexts[0], 0));
        cu!(cuCtxDisablePeerAccess(contexts[0]));
        cu!(cuCtxSetCurrent(contexts[0]));
        let address = cuda::malloc(4);
        assert_eq!(cuda::get_access(address, 1), cuda::NONE);
        zero(address);
        cu!(cuCtxSetCurrent(contexts[1]));
        cu!(cuCtxEnablePeerAccess(contexts[0], 0));
        cuda::increment(address);
        assert_eq!(cuda::read_u32(address), 1, "re-enabled peer access");
    });
}

#[test]
fn failed_grant_during_peer_enable_restores_existing_mallocs() {
    let preload = format!(
        "{} {}",
        shim::artifacts().frontend.display(),
        fixtures::fail_grant().display()
    );
    run_with(Options::default().env("LD_PRELOAD", preload), || {
        let contexts = created_contexts();
        cu!(cuCtxSetCurrent(contexts[0]));
        let addresses = [cuda::malloc(4), cuda::malloc(4)];
        cu!(cuCtxSetCurrent(contexts[1]));
        let fail_grant: extern "C" fn(u32) = loader::global(c"fail_grant");
        // The first malloc's grant succeeds and the second's applies but fails.
        fail_grant(2);
        let status = unsafe { sys::cuCtxEnablePeerAccess(contexts[0], 0) };
        assert_eq!(status, CUresult::CUDA_ERROR_OUT_OF_MEMORY);
        for address in addresses {
            assert_eq!(cuda::get_access(address, 1), cuda::NONE, "{address:#x}");
            assert_eq!(
                cuda::get_access(address, 0),
                cuda::READWRITE,
                "{address:#x}"
            );
        }
        // The rollback also undid the native enable and committed no peer.
        let status = unsafe { sys::cuCtxDisablePeerAccess(contexts[0]) };
        assert_eq!(status, CUresult::CUDA_ERROR_PEER_ACCESS_NOT_ENABLED);
        cu!(cuCtxSetCurrent(contexts[0]));
        assert_eq!(cuda::get_access(cuda::malloc(4), 1), cuda::NONE);
        cu!(cuCtxSetCurrent(contexts[1]));
        cu!(cuCtxEnablePeerAccess(contexts[0], 0));
        for address in addresses {
            zero(address);
            cuda::increment(address);
            assert_eq!(cuda::read_u32(address), 1, "{address:#x} after re-enable");
        }
    });
}

#[test]
fn mallocs_after_peer_context_destroyed_get_no_peer_access() {
    run(|| {
        let contexts = created_contexts();
        cu!(cuCtxSetCurrent(contexts[1]));
        cu!(cuCtxEnablePeerAccess(contexts[0], 0));
        cu!(cuCtxDestroy_v2(contexts[1]));
        cu!(cuCtxSetCurrent(contexts[0]));
        let address = cuda::malloc(4);
        assert_eq!(cuda::get_access(address, 1), cuda::NONE);
    });
}
