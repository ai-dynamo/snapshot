// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Importers keep shared backing restorable after its creator drops every local reference,
//! and backing that every holder released leaves no record.

use cuinterpose_gpu_tests::process::{self, Child, channel, spawn};
use cuinterpose_gpu_tests::sys::{
    CUdeviceptr, CUmemAllocationProp, CUmemAllocationType, CUmemGenericAllocationHandle,
};
use cuinterpose_gpu_tests::{cu, cuda, shim};
use cuinterpose_protocol::Record;
use serde::{Deserialize, Serialize};

const HEAD: &[u8] = b"full backing starts here";
const TAIL: &[u8] = b"full backing ends here";

/// What an importer keeps between commands.
#[derive(Clone, Copy, Serialize, Deserialize)]
enum Retention {
    /// The imported handle.
    Handle,
    /// A mapping of the import, whose handle it releases.
    Mapping,
}

#[derive(Serialize, Deserialize)]
enum Command {
    Write(Vec<u8>),
    Verify(Vec<u8>),
    Stop,
}

#[test]
fn device_backing_restores_after_creator_releases_it_while_importers_hold_mappings_and_handles() {
    restore_after_creator_releases(false);
}

#[test]
fn host_numa_backing_restores_after_creator_releases_it_while_importers_hold_mappings_and_handles()
{
    restore_after_creator_releases(true);
}

fn restore_after_creator_releases(host_numa: bool) {
    match process::role().as_deref() {
        Some("creator") => return creator(host_numa),
        Some("importer") => return importer(host_numa),
        _ => {}
    }
    if host_numa {
        shim::require_host_numa();
    }
    let creator = spawn("creator");
    let mut importers = [spawn("importer"), spawn("importer")];
    // When the creator holds nothing, the lowest-PID holder saves the backing, so a GPU 1
    // context reconstructs backing that a GPU 0 context created. The owner saves it from
    // a mapping and the peer reimports it into a handle.
    importers.sort_by_key(Child::pid);
    let [owner, peer] = importers;
    let (size, descriptor) = creator.recv_fd::<usize>();
    for (importer, device, retention) in [
        (&owner, 1, Retention::Mapping),
        (&peer, 0, Retention::Handle),
    ] {
        importer.send_fd(&(device, retention, size), &descriptor);
        assert_eq!(importer.recv::<String>(), "ready");
    }
    drop(descriptor);
    creator.send(&"release");
    assert_eq!(creator.recv::<String>(), "released");
    // Imports still name their creator, so it must stay in the checkpoint group.
    let omitted = shim::coordinate("--inspect", &[owner.pid(), peer.pid()], None);
    assert!(
        !omitted.success && omitted.stderr.contains("missing creator"),
        "{}",
        omitted.stderr
    );
    let pids = [creator.pid(), owner.pid(), peer.pid()];
    for cycle in 0..2 {
        let manifest = shim::checkpoint(&pids, true);
        assert_eq!(manifest[&creator.pid()], []);
        for importer in [&owner, &peer] {
            let sizes: Vec<_> = manifest[&importer.pid()]
                .iter()
                .filter_map(|record| match record {
                    Record::Allocation { size, .. } => Some(*size),
                    _ => None,
                })
                .collect();
            assert_eq!(sizes, [size as u64]);
            importer.command(&Command::Verify(HEAD.to_vec()));
        }
        // A write through one holder is visible through the other.
        let changed = vec![0x60 + cycle; HEAD.len()];
        owner.command(&Command::Write(changed.clone()));
        peer.command(&Command::Verify(changed));
        peer.command(&Command::Write(HEAD.to_vec()));
    }
    creator.send(&"stop");
    creator.wait();
    for importer in [owner, peer] {
        importer.send(&Command::Stop);
        importer.wait();
    }
}

#[test]
fn backing_released_between_checkpoints_leaves_no_record() {
    match process::role().as_deref() {
        Some("replacing-creator") => return replacing_creator(),
        Some("replacing-importer") => return replacing_importer(),
        _ => {}
    }
    let creator = spawn("replacing-creator");
    let importer = spawn("replacing-importer");
    let pids = [creator.pid(), importer.pid()];
    let mut previous = None;
    for value in 1..=3u8 {
        // Both processes release the previous backing, then share a new one.
        creator.send(&Some(value));
        let (size, descriptor) = creator.recv_fd::<usize>();
        importer.send(&Some(value));
        importer.send_fd(&size, &descriptor);
        assert_eq!(importer.recv::<String>(), "ready");
        let manifest = shim::checkpoint(&pids, true);
        let references: Vec<_> = pids
            .iter()
            .flat_map(|pid| &manifest[pid])
            .filter_map(|record| match record {
                Record::Allocation { allocation, .. } => Some(*allocation),
                _ => None,
            })
            .collect();
        assert_eq!(references.len(), 2, "{manifest:?}");
        assert_eq!(references[0], references[1]);
        assert_ne!(previous, Some(references[0]), "stale record");
        previous = Some(references[0]);
    }
    for child in [creator, importer] {
        child.send(&None::<u8>);
        child.wait();
    }
}

/// Replaces its allocation with a new one, filled with each value it receives.
fn replacing_creator() {
    cuda::primary_contexts(1);
    let properties = cuda::allocation_properties(0, false);
    let mut current = None;
    while let Some(value) = channel().recv::<Option<u8>>() {
        // Releasing first lets the driver reuse the backing and handle.
        release(current.take());
        let (handle, address, size) = cuda::create_mapped(&properties, None);
        cuda::write_bytes(address, &[value; 16]);
        channel().send_fd(&size, &cuda::export_fd(handle));
        current = Some((handle, address, size));
    }
    release(current);
}

/// Replaces its import with each one it receives, after checking the restored contents
/// of the previous one.
fn replacing_importer() {
    cuda::primary_contexts(1);
    let mut current = None;
    while let Some(value) = channel().recv::<Option<u8>>() {
        release_checked(current.take());
        let (size, descriptor) = channel().recv_fd::<usize>();
        let handle = cuda::import_fd(&descriptor);
        let address = cuda::map(handle, size, cuda::device(0));
        current = Some((handle, address, size, value));
        channel().send(&"ready");
    }
    release_checked(current);
}

fn release_checked(import: Option<(CUmemGenericAllocationHandle, CUdeviceptr, usize, u8)>) {
    if let Some((handle, address, size, value)) = import {
        cuda::assert_bytes(address, &[value; 16], "after restore");
        release(Some((handle, address, size)));
    }
}

// Releasing after restore checks the restored handle and mapping.
fn release(allocation: Option<(CUmemGenericAllocationHandle, CUdeviceptr, usize)>) {
    if let Some((handle, address, size)) = allocation {
        cuda::unmap(address, size);
        cu!(cuMemRelease(handle));
    }
}

fn creator(host_numa: bool) {
    cuda::primary_contexts(1);
    let properties = cuda::allocation_properties(0, host_numa);
    // Marking both ends of a two-granule allocation shows that the whole backing survives.
    let size = 2 * cuda::granularity(&properties);
    let (handle, address, _) = cuda::create_mapped(&properties, Some(size));
    write_bytes(address, HEAD, host_numa);
    write_bytes(tail(address, size), TAIL, host_numa);
    channel().send_fd(&size, &cuda::export_fd(handle));
    assert_eq!(channel().recv::<String>(), "release");
    cuda::unmap(address, size);
    cu!(cuMemRelease(handle));
    channel().send(&"released");
    assert_eq!(channel().recv::<String>(), "stop");
}

fn importer(host_numa: bool) {
    let ((device, retention, size), descriptor) = channel().recv_fd::<(i32, Retention, usize)>();
    cu!(cuInit(0));
    cuda::make_primary_current(device);
    let handle = cuda::import_fd(&descriptor);
    drop(descriptor);
    let imported = cuda::properties_of(handle);
    let access = if host_numa {
        imported.location
    } else {
        cuda::device(device)
    };
    let mapping = match retention {
        Retention::Handle => None,
        Retention::Mapping => {
            let mapping = cuda::map(handle, size, access);
            cu!(cuMemRelease(handle));
            Some(mapping)
        }
    };
    channel().send(&"ready");
    loop {
        let (write, data) = match channel().recv() {
            Command::Write(data) => (true, data),
            Command::Verify(data) => (false, data),
            Command::Stop => break,
        };
        let handle = match mapping {
            Some(mapping) => cuda::retain(mapping),
            None => handle,
        };
        assert_eq!(
            driver_flags(&cuda::properties_of(handle)),
            driver_flags(&imported),
            "reconstruction changed flags"
        );
        let address = cuda::map(handle, size, access);
        if write {
            write_bytes(address, &data, host_numa);
        } else {
            assert_bytes(address, &data, host_numa, "head");
            assert_bytes(tail(address, size), TAIL, host_numa, "tail");
        }
        cuda::unmap(address, size);
        if mapping.is_some() {
            cu!(cuMemRelease(handle));
        }
        channel().send(&"ok");
    }
    // Releasing after restore checks the restored handle or mapping.
    match mapping {
        Some(mapping) => cuda::unmap(mapping, size),
        None => cu!(cuMemRelease(handle)),
    }
}

/// The properties that come from the driver's view of the current backing. The shim
/// reports location and handle types from its records instead.
fn driver_flags(properties: &CUmemAllocationProp) -> (CUmemAllocationType, u8, u8, u16) {
    let flags = properties.allocFlags;
    (
        properties.type_,
        flags.compressionType,
        flags.gpuDirectRDMACapable,
        flags.usage,
    )
}

fn tail(address: CUdeviceptr, size: usize) -> CUdeviceptr {
    address + (size - TAIL.len()) as u64
}

// HOST_NUMA mappings grant access only to the CPU.
fn write_bytes(address: CUdeviceptr, data: &[u8], host_numa: bool) {
    if host_numa {
        // SAFETY: callers pass a range of a HOST_NUMA mapping with CPU access.
        unsafe { cuda::write_host(address, data) };
    } else {
        cuda::write_bytes(address, data);
    }
}

fn assert_bytes(address: CUdeviceptr, expected: &[u8], host_numa: bool, stage: &str) {
    if host_numa {
        // SAFETY: callers pass a range of a HOST_NUMA mapping with CPU access.
        let actual = unsafe { cuda::read_host(address, expected.len()) };
        assert_eq!(actual, expected, "{stage}");
    } else {
        cuda::assert_bytes(address, expected, stage);
    }
}
