// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Legacy cuIpc memory handles opened by a second process across reconstruction.

use cuinterpose_gpu_tests::process::{self, Options, channel, spawn_with};
use cuinterpose_gpu_tests::sys::{self, CUipcMem_flags, CUmemLocationType};
use cuinterpose_gpu_tests::{cu, cuda, shim};

#[derive(Clone, Copy)]
struct Share {
    visibility: Visibility,
    /// Enable peer access to the import after opening it.
    peer: bool,
    native: bool,
}

#[derive(Clone, Copy)]
enum Visibility {
    Both,
    Reordered,
    Disjoint,
}

const SHARE: Share = Share {
    visibility: Visibility::Both,
    peer: false,
    native: false,
};

#[test]
fn ipc_import_restores_through_native_checkpoint_when_processes_order_gpus_differently() {
    // The importer sees the exporter's GPU 0 as its GPU 1, so prepare must accept
    // per-process device ordinals.
    share_and_restore(Share {
        visibility: Visibility::Reordered,
        native: true,
        ..SHARE
    });
}

#[test]
fn ipc_import_restores_when_importer_cannot_see_exporter_gpu() {
    share_and_restore(Share {
        visibility: Visibility::Disjoint,
        ..SHARE
    });
}

#[test]
fn ipc_import_restores_with_peer_access_enabled_after_open() {
    // Device permissions intentionally allow more than native context isolation, so
    // extending an import to a peer of the importing context is a shim extension.
    share_and_restore(Share {
        peer: true,
        ..SHARE
    });
}

/// Increments the exporter's malloc through an IPC import before and after restore.
fn share_and_restore(share: Share) {
    // Increments: open, optionally peer, restore.
    let increments = if share.peer { 3 } else { 2 };
    match process::role().as_deref() {
        Some("exporter") => return exporter(increments),
        Some("importer") => return importer(share),
        _ => {}
    }
    let gpus = std::env::var("CUDA_VISIBLE_DEVICES").unwrap_or_else(|_| "0,1".into());
    let [first, second, ..] = gpus.split(',').collect::<Vec<_>>()[..] else {
        panic!("requires two GPUs in CUDA_VISIBLE_DEVICES, found {gpus:?}");
    };
    let (exporter_options, importer_options) = match share.visibility {
        Visibility::Both => (Options::default(), Options::default()),
        Visibility::Reordered => (
            Options::default(),
            Options::default().env("CUDA_VISIBLE_DEVICES", format!("{second},{first}")),
        ),
        Visibility::Disjoint => (
            Options::default().env("CUDA_VISIBLE_DEVICES", first),
            Options::default().env("CUDA_VISIBLE_DEVICES", second),
        ),
    };
    let source = spawn_with("exporter", exporter_options);
    let target = spawn_with("importer", importer_options);
    target.send(&source.recv::<Vec<u8>>());
    assert_eq!(target.recv::<String>(), "opened");
    shim::checkpoint(&[source.pid(), target.pid()], share.native);
    target.send(&"restored");
    target.wait();
    source.send(&"imports closed");
    source.wait();
}

fn exporter(increments: u32) {
    cuda::primary_contexts(1);
    let address = cuda::malloc(4);
    cuda::write_bytes(address, &0u32.to_le_bytes());
    // SAFETY: CUipcMemHandle is plain data.
    let mut handle: sys::CUipcMemHandle = unsafe { std::mem::zeroed() };
    cu!(cuIpcGetMemHandle(&mut handle, address));
    channel().send(&handle.reserved.map(|byte| byte as u8).to_vec());
    assert_eq!(channel().recv::<String>(), "imports closed");
    assert_eq!(cuda::read_u32(address), increments, "importer increments");
    // Freeing after restore checks the restored malloc.
    cu!(cuMemFree_v2(address));
}

fn importer(share: Share) {
    // The importer's GPU, and the exporter's GPU as the importer numbers it if visible.
    let (device, exporter_gpu) = match share.visibility {
        Visibility::Both => (1, Some(0)),
        Visibility::Reordered => (0, Some(1)),
        Visibility::Disjoint => (0, None),
    };
    cu!(cuInit(0));
    let context = cuda::make_primary_current(device);
    // SAFETY: CUipcMemHandle is plain data.
    let mut handle: sys::CUipcMemHandle = unsafe { std::mem::zeroed() };
    let bytes = channel().recv::<Vec<u8>>();
    handle.reserved = std::array::from_fn(|index| bytes[index] as _);
    let mut address = 0;
    let lazy = CUipcMem_flags::CU_IPC_MEM_LAZY_ENABLE_PEER_ACCESS as u32;
    cu!(cuIpcOpenMemHandle_v2(&mut address, handle, lazy));
    cuda::assert_malloc_semantics(address, true, "IPC import");
    let location = shim::mapped_location(process::pid(), address);
    assert_eq!(
        location.location_type,
        CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE as u32
    );
    if let Some(gpu) = exporter_gpu {
        assert_eq!(location.id, gpu);
    }
    cuda::increment(address);
    if share.peer {
        cuda::make_primary_current(0);
        cu!(cuCtxEnablePeerAccess(context, 0));
        cuda::increment(address);
    }
    channel().send(&"opened");
    assert_eq!(channel().recv::<String>(), "restored");
    cuda::increment(address);
    cu!(cuCtxSetCurrent(context));
    cu!(cuIpcCloseMemHandle(address));
}
