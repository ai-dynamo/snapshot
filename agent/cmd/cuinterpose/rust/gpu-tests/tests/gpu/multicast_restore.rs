// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Multicast bindings through a native checkpoint, including bindings at nonzero offsets,
//! and rejected V1 bindings.

use cuinterpose_gpu_tests::process::{self, channel, pid, run, spawn};
use cuinterpose_gpu_tests::sys::{self, CUdeviceptr, CUmemAllocationType, CUresult};
use cuinterpose_gpu_tests::{cu, cuda, shim};
use cuinterpose_protocol::{BindingSource, BindingVersion, MemberRange, Record};
use std::collections::BTreeMap;

const MEMBER_VALUES: [u32; 2] = [0x1234_5678, 0x1111_1111];

#[derive(Clone, Copy)]
enum Source {
    /// cuMulticastBindMem with a tracked VMM member.
    Memory,
    /// cuMulticastBindAddr with an address.
    Address,
}

// Each version binds on both GPUs. The v1 calls take the device from the member or the
// current context, and v2 replays the device it was given, so each is checked on GPU 1.

#[test]
fn bind_mem_restores_through_native_checkpoint() {
    through_native_checkpoint(|| bound_multicast(Source::Memory, BindingVersion::V1));
}

#[test]
fn bind_mem_v2_restores_through_native_checkpoint() {
    through_native_checkpoint(|| bound_multicast(Source::Memory, BindingVersion::V2));
}

#[test]
fn bind_addr_restores_through_native_checkpoint() {
    through_native_checkpoint(|| bound_multicast(Source::Address, BindingVersion::V1));
}

#[test]
fn bind_addr_v2_restores_through_native_checkpoint() {
    through_native_checkpoint(|| bound_multicast(Source::Address, BindingVersion::V2));
}

#[test]
fn bind_mem_at_nonzero_member_and_multicast_offsets_restores_through_native_checkpoint() {
    through_native_checkpoint(|| bound_at_offsets(Source::Memory));
}

#[test]
fn bind_addr_at_nonzero_multicast_offset_restores_through_native_checkpoint() {
    through_native_checkpoint(|| bound_at_offsets(Source::Address));
}

/// Runs `bound` in a child process. The child sends "ready" after its first checks and
/// repeats them after the native checkpoint, when it receives "continue".
fn through_native_checkpoint(bound: impl FnOnce()) {
    if process::role().as_deref() == Some("bound") {
        return bound();
    }
    shim::require_multicast();
    let child = spawn("bound");
    assert_eq!(child.recv::<String>(), "ready");
    shim::checkpoint(&[child.pid()], true);
    child.send(&"continue");
    child.wait();
}

/// Binds one member per GPU and reads their sum through the multicast mapping before
/// and after the checkpoint.
fn bound_multicast(source: Source, version: BindingVersion) {
    let contexts = cuda::primary_contexts(2);
    let (group, size) = cuda::multicast_group(&[0, 1]);
    let (mut members, mut addresses) = (Vec::new(), Vec::new());
    for (device, value) in (0..).zip(MEMBER_VALUES) {
        cu!(cuCtxSetCurrent(contexts[device as usize]));
        let (member, address) = match source {
            Source::Memory => {
                let properties = cuda::allocation_properties(device, false);
                let (member, address, _) = cuda::create_mapped(&properties, Some(size));
                (member, address)
            }
            // The shim does not track pool memory. CUDA restores it natively.
            Source::Address => (0, pool_address(device, size)),
        };
        addresses.push(address);
        cu!(cuMemsetD32_v2(address, value, size / 4));
        cu!(cuCtxSynchronize());
        match (source, version) {
            (Source::Memory, BindingVersion::V1) => {
                cu!(cuMulticastBindMem(group, 0, member, 0, size, 0))
            }
            (Source::Memory, BindingVersion::V2) => {
                cu!(cuMulticastBindMem_v2(group, device, 0, member, 0, size, 0))
            }
            (Source::Address, BindingVersion::V1) => {
                cu!(cuMulticastBindAddr(group, 0, address, size, 0))
            }
            (Source::Address, BindingVersion::V2) => {
                cu!(cuMulticastBindAddr_v2(group, device, 0, address, size, 0))
            }
        }
        if let Source::Memory = source {
            let held = (device == 0).then_some(member);
            // GPU 1 keeps only a mapping, so restore must retain its member from the
            // mapping.
            if held.is_none() {
                cu!(cuMemRelease(member));
            }
            members.push((held, address));
        }
    }
    let records = shim::records(pid());
    // Each GPU's binding source, in device order.
    let sources: Vec<_> = match source {
        Source::Memory => {
            let tracked: BTreeMap<_, _> = records
                .iter()
                .filter_map(|record| match record {
                    Record::Allocation {
                        allocation,
                        location,
                        ..
                    } => Some((location.id, *allocation)),
                    _ => None,
                })
                .collect();
            let member = |device| MemberRange {
                allocation: tracked[&device],
                offset: 0,
            };
            (0..2)
                .map(|device| BindingSource::Memory(member(device)))
                .collect()
        }
        Source::Address => addresses
            .into_iter()
            .map(|address| BindingSource::Address {
                address,
                tracked_member: None,
            })
            .collect(),
    };
    let expected: BTreeMap<_, _> = (0..)
        .zip(sources.into_iter().map(|source| (source, version)))
        .collect();
    let bindings = multicast_bindings(&records);
    assert_eq!(bindings.len(), 2, "{bindings:?}");
    let recorded: BTreeMap<_, _> = bindings
        .iter()
        .filter_map(|binding| match binding {
            Record::MulticastBinding {
                device,
                source,
                version,
                ..
            } => Some((*device, (*source, *version))),
            _ => None,
        })
        .collect();
    assert_eq!(recorded, expected, "{bindings:?}");
    cu!(cuCtxSetCurrent(contexts[0]));
    let multicast_address = cuda::map(group, size, cuda::device(0));
    let sum = MEMBER_VALUES.iter().sum();
    assert_eq!(cuda::multimem_sum(multicast_address), sum);
    channel().send(&"ready");
    assert_eq!(channel().recv::<String>(), "continue");
    assert_eq!(multicast_bindings(&shim::records(pid())), bindings);
    assert_eq!(cuda::multimem_sum(multicast_address), sum);
    // Cleanup after restore checks the restored bindings, mappings and handles.
    for device in 0..2 {
        cu!(cuMulticastUnbind(group, device, 0, size));
    }
    assert_eq!(multicast_bindings(&shim::records(pid())), []);
    for (held, address) in members {
        cuda::unmap(address, size);
        if let Some(member) = held {
            cu!(cuMemRelease(member));
        }
    }
    cuda::unmap(multicast_address, size);
    cu!(cuMemRelease(group));
}

fn multicast_bindings(records: &[Record]) -> Vec<Record> {
    records
        .iter()
        .filter(|record| matches!(record, Record::MulticastBinding { .. }))
        .cloned()
        .collect()
}

/// Granules in the multicast object and in each GPU's member. A three-granule binding
/// fits at offset 1 or 2 of either.
const GRANULES: usize = 5;

/// Binds member granules 1 to 3 of each GPU to multicast granules 2 to 4, with v1 on
/// GPU 0 and v2 on GPU 1, and checks which member granules multicast granule 2 holds
/// before and after the checkpoint.
///
/// The address source binds from the member's mapping. Mappings have no offset case
/// because cuMemMap documents its offset as currently required to be zero.
fn bound_at_offsets(source: Source) {
    let contexts = cuda::primary_contexts(2);
    let mut properties = cuda::multicast_properties();
    let granule = properties.size;
    let size = GRANULES * granule;
    properties.size = size;
    let mut group = 0;
    cu!(cuMulticastCreate(&mut group, &properties));
    for device in 0..2 {
        cu!(cuMulticastAddDevice(group, device));
    }
    // Each binding maps `length` bytes from member offset `from` to multicast offset `at`.
    let (from, at, length) = (granule, 2 * granule, 3 * granule);
    let mut members = Vec::new();
    for device in 0..2 {
        cu!(cuCtxSetCurrent(contexts[device as usize]));
        let properties = cuda::allocation_properties(device, false);
        let (member, address, _) = cuda::create_mapped(&properties, Some(size));
        for index in 0..GRANULES {
            // Each granule holds its index in this GPU's own byte, so a multimem sum shows
            // which granule each GPU binds there.
            let value = (index as u32) << (8 * device);
            let start = address + (index * granule) as u64;
            cu!(cuMemsetD32_v2(start, value, granule / 4));
        }
        cu!(cuCtxSynchronize());
        let start = address + from as u64;
        match (source, device) {
            (Source::Memory, 0) => cu!(cuMulticastBindMem(group, at, member, from, length, 0)),
            (Source::Memory, _) => {
                cu!(cuMulticastBindMem_v2(
                    group, device, at, member, from, length, 0
                ))
            }
            (Source::Address, 0) => cu!(cuMulticastBindAddr(group, at, start, length, 0)),
            (Source::Address, _) => {
                cu!(cuMulticastBindAddr_v2(group, device, at, start, length, 0))
            }
        }
        members.push((member, address));
    }
    cu!(cuCtxSetCurrent(contexts[0]));
    let multicast = cuda::map(group, size, cuda::device(0));
    // Multicast granule 2 must hold member granule 1 of each GPU. A binding replayed from
    // member offset 0 would put granule 0 there. One replayed at multicast offset 0, or
    // with its offsets swapped, would put granule 3 there. Each still binds multicast
    // granule 2, so the check never reads unbound memory.
    let held = || {
        let [first, second, ..] = cuda::multimem_sum(multicast + at as u64).to_le_bytes();
        [first, second]
    };
    assert_eq!(held(), [1, 1], "before the checkpoint");
    channel().send(&"ready");
    assert_eq!(channel().recv::<String>(), "continue");
    assert_eq!(held(), [1, 1], "after the checkpoint");
    // Cleanup after restore checks the restored bindings, mappings and handles.
    for device in 0..2 {
        cu!(cuMulticastUnbind(group, device, at, length));
    }
    for (member, address) in members {
        cuda::unmap(address, size);
        cu!(cuMemRelease(member));
    }
    cuda::unmap(multicast, size);
    cu!(cuMemRelease(group));
}

#[test]
fn bind_mem_v1_rejects_host_numa_member_without_recording() {
    shim::require_multicast();
    shim::require_host_numa();
    run(|| {
        cuda::primary_contexts(1);
        let (group, size) = cuda::multicast_group(&[0, 1]);
        let properties = cuda::allocation_properties(0, true);
        // A smaller member would be rejected for its size before its location is checked.
        let member_size = size.next_multiple_of(cuda::granularity(&properties));
        let mut member = 0;
        cu!(cuMemCreate(&mut member, member_size, &properties, 0));
        let before = shim::records(pid());
        // cuMulticastBindMem binds a member on the GPU it resides on, which a host member
        // lacks.
        let status = unsafe { sys::cuMulticastBindMem(group, 0, member, 0, size, 0) };
        assert_eq!(status, CUresult::CUDA_ERROR_NOT_SUPPORTED);
        assert_eq!(
            shim::records(pid()),
            before,
            "rejected HOST_NUMA bind changed tracked state"
        );
    });
}

/// A size-aligned address in a native stream-ordered pool on `device`.
fn pool_address(device: i32, size: usize) -> CUdeviceptr {
    // SAFETY: CUmemPoolProps is plain data, and zero is the default of each field.
    let mut properties: sys::CUmemPoolProps = unsafe { std::mem::zeroed() };
    properties.allocType = CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED;
    properties.handleTypes = cuda::POSIX_FD;
    properties.location = cuda::device(device);
    let mut pool = std::ptr::null_mut();
    cu!(cuMemPoolCreate(&mut pool, &properties));
    let mut stream = std::ptr::null_mut();
    cu!(cuStreamCreate(&mut stream, 0));
    let mut base = 0;
    cu!(cuMemAllocFromPoolAsync(&mut base, 2 * size, pool, stream));
    cu!(cuStreamSynchronize(stream));
    base.next_multiple_of(size as u64)
}
