// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Peer access alone selects a host carrier for unexported allocations.

use cuinterpose_gpu_tests::process::{self, channel, pid, run, spawn};
use cuinterpose_gpu_tests::sys::{self, CUcontext, CUmemAllocationHandleType, CUresult};
use cuinterpose_gpu_tests::{cu, cuda, shim};
use cuinterpose_protocol::Record;
use serde::{Deserialize, Serialize};

// Without peer carriers, H200 driver 615.71.09 restored peer-accessible allocations of
// 2 MiB and 510 MiB natively but failed at 512 MiB and 1 GiB. Carrier selection does not
// depend on size, so cases run at the smallest failing size.
const SIZE: usize = 512 << 20;

/// How GPU 0's allocation is made and how GPU 1 reaches it.
#[derive(Clone, Copy, Debug, PartialEq, Serialize, Deserialize)]
enum Case {
    /// cuMemCreate, then a grant to GPU 1.
    Vmm,
    /// The same without a shareable handle type.
    NonexportableVmm,
    /// cuMemAlloc, then GPU 1's context enables peer access.
    MallocWithPeerEnabledAfter,
    VmmWithoutPeer,
    MallocWithoutPeer,
    NonexportableVmmWithoutPeer,
    /// A granted VMM allocation whose handle is released.
    VmmReleased,
    /// A VMM allocation whose grant to GPU 1 is revoked before the checkpoint.
    VmmRevoked,
    /// GPU 1's context enables peer access before cuMemAlloc.
    MallocWithPeerEnabledBefore,
    /// GPU 1's context enables peer access after cuMemAlloc and then disables it.
    MallocWithPeerDisabled,
}

impl Case {
    fn vmm(self) -> bool {
        matches!(
            self,
            Self::Vmm
                | Self::NonexportableVmm
                | Self::VmmWithoutPeer
                | Self::NonexportableVmmWithoutPeer
                | Self::VmmReleased
                | Self::VmmRevoked
        )
    }

    /// GPU 1 received access at some point, so the record must report it.
    fn peer(self) -> bool {
        !matches!(
            self,
            Self::VmmWithoutPeer | Self::NonexportableVmmWithoutPeer | Self::MallocWithoutPeer
        )
    }
}

#[test]
fn peer_accessible_allocation_restores_through_native_checkpoint_vmm() {
    // 1 GiB keeps the carrier tested if a later driver restores 512 MiB natively.
    restore(&[(Case::Vmm, SIZE), (Case::Vmm, 1 << 30)]);
}

#[test]
fn peer_accessible_allocation_restores_through_native_checkpoint_nonexportable_vmm() {
    restore(&[(Case::NonexportableVmm, SIZE)]);
}

#[test]
fn peer_accessible_allocation_restores_through_native_checkpoint_malloc_with_peer_enabled_after() {
    restore(&[(Case::MallocWithPeerEnabledAfter, SIZE)]);
}

#[test]
fn allocation_without_peer_access_restores_without_peer_flag() {
    // The nonexportable case runs last so a failure there does not hide the others.
    restore_variants(&[
        Case::VmmWithoutPeer,
        Case::MallocWithoutPeer,
        Case::NonexportableVmmWithoutPeer,
    ]);
}

#[test]
fn peer_access_vmm_released_selects_carrier() {
    restore(&[(Case::VmmReleased, SIZE)]);
}

#[test]
fn peer_access_vmm_revoked_selects_carrier() {
    restore(&[(Case::VmmRevoked, SIZE)]);
}

#[test]
fn peer_access_malloc_enabled_before_selects_carrier() {
    restore(&[(Case::MallocWithPeerEnabledBefore, SIZE)]);
}

#[test]
fn peer_access_malloc_disabled_selects_carrier() {
    restore(&[(Case::MallocWithPeerDisabled, SIZE)]);
}

fn restore_variants(cases: &[Case]) {
    let cases: Vec<_> = cases.iter().map(|&case| (case, SIZE)).collect();
    restore(&cases);
}

/// Checkpoints a fresh allocating process for each case and size.
fn restore(cases: &[(Case, usize)]) {
    if process::role().as_deref() == Some("allocator") {
        return allocator();
    }
    for &(case, size) in cases {
        let stage = format!("{case:?} at {} MiB", size >> 20);
        // Names the stage in a failed test's captured output, where a failing step such
        // as the checkpoint does not.
        println!("{stage}");
        let child = spawn("allocator");
        child.send(&(case, size));
        assert_eq!(child.recv::<String>(), "ready", "{stage}");
        // Nothing exports this allocation, so only peer access can select a host carrier.
        let manifest = shim::checkpoint(&[child.pid()], true);
        let allocations = allocations(&manifest[&child.pid()]);
        let [
            Record::Allocation {
                size: saved,
                shared,
                peer_access_granted,
                ..
            },
        ] = &allocations[..]
        else {
            panic!("{stage}: {allocations:?}");
        };
        assert!(*saved == size as u64 && !shared, "{stage}: {allocations:?}");
        assert_eq!(
            *peer_access_granted,
            case.peer(),
            "{stage}: {allocations:?}"
        );
        child.send(&"restored");
        child.wait();
    }
}

/// Makes the case's allocation, increments its ends from the accessing GPU, and checks
/// every 2 MiB before and after the checkpoint.
fn allocator() {
    let (case, size): (Case, usize) = channel().recv();
    let stage = format!("{case:?} at {} MiB", size >> 20);
    let contexts = cuda::primary_contexts(2);
    let peer = case.peer();
    if case == Case::MallocWithPeerEnabledBefore {
        enable_peer_access(&contexts);
    }
    let (address, handle) = if case.vmm() {
        let mut properties = cuda::allocation_properties(0, false);
        if matches!(
            case,
            Case::NonexportableVmm | Case::NonexportableVmmWithoutPeer
        ) {
            // Exportability only concerns other processes. Another GPU here can still map it.
            properties.requestedHandleTypes = CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_NONE;
        }
        let (handle, address, _) = cuda::create_mapped(&properties, Some(size));
        if peer {
            cuda::set_access(address, size, &[(cuda::device(1), cuda::READWRITE)]);
        }
        if case == Case::VmmReleased {
            // Saving must then retain the backing from its mapping.
            cu!(cuMemRelease(handle));
            (address, None)
        } else {
            (address, Some(handle))
        }
    } else {
        let address = cuda::malloc(size);
        if matches!(
            case,
            Case::MallocWithPeerEnabledAfter | Case::MallocWithPeerDisabled
        ) {
            enable_peer_access(&contexts);
        }
        (address, None)
    };
    cu!(cuMemsetD32_v2(address, 7, size / 4));
    // The memset may still be running, and kernels in GPU 1's context do not wait for it.
    cu!(cuCtxSynchronize());
    let last = address + size as u64 - 4;
    let samples: Vec<_> = (address..last).step_by(2 << 20).chain([last]).collect();
    let verify = |incremented: u32| {
        for &sample in &samples {
            let expected = if sample == address || sample == last {
                incremented
            } else {
                7
            };
            cuda::assert_bytes(sample, &expected.to_le_bytes(), &stage);
        }
    };
    let accessor = contexts[usize::from(peer)];
    cu!(cuCtxSetCurrent(accessor));
    cuda::increment(address);
    cuda::increment(last);
    verify(8);
    match case {
        Case::VmmRevoked => {
            // A revoked grant still selects the carrier, because native restore may still
            // fail.
            cuda::set_access(address, size, &[(cuda::device(1), cuda::NONE)]);
        }
        Case::MallocWithPeerDisabled => {
            cu!(cuCtxDisablePeerAccess(contexts[0]));
            // Unlike native cuMemAlloc, the shim keeps existing grants after a disable, so
            // GPU 1 still reaches this allocation and its record stays peer-accessible.
            assert_eq!(cuda::get_access(address, 1), cuda::READWRITE, "{stage}");
        }
        _ => {}
    }
    channel().send(&"ready");
    assert_eq!(channel().recv::<String>(), "restored");
    cu!(cuCtxSetCurrent(contexts[0]));
    verify(8);
    if case == Case::VmmRevoked {
        assert_eq!(cuda::get_access(address, 1), cuda::NONE, "{stage}");
        cuda::set_access(address, size, &[(cuda::device(1), cuda::READWRITE)]);
    }
    cu!(cuCtxSetCurrent(accessor));
    cuda::increment(address);
    cuda::increment(last);
    verify(9);
    cu!(cuCtxSetCurrent(contexts[0]));
    if matches!(
        case,
        Case::MallocWithPeerEnabledBefore
            | Case::MallocWithPeerEnabledAfter
            | Case::MallocWithPeerDisabled
    ) {
        // The shim's peer table survives prepare and restore, so allocations made
        // afterwards follow the last enable or disable.
        let fresh = cuda::malloc(4);
        let granted = if case == Case::MallocWithPeerDisabled {
            cuda::NONE
        } else {
            cuda::READWRITE
        };
        assert_eq!(cuda::get_access(fresh, 1), granted, "{stage}");
        cu!(cuMemFree_v2(fresh));
    }
    // Releasing checks the restored allocation's handles.
    if case.vmm() {
        cuda::unmap(address, size);
    } else {
        cu!(cuMemFree_v2(address));
    }
    if let Some(handle) = handle {
        cu!(cuMemRelease(handle));
    }
}

/// Lets GPU 1's context access GPU 0's memory.
fn enable_peer_access(contexts: &[CUcontext]) {
    cu!(cuCtxSetCurrent(contexts[1]));
    cu!(cuCtxEnablePeerAccess(contexts[0], 0));
    cu!(cuCtxSetCurrent(contexts[0]));
}

fn allocations(records: &[Record]) -> Vec<Record> {
    records
        .iter()
        .filter(|record| matches!(record, Record::Allocation { .. }))
        .cloned()
        .collect()
}

#[test]
fn nonexportable_allocation_is_tracked_and_export_fails() {
    run(|| {
        cuda::primary_contexts(1);
        let mut properties = cuda::allocation_properties(0, false);
        properties.requestedHandleTypes = CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_NONE;
        let (handle, _, size) = cuda::create_mapped(&properties, None);
        // Another GPU in this process can still map the backing, so the shim tracks it.
        cuda::assert_handle_namespace(handle, true, "nonexportable create");
        let tracked = allocations(&shim::records(pid()));
        let [
            Record::Allocation {
                size: recorded,
                handle_types,
                shared,
                ..
            },
        ] = &tracked[..]
        else {
            panic!("{tracked:?}");
        };
        assert_eq!(
            (*recorded, *handle_types, *shared),
            (size as u64, 0, false),
            "{tracked:?}"
        );
        let mut fd: i32 = -1;
        let status = unsafe {
            sys::cuMemExportToShareableHandle((&raw mut fd).cast(), handle, cuda::POSIX_FD, 0)
        };
        assert_eq!(status, CUresult::CUDA_ERROR_INVALID_VALUE);
        assert_eq!(allocations(&shim::records(pid())), tracked);
    });
}
