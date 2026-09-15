// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use super::process::Socket;
use super::state::{self, Result};
use cuinterpose_protocol::{Header, Identity, Operation};
use std::io::Write;
use std::os::fd::AsRawFd;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::atomic::Ordering;
use std::sync::mpsc::{self, TrySendError};

// One running control operation and at most eight waiting connections. Peer
// exports never enter this queue: reciprocal importers need them to progress.
const CONTROL_QUEUE_CAPACITY: usize = 8;

pub fn start(endpoint: &str, identity: Identity) -> Result<()> {
    let listener = Socket::open(|| UnixListener::bind(endpoint))
        .map_err(|_| cuinterpose_abi::NOT_INITIALIZED)?;
    let started = (|| -> std::io::Result<()> {
        listener.set_nonblocking(true)?;
        std::fs::set_permissions(endpoint, std::fs::Permissions::from_mode(0o600))?;
        let (sender, receiver) =
            mpsc::sync_channel::<(Socket<UnixStream>, Header)>(CONTROL_QUEUE_CAPACITY);
        let _worker = std::thread::Builder::new()
            .name("cuinterpose-control".into())
            .spawn(move || {
                // Queued sockets remain in the atfork FD registry.
                while let Ok((socket, request)) = receiver.recv() {
                    cuinterpose_abi::boundary(&super::FAILED, (), || {
                        let _ = serve(socket, request, identity);
                    });
                }
            })
            .map_err(|error| {
                eprintln!("cuinterpose: control worker startup failed: {error}");
                error
            })?;
        let started = std::thread::Builder::new()
            .name("cuinterpose-peer".into())
            .spawn(move || {
                loop {
                    let mut poll = libc::pollfd {
                        fd: listener.as_raw_fd(),
                        events: libc::POLLIN,
                        revents: 0,
                    };
                    if unsafe { libc::poll(&mut poll, 1, -1) } <= 0 {
                        continue;
                    }
                    let Ok(socket) = Socket::open(|| listener.accept().map(|(socket, _)| socket))
                    else {
                        continue;
                    };
                    cuinterpose_abi::boundary(&super::FAILED, (), || {
                        let _ = dispatch(socket, identity, &sender);
                    });
                }
            });
        if let Err(error) = started {
            // Failed spawn drops its closure, closing the listener and the
            // only sender. Detach the idle worker: initialization may run in a
            // DSO constructor holding the loader lock, which worker TLS startup
            // or teardown also needs. Joining here would deadlock. No request
            // was queued; recv exits once thread startup can finish.
            eprintln!("cuinterpose: peer listener startup failed: {error}");
            return Err(error);
        }
        Ok(())
    })();
    if started.is_err() {
        // We successfully bound this path, so it is ours to remove. A bind
        // failure above must never unlink an application-owned filesystem entry.
        let _ = std::fs::remove_file(endpoint);
        return Err(cuinterpose_abi::NOT_INITIALIZED);
    }
    Ok(())
}

fn dispatch(
    socket: Socket<UnixStream>,
    identity: Identity,
    sender: &mpsc::SyncSender<(Socket<UnixStream>, Header)>,
) -> std::io::Result<()> {
    // Classification uses per-I/O socket timeouts, not a total header deadline.
    // A slow peer can delay acceptance, but never waits on STATE or lifecycle
    // CUDA calls. Fork during active protocol traffic is outside the contract.
    let timeout = Some(cuinterpose_protocol::timeout(Operation::Handshake));
    socket.set_read_timeout(timeout)?;
    socket.set_write_timeout(timeout)?;
    let (request, descriptor) = cuinterpose_protocol::receive_header(&socket)?;
    if descriptor.is_some()
        || request.status != 0
        || request.count != 0
        || !(request.participant == identity
            || (request.operation == Operation::Handshake && request.participant == [0; 33]))
    {
        return refuse(
            &socket,
            request.operation,
            identity,
            "invalid cuinterpose control request",
        );
    }
    if request.operation == Operation::Export {
        return serve(socket, request, identity);
    }
    match sender.try_send((socket, request)) {
        Ok(()) => Ok(()),
        Err(error) => {
            let ((socket, request), message) = match error {
                TrySendError::Full(request) => {
                    (request, "control queue full; refused without mutation")
                }
                TrySendError::Disconnected(request) => (
                    request,
                    "control worker unavailable; refused without mutation",
                ),
            };
            refuse(&socket, request.operation, identity, message)
        }
    }
}

fn refuse(
    socket: &UnixStream,
    operation: Operation,
    identity: Identity,
    message: &str,
) -> std::io::Result<()> {
    let mut response = Header::new(operation, identity);
    response.status = -1;
    let length = message.len().min(response.message.len() - 1);
    response.message[..length].copy_from_slice(&message.as_bytes()[..length]);
    cuinterpose_protocol::send_header(socket, &response, None)
}

fn serve(
    mut socket: Socket<UnixStream>,
    request: Header,
    identity: Identity,
) -> std::io::Result<()> {
    let mut response = Header::new(request.operation, identity);
    let mut records = Vec::new();
    let mut passed = None;
    let mut cuda_error = None;
    let result = (|| -> std::result::Result<(), &'static str> {
        if super::FAILED.load(Ordering::Acquire) {
            return Err("cuinterpose state failed");
        }
        if request.operation == Operation::Export {
            if !matches!(request.resource_kind, 1 | 2) {
                return Err("creator resource is unavailable");
            }
            passed = Some(
                state::cache()
                    .map_err(|_| "export cache unavailable")?
                    .acquire(&(request.resource_kind, request.allocation))
                    .map_err(|_| "creator resource is unavailable")?,
            );
            response.resource_kind = request.resource_kind;
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
                state
                    .validate_lifecycle(request.operation as u16)
                    .map_err(|_| "CUDA lifecycle operation refused without mutation")?;
                let operation = request.operation as u16;
                let result = if (9..=12).contains(&operation) {
                    super::multicast::restore_phase(state, operation)
                        .map(|bytes| super::host_carrier::Transfer { bytes, copy_us: 0 })
                } else {
                    state.lifecycle(operation)
                };
                match result {
                    Ok(transfer) => {
                        response.payload_size = transfer.bytes;
                        response.copy_us = transfer.copy_us;
                    }
                    Err(code) => {
                        cuda_error = Some(code);
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
        let message = match cuda_error {
            Some(code) => format!("{message}: CUDA error {code}"),
            None => message.to_owned(),
        };
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
