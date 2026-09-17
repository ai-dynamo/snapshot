// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Two prestarted workers separate peer FD service from serialized CUDA control.
//! Queue pressure refuses requests before mutation; no operation is retried.

use super::process::Socket;
use super::state::{self, Result};
use cudarc::driver::sys::CUresult::CUDA_ERROR_NOT_INITIALIZED;
use cuinterpose_protocol::{self as protocol, Operation, ParticipantId, Reply, Request, Response};
use rustix::event::{PollFd, PollFlags, poll};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::atomic::Ordering;
use std::sync::mpsc::{self, TrySendError};

// One running control operation and at most eight waiting connections. Peer
// exports never enter this queue: reciprocal importers need them to progress.
const CONTROL_QUEUE_CAPACITY: usize = 8;

enum ControlRequest {
    Handshake,
    Inspect,
    Execute(Operation),
}

pub fn start(endpoint: &str, identity: ParticipantId) -> Result<()> {
    let listener =
        Socket::open(|| UnixListener::bind(endpoint)).map_err(|_| CUDA_ERROR_NOT_INITIALIZED)?;
    let started = (|| -> std::io::Result<()> {
        listener.set_nonblocking(true)?;
        std::fs::set_permissions(endpoint, std::fs::Permissions::from_mode(0o600))?;
        let (sender, receiver) =
            mpsc::sync_channel::<(Socket<UnixStream>, ControlRequest)>(CONTROL_QUEUE_CAPACITY);
        let _worker = std::thread::Builder::new()
            .name("cuinterpose-control".into())
            .spawn(move || {
                // Queued sockets remain in the atfork FD registry.
                while let Ok((socket, request)) = receiver.recv() {
                    crate::boundary::call(&super::G_FAILED, (), || {
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
                    let mut events = [PollFd::new(&*listener, PollFlags::IN)];
                    match poll(&mut events, None) {
                        Err(rustix::io::Errno::INTR) => continue,
                        Ok(_) if events[0].revents() == PollFlags::IN => {}
                        _ => {
                            super::G_FAILED.store(true, Ordering::Release);
                            break;
                        }
                    }
                    let Ok(socket) = Socket::open(|| listener.accept().map(|(socket, _)| socket))
                    else {
                        continue;
                    };
                    crate::boundary::call(&super::G_FAILED, (), || {
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
        return Err(CUDA_ERROR_NOT_INITIALIZED.into());
    }
    Ok(())
}

fn dispatch(
    socket: Socket<UnixStream>,
    identity: ParticipantId,
    sender: &mpsc::SyncSender<(Socket<UnixStream>, ControlRequest)>,
) -> protocol::Result<()> {
    // Classification uses per-I/O socket timeouts, not a total header deadline.
    // A slow peer can delay acceptance, but never waits on STATE or lifecycle
    // CUDA calls. Fork during active protocol traffic is outside the contract.
    let timeout = Some(cuinterpose_protocol::timeout(None));
    socket.set_read_timeout(timeout)?;
    socket.set_write_timeout(timeout)?;
    let (request, descriptor): (Request, _) = protocol::receive(&socket)?;
    let identified = match &request {
        Request::Handshake => true,
        Request::Inspect { participant }
        | Request::Execute { participant, .. }
        | Request::Export { participant, .. } => *participant == identity,
    };
    if descriptor.is_some() || !identified {
        return refuse(&socket, identity, "invalid cuinterpose control request");
    }
    let request = match request {
        Request::Handshake => ControlRequest::Handshake,
        Request::Inspect { .. } => ControlRequest::Inspect,
        Request::Execute { operation, .. } => ControlRequest::Execute(operation),
        Request::Export {
            resource,
            allocation,
            ..
        } => {
            if super::G_FAILED.load(Ordering::Acquire) {
                return refuse(&socket, identity, "cuinterpose state failed");
            }
            let lease =
                match state::cache().and_then(|cache| cache.acquire(&(resource, allocation))) {
                    Ok(lease) => lease,
                    Err(_) => return refuse(&socket, identity, "creator resource is unavailable"),
                };
            return protocol::send(
                &socket,
                &Response {
                    participant: identity,
                    result: Ok(Reply::Export {
                        resource,
                        allocation,
                    }),
                },
                Some(lease.descriptor()),
            );
        }
    };
    match sender.try_send((socket, request)) {
        Ok(()) => Ok(()),
        Err(error) => {
            let ((socket, _), message) = match error {
                TrySendError::Full(request) => {
                    (request, "control queue full; refused without mutation")
                }
                TrySendError::Disconnected(request) => (
                    request,
                    "control worker unavailable; refused without mutation",
                ),
            };
            refuse(&socket, identity, message)
        }
    }
}

fn refuse(socket: &UnixStream, identity: ParticipantId, message: &str) -> protocol::Result<()> {
    protocol::send(
        socket,
        &Response {
            participant: identity,
            result: Err(message.into()),
        },
        None,
    )
}

fn serve(
    socket: Socket<UnixStream>,
    request: ControlRequest,
    identity: ParticipantId,
) -> protocol::Result<()> {
    let loading = matches!(request, ControlRequest::Execute(Operation::LoadAllocations));
    let result = (|| -> std::result::Result<Reply, String> {
        if super::G_FAILED.load(Ordering::Acquire) {
            return Err("cuinterpose state failed".into());
        }
        let mut state = state::get().map_err(|_| "cuinterpose state is unavailable")?;
        match request {
            ControlRequest::Handshake => Ok(Reply::Handshake),
            ControlRequest::Inspect => {
                let live_raw_imports = state.live_raw_imports();
                let unsupported_creations = state.unsupported_exportable_creations();
                let records = state
                    .inspect()
                    .map_err(|_| "cannot inspect current CUDA state")?;
                Ok(Reply::Inspection {
                    records,
                    live_raw_imports,
                    unsupported_creations,
                })
            }
            ControlRequest::Execute(operation) => {
                state
                    .validate_lifecycle(operation)
                    .map_err(|_| "CUDA lifecycle operation refused without mutation")?;
                let result = if matches!(
                    operation,
                    Operation::RestoreMulticastCreators
                        | Operation::RestoreMulticastImporters
                        | Operation::RestoreMulticastDevices
                        | Operation::RestoreMulticastBindings
                ) {
                    super::multicast::restore_phase(state, operation)
                        .map(|bytes| super::host_carrier::Transfer { bytes, copy_us: 0 })
                } else {
                    state.lifecycle(operation)
                };
                match result {
                    Ok(transfer) => Ok(Reply::Completed {
                        operation,
                        bytes: transfer.bytes,
                        copy_us: transfer.copy_us,
                    }),
                    Err(code) => {
                        super::G_FAILED.store(true, Ordering::Release);
                        Err(format!("CUDA lifecycle operation failed: {code}"))
                    }
                }
            }
        }
    })();
    let loaded = loading && result.is_ok();
    protocol::send(
        &socket,
        &Response {
            participant: identity,
            result,
        },
        None,
    )?;
    if loaded
        && let Ok(mut state) = state::get()
        && let Some(arena) = state.arena.take()
        && arena.release().is_err()
    {
        super::G_FAILED.store(true, Ordering::Release);
    }
    Ok(())
}
