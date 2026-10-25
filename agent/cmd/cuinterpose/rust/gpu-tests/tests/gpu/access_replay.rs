// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! cuMemSetAccess updates recorded per mapping and replayed after reconstruction.

use cuinterpose_gpu_tests::cuda::{self, NONE, READ, READWRITE};
use cuinterpose_gpu_tests::process::{pid, run};
use cuinterpose_gpu_tests::sys::{self, CUdeviceptr, CUmemAccess_flags, CUmemAccessDesc, CUresult};
use cuinterpose_gpu_tests::{cu, shim};

/// GPU 0's and GPU 1's access to `address`.
fn granted(address: CUdeviceptr) -> [CUmemAccess_flags; 2] {
    [0, 1].map(|device| cuda::get_access(address, device))
}

#[test]
fn repeated_set_access_restores_final_per_device_access() {
    run(|| {
        cuda::primary_contexts(1);
        let properties = cuda::allocation_properties(0, false);
        let (handle, address, size) = cuda::create_mapped(&properties, None);
        cuda::mark_shared(handle);
        cuda::set_access(address, size, &[(cuda::device(1), READWRITE)]);
        // The last descriptor for a location wins within one call.
        cuda::set_access(
            address,
            size,
            &[(cuda::device(1), READWRITE), (cuda::device(1), READ)],
        );
        // NONE removes GPU 0's entry, and GPU 1 keeps the access this call omits.
        cuda::set_access(address, size, &[(cuda::device(0), NONE)]);
        assert_eq!(granted(address), [NONE, READ], "before reconstruction");
        shim::checkpoint(&[pid()], false);
        assert_eq!(granted(address), [NONE, READ], "after reconstruction");
        // Unmapping and releasing check the restored mapping and handle.
        cuda::unmap(address, size);
        cu!(cuMemRelease(handle));
    });
}

#[test]
fn set_access_rejected_on_partial_mapping_records_no_change() {
    access_over_three_granules(Layout::Single);
}

#[test]
fn set_access_spanning_adjacent_mappings_restores_on_each_mapping() {
    access_over_three_granules(Layout::Adjacent);
}

/// How three granules of one reservation are backed.
#[derive(Clone, Copy)]
enum Layout {
    /// One allocation mapped over all three.
    Single,
    /// A separate allocation mapped into each.
    Adjacent,
}

const CONTENTS: [&[u8]; 3] = [b"granule 0", b"granule 1", b"granule 2"];

fn access_over_three_granules(layout: Layout) {
    run(move || {
        cuda::primary_contexts(1);
        let properties = cuda::allocation_properties(0, false);
        let granule = cuda::granularity(&properties);
        let size = 3 * granule;
        let mut address = 0;
        cu!(cuMemAddressReserve(&mut address, size, 0, 0, 0));
        let length = match layout {
            Layout::Single => size,
            Layout::Adjacent => granule,
        };
        let range = address..address + size as u64;
        let mappings: Vec<_> = range
            .clone()
            .step_by(length)
            .map(|start| {
                let mut handle = 0;
                cu!(cuMemCreate(&mut handle, length, &properties, 0));
                cu!(cuMemMap(start, length, 0, handle, 0));
                cuda::mark_shared(handle);
                (start, handle)
            })
            .collect();
        cuda::set_access(address, size, &[(cuda::device(0), READWRITE)]);
        let granules: Vec<_> = range.step_by(granule).collect();
        for (&start, data) in granules.iter().zip(CONTENTS) {
            cuda::write_bytes(start, data);
        }
        let expected = match layout {
            Layout::Single => {
                // The shim prepares an update for the mapping at address before the driver
                // rejects the partial range, so it must not publish that update.
                let read = CUmemAccessDesc {
                    location: cuda::device(0),
                    flags: READ,
                };
                let status = unsafe { sys::cuMemSetAccess(address, granule, &read, 1) };
                assert_eq!(status, CUresult::CUDA_ERROR_INVALID_VALUE);
                READWRITE
            }
            Layout::Adjacent => {
                // One call spans all three mappings, so every one of their records must
                // change.
                cuda::set_access(address, size, &[(cuda::device(0), READ)]);
                READ
            }
        };
        shim::checkpoint(&[pid()], false);
        let access: Vec<_> = granules
            .iter()
            .map(|&start| cuda::get_access(start, 0))
            .collect();
        assert_eq!(access, [expected; 3], "after reconstruction");
        for (&start, data) in granules.iter().zip(CONTENTS) {
            cuda::assert_bytes(start, data, "after reconstruction");
        }
        // Unmapping and releasing check the restored mappings and handles.
        for (start, handle) in mappings {
            cu!(cuMemUnmap(start, length));
            cu!(cuMemRelease(handle));
        }
    });
}
