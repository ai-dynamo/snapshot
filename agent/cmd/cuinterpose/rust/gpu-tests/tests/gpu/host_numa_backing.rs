// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Creator-owned HOST_NUMA backing and its CPU aliases survive reconstruction.

use cuinterpose_gpu_tests::process::{self, Options, channel, run_with};
use cuinterpose_gpu_tests::sys::CUdeviceptr;
use cuinterpose_gpu_tests::{cu, cuda, shim};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;

const DEVICE_BYTES: &[u8] = b"mixed device backing";

#[derive(Serialize, Deserialize)]
enum Command {
    Write(u8),
    Verify(u8),
    Stop,
}

#[test]
fn host_numa_backing_and_cpu_aliases_restore_on_creator_numa_node() {
    if process::role().as_deref() == Some("importer") {
        return importer();
    }
    shim::require_host_numa();
    let gpus = std::env::var("CUDA_VISIBLE_DEVICES").unwrap_or_else(|_| "0,1".into());
    let first = gpus.split(',').next().unwrap().to_owned();
    // With one visible GPU, a nonzero NUMA node used as a device ordinal fails instead of
    // selecting another GPU.
    run_with(
        Options::default().env("CUDA_VISIBLE_DEVICES", first),
        creator,
    );
}

fn creator() {
    cu!(cuInit(0));
    let properties = cuda::allocation_properties(0, true);
    let size = cuda::granularity(&properties);
    // Creating, mapping and exporting record the current context. Without one, the
    // carrier falls back to a device that it must not derive from the NUMA node.
    let mut handle = 0;
    cu!(cuMemCreate(&mut handle, size, &properties, 0));
    cuda::assert_handle_namespace(handle, true, "HOST_NUMA create");
    let aliases: Vec<_> = (0..2)
        .map(|_| cuda::map(handle, size, properties.location))
        .collect();
    fill(aliases[0], size, 0x31);
    let descriptor = cuda::export_fd(handle);
    // The same carrier also reconstructs device backing created in a context.
    cuda::primary_contexts(1);
    let (device_handle, device_address, device_size) =
        cuda::create_mapped(&cuda::allocation_properties(0, false), None);
    cuda::write_bytes(device_address, DEVICE_BYTES);
    cuda::mark_shared(device_handle);
    let importer = process::spawn("importer");
    importer.send_fd(&size, &descriptor);
    drop(descriptor);
    // CUDA reports node 0 for every imported HOST_NUMA handle, so the shim reports the
    // creator's node. This and the one-GPU limit only catch a bug when GPU 0's nearest
    // node is nonzero.
    assert_eq!(
        importer.recv::<i32>(),
        properties.location.id,
        "the import changed the NUMA node"
    );
    let mut value = 0x31;
    for cycle in 0..2 {
        shim::checkpoint(&[process::pid(), importer.pid()], false);
        for &alias in &aliases {
            assert_filled(alias, size, value, "after restore");
        }
        cuda::assert_bytes(device_address, DEVICE_BYTES, "mixed carrier");
        importer.command(&Command::Verify(value));
        value = 0x52 + cycle;
        importer.command(&Command::Write(value));
        for &alias in &aliases {
            assert_filled(alias, size, value, "importer write");
        }
        value = 0x73 + cycle;
        fill(aliases[1], size, value);
        importer.command(&Command::Verify(value));
    }
    importer.send(&Command::Stop);
    importer.wait();
    // Teardown after restore checks the restored aliases and handles.
    for alias in aliases {
        cuda::unmap(alias, size);
    }
    cu!(cuMemRelease(handle));
    cuda::unmap(device_address, device_size);
    cu!(cuMemRelease(device_handle));
}

fn importer() {
    let (size, descriptor) = channel().recv_fd::<usize>();
    cu!(cuInit(0));
    let handle = cuda::import_fd(&descriptor);
    drop(descriptor);
    cuda::assert_handle_namespace(handle, true, "HOST_NUMA import");
    let properties = cuda::properties_of(handle);
    let address = cuda::map(handle, size, properties.location);
    channel().send(&properties.location.id);
    loop {
        match channel().recv() {
            Command::Write(value) => fill(address, size, value),
            Command::Verify(value) => assert_filled(address, size, value, "importer"),
            Command::Stop => break,
        }
        channel().send(&"ok");
    }
    // Teardown after restore checks the restored mapping and handle.
    cuda::unmap(address, size);
    cu!(cuMemRelease(handle));
}

fn fill(address: CUdeviceptr, size: usize, value: u8) {
    // SAFETY: callers pass a HOST_NUMA mapping of `size` bytes with CPU access.
    unsafe { cuda::write_host(address, &vec![value; size]) };
}

fn assert_filled(address: CUdeviceptr, size: usize, value: u8, stage: &str) {
    // SAFETY: callers pass a HOST_NUMA mapping of `size` bytes with CPU access.
    let found: BTreeSet<u8> = unsafe { cuda::read_host(address, size) }
        .into_iter()
        .collect();
    assert_eq!(
        found,
        BTreeSet::from([value]),
        "{stage}: expected only {value}"
    );
}
