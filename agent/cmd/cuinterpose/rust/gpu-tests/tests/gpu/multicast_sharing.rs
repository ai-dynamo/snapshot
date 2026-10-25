// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! A multicast object shared by two processes that each bind one GPU's member.

use cuinterpose_gpu_tests::process::{self, channel, pid, spawn};
use cuinterpose_gpu_tests::{cu, cuda, shim};
use cuinterpose_protocol::{AllocationReference, BindingVersion, Record};

const MEMBER_VALUES: [u32; 2] = [0x1234_5678, 0x1111_1111];

/// How a process obtains the multicast object and binds its member.
#[derive(Clone, Copy)]
enum Role {
    /// Creates the object, adds GPU 0, exports it, and binds with cuMulticastBindMem.
    Creator,
    /// Imports the object, adds GPU 1, and binds with cuMulticastBindMem_v2.
    Importer,
}

impl Role {
    fn device(self) -> i32 {
        match self {
            Self::Creator => 0,
            Self::Importer => 1,
        }
    }

    fn version(self) -> BindingVersion {
        match self {
            Self::Creator => BindingVersion::V1,
            Self::Importer => BindingVersion::V2,
        }
    }
}

#[test]
fn multicast_group_with_one_member_per_process_restores_through_native_checkpoint() {
    match process::role().as_deref() {
        Some("creator") => return bound_member(Role::Creator),
        Some("importer") => return bound_member(Role::Importer),
        _ => {}
    }
    shim::require_multicast();
    let creator = spawn("creator");
    let importer = spawn("importer");
    let (size, ticket) = creator.recv_fd::<usize>();
    importer.send_fd(&size, &ticket);
    drop(ticket);
    // The creator added GPU 0 before it exported, so the importer's GPU completes the
    // group. Neither bind then waits for the other process.
    assert_eq!(importer.recv::<String>(), "added");
    let children = [(&creator, Role::Creator), (&importer, Role::Importer)];
    // Both members are bound before either process reads through its mapping.
    for step in ["bind", "map"] {
        for (child, _) in children {
            child.command(&step);
        }
    }
    let manifest = shim::checkpoint(&[creator.pid(), importer.pid()], true);
    // Both processes name the creator's object, and each records only its own GPU.
    let object = manifest[&creator.pid()]
        .iter()
        .find_map(|record| match record {
            Record::Multicast { allocation, .. } => Some(*allocation),
            _ => None,
        })
        .expect("the creator records its multicast object");
    assert_eq!(object.creator_pid, creator.pid());
    for (child, role) in children {
        let records = &manifest[&child.pid()];
        let (device, version) = (role.device(), role.version());
        assert_eq!(
            membership(records),
            (vec![object], vec![device], vec![(device, version)]),
            "{records:?}"
        );
    }
    for (child, _) in children {
        child.command(&"restored");
    }
    for child in [importer, creator] {
        child.send(&"stop");
        child.wait();
    }
}

/// Binds one member on this process's GPU, and reads the sum of both processes' members
/// through the multicast mapping before and after restore.
fn bound_member(role: Role) {
    let device = role.device();
    cu!(cuInit(0));
    cuda::make_primary_current(device);
    let (group, size) = match role {
        Role::Creator => {
            let (group, size) = cuda::multicast_group(&[device]);
            channel().send_fd(&size, &cuda::export_fd(group));
            (group, size)
        }
        Role::Importer => {
            let (size, ticket) = channel().recv_fd::<usize>();
            let group = cuda::import_fd(&ticket);
            drop(ticket);
            cu!(cuMulticastAddDevice(group, device));
            channel().send(&"added");
            (group, size)
        }
    };
    let properties = cuda::allocation_properties(device, false);
    let (member, address, _) = cuda::create_mapped(&properties, Some(size));
    cu!(cuMemsetD32_v2(
        address,
        MEMBER_VALUES[device as usize],
        size / 4
    ));
    cu!(cuCtxSynchronize());
    expect("bind");
    match role.version() {
        BindingVersion::V1 => cu!(cuMulticastBindMem(group, 0, member, 0, size, 0)),
        BindingVersion::V2 => cu!(cuMulticastBindMem_v2(group, device, 0, member, 0, size, 0)),
    }
    channel().send(&"ok");
    expect("map");
    let multicast_address = cuda::map(group, size, cuda::device(device));
    let sum = MEMBER_VALUES.iter().sum();
    assert_eq!(
        cuda::multimem_sum(multicast_address),
        sum,
        "before checkpoint"
    );
    let records = multicast_records(&shim::records(pid()));
    channel().send(&"ok");
    expect("restored");
    assert_eq!(multicast_records(&shim::records(pid())), records);
    assert_eq!(cuda::multimem_sum(multicast_address), sum, "after restore");
    channel().send(&"ok");
    expect("stop");
    // Cleanup after restore checks the restored binding, mappings, and handles.
    cu!(cuMulticastUnbind(group, device, 0, size));
    cuda::unmap(multicast_address, size);
    cuda::unmap(address, size);
    cu!(cuMemRelease(member));
    cu!(cuMemRelease(group));
}

fn expect(step: &str) {
    assert_eq!(channel().recv::<String>(), step);
}

fn multicast_records(records: &[Record]) -> Vec<Record> {
    records
        .iter()
        .filter(|record| {
            matches!(
                record,
                Record::Multicast { .. }
                    | Record::MulticastDevice { .. }
                    | Record::MulticastBinding { .. }
                    | Record::MulticastMapping { .. }
            )
        })
        .cloned()
        .collect()
}

/// The multicast objects in `records`, the devices added to them, and the device and
/// version of each binding.
fn membership(
    records: &[Record],
) -> (
    Vec<AllocationReference>,
    Vec<i32>,
    Vec<(i32, BindingVersion)>,
) {
    let (mut objects, mut devices, mut bindings) = (Vec::new(), Vec::new(), Vec::new());
    for record in records {
        match *record {
            Record::Multicast { allocation, .. } => objects.push(allocation),
            Record::MulticastDevice { device, .. } => devices.push(device),
            Record::MulticastBinding {
                device, version, ..
            } => bindings.push((device, version)),
            _ => {}
        }
    }
    (objects, devices, bindings)
}
