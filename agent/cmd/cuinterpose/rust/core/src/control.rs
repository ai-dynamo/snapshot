// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::state::{self, CACHE, Result};
use cuinterpose_protocol::{Header, Identity, Operation};
use std::io::Write;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::atomic::Ordering;

pub fn start() -> Result<()> {
    let state = state::get()?;
    let listener =
        UnixListener::bind(&state.endpoint).map_err(|_| cuinterpose_abi::NOT_INITIALIZED)?;
    std::fs::set_permissions(&state.endpoint, std::fs::Permissions::from_mode(0o600))
        .map_err(|_| cuinterpose_abi::NOT_INITIALIZED)?;
    let identity = state.identity;
    drop(state);
    std::thread::Builder::new()
        .name("cuinterpose".into())
        .spawn(move || {
            for socket in listener.incoming() {
                let Ok(socket) = socket else {
                    continue;
                };
                // A peer request must progress independently of a CUDA lifecycle
                // request. Each handler contains panics before leaving its thread.
                let _ = std::thread::Builder::new()
                    .name("cuinterpose-rpc".into())
                    .spawn(move || {
                        cuinterpose_abi::boundary(&super::FAILED, (), || {
                            let _ = serve(socket, identity);
                        });
                    });
            }
        })
        .map_err(|_| cuinterpose_abi::NOT_INITIALIZED)?;
    Ok(())
}

fn serve(mut socket: UnixStream, identity: Identity) -> std::io::Result<()> {
    let timeout = Some(cuinterpose_protocol::timeout(Operation::Handshake));
    socket.set_read_timeout(timeout)?;
    socket.set_write_timeout(timeout)?;
    let (request, descriptor) = cuinterpose_protocol::receive_header(&socket)?;
    let mut response = Header::new(request.operation, identity);
    let mut records = Vec::new();
    let mut passed = None;
    let result = (|| -> std::result::Result<(), &'static str> {
        if descriptor.is_some()
            || request.status != 0
            || request.count != 0
            || !(request.participant == identity
                || (request.operation == Operation::Handshake && request.participant == [0; 33]))
        {
            return Err("invalid cuinterpose control request");
        }
        if super::FAILED.load(Ordering::Acquire) {
            return Err("cuinterpose state failed");
        }
        if request.operation == Operation::Export {
            if request.resource_kind != 1 {
                return Err("creator resource is unavailable");
            }
            passed = Some(
                CACHE
                    .acquire(&request.allocation)
                    .map_err(|_| "creator resource is unavailable")?,
            );
            response.resource_kind = 1;
            response.allocation = request.allocation;
            return Ok(());
        }
        let mut state = state::get().map_err(|_| "cuinterpose state is unavailable")?;
        let stats = state.stats();
        response.phase = stats.phase as u8;
        response.live_raw_imports = stats
            .live_raw_imports
            .try_into()
            .map_err(|_| "too many raw imports")?;
        response.unsupported_creations = stats
            .unsupported_exportable_creations
            .try_into()
            .map_err(|_| "too many unsupported creations")?;
        match request.operation {
            Operation::Handshake => {}
            Operation::Inspect => {
                records = state
                    .inspect()
                    .map_err(|_| "cannot inspect current CUDA state")?;
            }
            _ => {
                let start = std::time::Instant::now();
                match state.lifecycle(request.operation as u16) {
                    Ok(bytes) => {
                        response.payload_size = bytes;
                        response.copy_us =
                            start.elapsed().as_micros().min(u128::from(u32::MAX)) as u32;
                    }
                    Err(_) => {
                        super::FAILED.store(true, Ordering::Release);
                        return Err("CUDA lifecycle operation failed");
                    }
                }
            }
        }
        Ok(())
    })();
    if let Err(message) = result {
        response.status = -1;
        let length = message.len().min(response.message.len() - 1);
        response.message[..length].copy_from_slice(&message.as_bytes()[..length]);
    }
    response.count = records.len() as u32;
    if request.operation == Operation::Inspect {
        response.payload_size = (records.len() * cuinterpose_protocol::RECORD_SIZE) as u64;
    }
    cuinterpose_protocol::send_header(
        &socket,
        &response,
        passed.as_ref().map(|lease| lease.descriptor()),
    )?;
    drop(passed);
    for record in records.drain(..) {
        socket.write_all(&record.encode())?;
    }
    if request.operation == Operation::LoadAllocations && response.status == 0 {
        if let Ok(mut state) = state::get() {
            if let Some(arena) = state.arena.take() {
                if arena.release().is_err() {
                    super::FAILED.store(true, Ordering::Release);
                }
            }
        }
    }
    Ok(())
}
