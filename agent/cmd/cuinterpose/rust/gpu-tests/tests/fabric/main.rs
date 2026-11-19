// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! FABRIC memory and multicast objects shared by two processes on one node, on a real
//! driver. A FABRIC import starts without a kind: the shim learns it from the first call
//! that only one kind accepts, or at checkpoint from its creator's records.
//!
//! These tests need FABRIC handle support, which needs an IMEX channel, so they run in
//! their own binary: `cargo test -p cuinterpose-gpu-tests --test fabric -- --test-threads=1`.

use cuinterpose_gpu_tests::process::{self, channel, pid, spawn};
use cuinterpose_gpu_tests::sys::{
    self, CUdevice_attribute, CUmemAllocationHandleType, CUmemAllocationProp,
    CUmemGenericAllocationHandle, CUmulticastObjectProp,
};
use cuinterpose_gpu_tests::{cu, cuda, shim};
use cuinterpose_protocol::{FABRIC_HANDLE_BYTES, FabricKind, Record};

const FABRIC: CUmemAllocationHandleType = CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_FABRIC;
const CONTENTS: &[u8] = b"shared through a fabric handle";
const MEMBER_VALUES: [u32; 2] = [0x1234_5678, 0x1111_1111];

/// Requires FABRIC handles and multicast on both GPUs. Scenario processes skip the check.
fn require_fabric() {
    shim::require_multicast();
    if process::role().is_some() {
        return;
    }
    let attribute = CUdevice_attribute::CU_DEVICE_ATTRIBUTE_HANDLE_TYPE_FABRIC_SUPPORTED;
    for device in 0..2 {
        assert!(
            cuda::device_attribute(attribute, device) != 0,
            "GPU {device} does not support FABRIC handles"
        );
    }
}

fn memory_properties(device: i32) -> CUmemAllocationProp {
    let mut properties = cuda::allocation_properties(device, false);
    properties.requestedHandleTypes = FABRIC;
    properties
}

/// A two-GPU FABRIC multicast object of one minimum granule. Returns it and its size.
fn multicast_group() -> (CUmemGenericAllocationHandle, usize) {
    let mut properties = CUmulticastObjectProp {
        numDevices: 2,
        size: 0,
        handleTypes: u64::from(FABRIC.0),
        flags: 0,
    };
    let minimum = sys::CUmulticastGranularity_flags::CU_MULTICAST_GRANULARITY_MINIMUM;
    cu!(cuMulticastGetGranularity(
        &mut properties.size,
        &properties,
        minimum
    ));
    let mut group = 0;
    cu!(cuMulticastCreate(&mut group, &properties));
    (group, properties.size)
}

fn export(handle: CUmemGenericAllocationHandle) -> Vec<u8> {
    let mut bytes = [0; FABRIC_HANDLE_BYTES];
    cu!(cuMemExportToShareableHandle(
        bytes.as_mut_ptr().cast(),
        handle,
        FABRIC,
        0
    ));
    bytes.to_vec()
}

fn import(bytes: &[u8]) -> CUmemGenericAllocationHandle {
    let mut bytes: [u8; FABRIC_HANDLE_BYTES] = bytes.try_into().unwrap();
    let mut handle = 0;
    cu!(cuMemImportFromShareableHandle(
        &mut handle,
        bytes.as_mut_ptr().cast(),
        FABRIC
    ));
    handle
}

/// The kind of each FABRIC import that this process reports.
fn import_kinds() -> Vec<Option<FabricKind>> {
    shim::records(pid())
        .into_iter()
        .filter_map(|record| match record {
            Record::FabricImport { kind, .. } => Some(kind),
            _ => None,
        })
        .collect()
}

fn expect(step: &str) {
    assert_eq!(channel().recv::<String>(), step);
}

/// A member allocation on `device` whose u32 values are all `value`.
fn member(device: i32, size: usize, value: u32) -> CUmemGenericAllocationHandle {
    let properties = cuda::allocation_properties(device, false);
    let (member, address, _) = cuda::create_mapped(&properties, Some(size));
    cu!(cuMemsetD32_v2(address, value, size / 4));
    cu!(cuCtxSynchronize());
    member
}

#[test]
fn memory_mapped_before_any_multicast_call_takes_its_kind_from_the_creator() {
    match process::role().as_deref() {
        Some("creator") => return creator(),
        Some("importer") => return importer(),
        _ => {}
    }
    require_fabric();
    let creator = spawn("creator");
    let importer = spawn("importer");
    // The handles carry no kind, so the test passes them on as bytes.
    for _ in 0..2 {
        importer.send(&creator.recv::<(Vec<u8>, usize)>());
    }
    assert_eq!(importer.recv::<String>(), "added");
    for step in ["bind", "map"] {
        for child in [&creator, &importer] {
            child.command(&step);
        }
    }
    let manifest = shim::checkpoint(&[creator.pid(), importer.pid()], true);
    // BeginCheckpoint classified the memory import from its creator's records.
    let mut kinds: Vec<_> = manifest[&importer.pid()]
        .iter()
        .filter_map(|record| match record {
            Record::FabricImport { kind, .. } => Some(*kind),
            _ => None,
        })
        .collect();
    kinds.sort();
    assert_eq!(
        kinds,
        [Some(FabricKind::Memory), Some(FabricKind::Multicast)]
    );
    for child in [&creator, &importer] {
        child.command(&"restored");
    }
    for child in [importer, creator] {
        child.send(&"stop");
        child.wait();
    }
}

/// Creates FABRIC memory and a multicast object on GPU 0, and binds a member to it.
fn creator() {
    cu!(cuInit(0));
    cuda::make_primary_current(0);
    let properties = memory_properties(0);
    let (memory, address, size) = cuda::create_mapped(&properties, None);
    cuda::write_bytes(address, CONTENTS);
    let (group, group_size) = multicast_group();
    cu!(cuMulticastAddDevice(group, 0));
    channel().send(&(export(memory), size));
    channel().send(&(export(group), group_size));
    expect("bind");
    let member = member(0, group_size, MEMBER_VALUES[0]);
    cu!(cuMulticastBindMem(group, 0, member, 0, group_size, 0));
    channel().send(&"ok");
    expect("map");
    let multicast_address = cuda::map(group, group_size, cuda::device(0));
    let sum = MEMBER_VALUES.iter().sum();
    assert_eq!(cuda::multimem_sum(multicast_address), sum);
    channel().send(&"ok");
    expect("restored");
    assert_eq!(cuda::multimem_sum(multicast_address), sum);
    cuda::assert_bytes(address, CONTENTS, "creator after restore");
    channel().send(&"ok");
    expect("stop");
    cu!(cuMulticastUnbind(group, 0, 0, group_size));
    cuda::unmap(multicast_address, group_size);
    cu!(cuMemRelease(member));
    cu!(cuMemRelease(group));
    cuda::unmap(address, size);
    cu!(cuMemRelease(memory));
}

/// Maps the creator's memory on GPU 1 before any multicast call, then joins the
/// multicast object with a member of its own.
fn importer() {
    cu!(cuInit(0));
    cuda::make_primary_current(1);
    let (bytes, size) = channel().recv::<(Vec<u8>, usize)>();
    let memory = import(&bytes);
    // cuMemMap and cuMemSetAccess accept either kind, so the import stays unknown.
    let address = cuda::map(memory, size, cuda::device(1));
    cuda::assert_bytes(address, CONTENTS, "importer before checkpoint");
    assert_eq!(import_kinds(), [None]);
    let (bytes, group_size) = channel().recv::<(Vec<u8>, usize)>();
    let group = import(&bytes);
    assert_eq!(import_kinds(), [None, None]);
    cu!(cuMulticastAddDevice(group, 1));
    let mut kinds = import_kinds();
    kinds.sort();
    assert_eq!(kinds, [None, Some(FabricKind::Multicast)]);
    channel().send(&"added");
    expect("bind");
    let member = member(1, group_size, MEMBER_VALUES[1]);
    cu!(cuMulticastBindMem_v2(group, 1, 0, member, 0, group_size, 0));
    channel().send(&"ok");
    expect("map");
    let multicast_address = cuda::map(group, group_size, cuda::device(1));
    let sum = MEMBER_VALUES.iter().sum();
    assert_eq!(cuda::multimem_sum(multicast_address), sum);
    channel().send(&"ok");
    expect("restored");
    let mut kinds = import_kinds();
    kinds.sort();
    assert_eq!(
        kinds,
        [Some(FabricKind::Memory), Some(FabricKind::Multicast)]
    );
    cuda::assert_bytes(address, CONTENTS, "importer after restore");
    assert_eq!(cuda::multimem_sum(multicast_address), sum);
    channel().send(&"ok");
    expect("stop");
    cu!(cuMulticastUnbind(group, 1, 0, group_size));
    cuda::unmap(multicast_address, group_size);
    cu!(cuMemRelease(member));
    cu!(cuMemRelease(group));
    cuda::unmap(address, size);
    cu!(cuMemRelease(memory));
}

#[test]
fn multicast_object_that_a_process_only_maps_takes_its_kind_from_the_creator() {
    match process::role().as_deref() {
        Some("creator") => return team_creator(),
        Some("importer") => return mapper(),
        _ => {}
    }
    require_fabric();
    let creator = spawn("creator");
    let importer = spawn("importer");
    importer.send(&creator.recv::<(Vec<u8>, usize)>());
    assert_eq!(importer.recv::<String>(), "mapped");
    let manifest = shim::checkpoint(&[creator.pid(), importer.pid()], true);
    let classified = manifest[&importer.pid()].iter().any(|record| {
        matches!(
            record,
            Record::FabricImport {
                kind: Some(FabricKind::Multicast),
                ..
            }
        )
    });
    assert!(classified, "{manifest:?}");
    for child in [&creator, &importer] {
        child.command(&"restored");
    }
    for child in [importer, creator] {
        child.send(&"stop");
        child.wait();
    }
}

/// Adds both GPUs to a FABRIC multicast object and binds a member on each, so another
/// process can map the object without any multicast call of its own.
fn team_creator() {
    cu!(cuInit(0));
    cuda::make_primary_current(0);
    let (group, size) = multicast_group();
    let mut members = Vec::new();
    for device in [0, 1] {
        cu!(cuMulticastAddDevice(group, device));
    }
    for device in [0, 1] {
        // A member is mapped with access for its own GPU only, so it is filled from that GPU.
        cuda::make_primary_current(device);
        let member = member(device, size, MEMBER_VALUES[device as usize]);
        cu!(cuMulticastBindMem_v2(group, device, 0, member, 0, size, 0));
        members.push(member);
    }
    cuda::make_primary_current(0);
    channel().send(&(export(group), size));
    expect("restored");
    channel().send(&"ok");
    expect("stop");
    for (device, member) in [0, 1].into_iter().zip(members) {
        cu!(cuMulticastUnbind(group, device, 0, size));
        cu!(cuMemRelease(member));
    }
    cu!(cuMemRelease(group));
}

/// Imports the multicast object and maps it on GPU 1, which accepts either kind.
fn mapper() {
    cu!(cuInit(0));
    cuda::make_primary_current(1);
    let (bytes, size) = channel().recv::<(Vec<u8>, usize)>();
    let group = import(&bytes);
    let address = cuda::map(group, size, cuda::device(1));
    let sum = MEMBER_VALUES.iter().sum();
    assert_eq!(cuda::multimem_sum(address), sum);
    assert_eq!(import_kinds(), [None]);
    channel().send(&"mapped");
    expect("restored");
    assert_eq!(import_kinds(), [Some(FabricKind::Multicast)]);
    assert_eq!(cuda::multimem_sum(address), sum);
    channel().send(&"ok");
    expect("stop");
    cuda::unmap(address, size);
    cu!(cuMemRelease(group));
}
