// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Native imports of memory exported by a process without the shim.

use cuinterpose_gpu_tests::process::{self, Options, channel, pid, spawn, spawn_with};
use cuinterpose_gpu_tests::{cu, cuda, shim};

const CONTENTS: &[u8] = b"exported without the shim";

#[test]
fn import_from_unshimmed_process_blocks_checkpoint_until_released() {
    match process::role().as_deref() {
        Some("exporter") => return exporter(),
        Some("importer") => return importer(),
        _ => {}
    }
    // Without the shim, the exporter hands out a CUDA descriptor instead of a shim ticket.
    let source = spawn_with("exporter", Options::unshimmed());
    let (size, descriptor) = source.recv_fd::<usize>();
    let target = spawn("importer");
    target.send_fd(&size, &descriptor);
    target.wait();
    source.send(&"imports closed");
    source.wait();
}

fn exporter() {
    cuda::primary_contexts(1);
    let properties = cuda::allocation_properties(0, false);
    let (handle, address, size) = cuda::create_mapped(&properties, None);
    cuda::write_bytes(address, CONTENTS);
    channel().send_fd(&size, &cuda::export_fd(handle));
    // Keep the backing alive until the importer is done.
    assert_eq!(channel().recv::<String>(), "imports closed");
}

fn importer() {
    let (size, descriptor) = channel().recv_fd::<usize>();
    cuda::primary_contexts(1);
    let handle = cuda::import_fd(&descriptor);
    drop(descriptor);
    cuda::assert_handle_namespace(handle, false, "foreign import");
    let address = cuda::map(handle, size, cuda::device(0));
    cuda::assert_bytes(address, CONTENTS, "foreign mapping");
    assert_refused(1, 1);
    cu!(cuMemRelease(handle));
    assert_refused(0, 1);
    let retained = cuda::retain(address);
    assert_refused(1, 1);
    cuda::unmap(address, size);
    assert_refused(1, 0);
    cu!(cuMemRelease(retained));
    // Preparation runs the same check, so a successful inspection shows nothing blocks it.
    let result = shim::coordinate("--inspect", &[pid()], None);
    assert!(result.success, "{}", result.stderr);
}

/// Requires inspection to refuse this process for its foreign handles and mappings.
fn assert_refused(handles: usize, mappings: usize) {
    let result = shim::coordinate("--inspect", &[pid()], None);
    let expected = format!("{handles} foreign allocation handles and {mappings} foreign mappings");
    assert!(
        !result.success && result.stderr.contains(&expected),
        "{}",
        result.stderr
    );
}
