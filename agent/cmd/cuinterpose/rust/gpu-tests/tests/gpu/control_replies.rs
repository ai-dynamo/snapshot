// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Control requests that arrive out of order or whose replies are lost.

use cuinterpose_gpu_tests::process::{self, channel, spawn};
use cuinterpose_gpu_tests::{cuda, shim};
use cuinterpose_protocol::{Operation, Record, Request};
use std::fs;

#[test]
fn only_lost_successful_load_allocations_reply_aborts_process() {
    if process::role().as_deref() == Some("holder") {
        return hold_shared_allocation();
    }
    let child = spawn("holder");
    let size: u64 = child.recv();
    let pid = child.pid();
    let load = Request::Execute {
        namespace_pid: pid,
        operation: Operation::LoadAllocations,
    };
    // Losing the reply to a read-only or rejected request must not abort. The next
    // reply also shows that the serial control worker handled both requests.
    shim::request_without_reply(pid, &Request::Inspect { namespace_pid: pid });
    shim::request_without_reply(pid, &load);
    let error = shim::request(pid, &load).unwrap_err();
    assert!(error.contains("LoadAllocations"), "{error}");
    let save = Request::SaveAllocations {
        namespace_pid: pid,
        owners: vec![],
    };
    let error = shim::request(pid, &save).unwrap_err();
    for expected in [
        "SaveAllocations",
        "expected MulticastPrepared",
        "actual Active",
    ] {
        assert!(error.contains(expected), "{error}");
    }
    // A shared allocation makes the final load copy a real carrier.
    let allocations: Vec<_> = shim::records(pid)
        .into_iter()
        .filter_map(|record| match record {
            Record::Allocation { shared, size, .. } => Some((shared, size)),
            _ => None,
        })
        .collect();
    assert_eq!(allocations, [(true, size)]);
    // Preparation also shows that the rejected requests left the phase unchanged.
    let checkpoint = process::work_dir().join("checkpoint");
    fs::create_dir(&checkpoint).unwrap();
    let prepared = shim::coordinate("--prepare", &[pid], Some(&checkpoint));
    assert!(prepared.success, "{}", prepared.stderr);
    shim::request_without_reply(pid, &load);
    let stderr = child.wait_for_signal(libc::SIGABRT);
    assert!(
        stderr.contains("cannot send successful LoadAllocations reply"),
        "{stderr}"
    );
}

fn hold_shared_allocation() {
    // The expected abort must not write a core dump.
    let limit = libc::rlimit {
        rlim_cur: 0,
        rlim_max: 0,
    };
    assert_eq!(unsafe { libc::setrlimit(libc::RLIMIT_CORE, &limit) }, 0);
    cuda::primary_contexts(1);
    let (handle, _, size) = cuda::create_mapped(&cuda::allocation_properties(0, false), None);
    cuda::mark_shared(handle);
    channel().send(&size);
    channel().recv::<()>();
}
