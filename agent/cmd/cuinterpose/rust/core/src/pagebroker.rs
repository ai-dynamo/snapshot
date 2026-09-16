// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Allocation-only capabilities supplied by the agent. This module cannot bind
//! transactions or choose storage paths; no broker socket survives an operation.

use crate::driver::{self, CudaError, Result};
use crate::host_carrier::{AllocationContent, Context};
use cuinterpose_abi::{INVALID_HANDLE, UNKNOWN};
use prost::Message;
use rustix::net::{SendAncillaryBuffer, SendAncillaryMessage, SendFlags, sendmsg};
use std::collections::BTreeMap;
use std::io::{self, IoSlice, Read, Write};
use std::mem::MaybeUninit;
use std::os::fd::{AsFd, OwnedFd};
use std::os::unix::net::UnixStream;
use std::time::{Duration, Instant};

#[allow(
    dead_code,
    reason = "generated shared schema also defines broker-owned manifests"
)]
mod wire {
    include!(concat!(env!("OUT_DIR"), "/snapshot.pagebroker.v1.rs"));
}
use wire::{AllocationBatch, AllocationExtent, AllocationSessionReply, AllocationSessionRequest};
use wire::{allocation_session_reply::Result as Reply, allocation_session_request::Command};

const MAX_BATCH: usize = 32;
const MAX_FRAME: usize = 64 * 1024;

fn exchange(socket: &UnixStream, command: Command, descriptors: &[OwnedFd]) -> io::Result<Reply> {
    let body = AllocationSessionRequest {
        command: Some(command),
    }
    .encode_to_vec();
    if body.len() > MAX_FRAME || descriptors.len() > MAX_BATCH {
        return Err(io::Error::other("PageBroker batch exceeds protocol bounds"));
    }
    let mut frame = Vec::with_capacity(4 + body.len());
    frame.extend_from_slice(&(body.len() as u32).to_be_bytes());
    frame.extend_from_slice(&body);
    let descriptors: Vec<_> = descriptors.iter().map(AsFd::as_fd).collect();
    let mut space = [MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(MAX_BATCH))];
    let mut ancillary = SendAncillaryBuffer::new(&mut space);
    if !descriptors.is_empty() {
        ancillary.push(SendAncillaryMessage::ScmRights(&descriptors));
    }
    let sent = loop {
        match sendmsg(
            socket,
            &[IoSlice::new(&frame)],
            &mut ancillary,
            SendFlags::NOSIGNAL,
        ) {
            Err(rustix::io::Errno::INTR) => continue,
            result => break result?,
        }
    };
    if sent == 0 {
        return Err(io::Error::other("PageBroker session closed"));
    }
    let mut socket = socket;
    socket.write_all(&frame[sent..])?;
    let mut prefix = [0; 4];
    socket.read_exact(&mut prefix)?;
    let size = u32::from_be_bytes(prefix) as usize;
    if size > MAX_FRAME {
        return Err(io::Error::other("PageBroker reply exceeds protocol bounds"));
    }
    let mut body = vec![0; size];
    socket.read_exact(&mut body)?;
    match AllocationSessionReply::decode(body.as_slice())?.result {
        Some(Reply::Failure(failure)) => Err(io::Error::other(
            failure
                .message
                .unwrap_or_else(|| "PageBroker transfer failed".into()),
        )),
        Some(reply) => Ok(reply),
        None => Err(io::Error::other("PageBroker reply has no result")),
    }
}

/// Complete all batches and the participant manifest, even for an empty set.
/// LOAD backing is retained in the caller on failure: process termination, not
/// an early release while a disconnected worker drains DMA, owns its cleanup.
pub fn transfer(
    session: OwnedFd,
    allocations: &mut [AllocationContent],
    load: bool,
) -> Result<u32> {
    let socket = UnixStream::from(session);
    let start = Instant::now();
    let result = (|| -> Result<()> {
        // Capabilities may originate from an agent runtime's nonblocking poller.
        socket.set_nonblocking(false).map_err(failed)?;
        socket
            .set_read_timeout(Some(Duration::from_secs(300)))
            .map_err(failed)?;
        socket
            .set_write_timeout(Some(Duration::from_secs(300)))
            .map_err(failed)?;
        // Export the complete rank before starting storage I/O. CUDA's public
        // export API takes one handle; grouping avoids per-allocation context
        // entry and UUID lookup without depending on private driver APIs.
        let mut groups = BTreeMap::<(usize, i32), Vec<usize>>::new();
        for (index, allocation) in allocations.iter().enumerate() {
            groups
                .entry((allocation.context, allocation.properties.location.id))
                .or_default()
                .push(index);
        }
        let mut extents = Vec::with_capacity(allocations.len());
        let mut descriptors = Vec::with_capacity(allocations.len());
        for ((context, device), indices) in groups {
            Context::run(context, device, || {
                let mut uuid = cudarc::driver::sys::CUuuid { bytes: [0; 16] };
                unsafe { driver::cuDeviceGetUuid(&mut uuid, device) }?;
                for index in indices {
                    let allocation = &mut allocations[index];
                    if load {
                        if allocation.driver.is_some() {
                            return Err(CudaError(INVALID_HANDLE));
                        }
                        let mut handle = 0;
                        let mut backing = allocation.properties;
                        if backing.handle_types == 0 {
                            backing.handle_types = cuinterpose_abi::POSIX_FD;
                        }
                        unsafe { driver::cuMemCreate(&mut handle, allocation.size, &backing, 0) }?;
                        allocation.driver = Some(handle);
                    }
                    let descriptor =
                        driver::export_posix(allocation.driver.ok_or(INVALID_HANDLE)?)?;
                    extents.push(AllocationExtent {
                        allocation_id: cuinterpose_protocol::ParticipantId(allocation.id.0)
                            .to_string(),
                        size: allocation.size as u64,
                        device_uuid: uuid.bytes.iter().map(|byte| *byte as u8).collect(),
                    });
                    descriptors.push(descriptor);
                }
                Ok(())
            })?;
        }
        for (extents, descriptors) in extents.chunks(MAX_BATCH).zip(descriptors.chunks(MAX_BATCH)) {
            let reply = exchange(
                &socket,
                Command::Batch(AllocationBatch {
                    extents: extents.to_vec(),
                }),
                descriptors,
            )
            .map_err(failed)?;
            let Reply::Completed(completed) = reply else {
                return Err(failed(io::Error::other(
                    "expected PageBroker batch completion",
                )));
            };
            if completed.extents.len() != extents.len()
                || !completed
                    .extents
                    .iter()
                    .zip(extents)
                    .all(|(actual, expected)| {
                        actual.allocation_id == expected.allocation_id
                            && actual.size == expected.size
                            && actual.device_uuid == expected.device_uuid
                    })
            {
                return Err(failed(io::Error::other(
                    "PageBroker completion metadata mismatch",
                )));
            }
        }
        if !matches!(
            exchange(&socket, Command::Finish(wire::CommitRequest {}), &[]).map_err(failed)?,
            Reply::Finished(_)
        ) {
            return Err(failed(io::Error::other(
                "expected PageBroker manifest completion",
            )));
        }
        Ok(())
    })();
    // Closing the scoped connection is mandatory before native CUDA/CRIU.
    drop(socket);
    result?;
    Ok(start.elapsed().as_micros().min(u32::MAX as u128) as u32)
}

fn failed(error: io::Error) -> CudaError {
    eprintln!("cuinterpose: allocation PageBroker transfer: {error}");
    CudaError(UNKNOWN)
}
