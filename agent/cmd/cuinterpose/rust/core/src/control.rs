// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Two prestarted workers separate peer FD service from serialized CUDA control.
//! Queue pressure refuses requests before mutation; no operation is retried.

use super::process::Socket;
use super::state::{self, Result};
use cudarc::driver::sys::CUresult::CUDA_ERROR_NOT_INITIALIZED;
use cuinterpose_protocol::{
    self as protocol, Operation, ParticipantDirectory, ParticipantId, Reply, Request, Response,
};
use rustix::event::{PollFd, PollFlags, poll};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::atomic::Ordering;
use std::sync::mpsc::{self, TrySendError};

// One running control operation and at most eight waiting connections. Peer
// exports never enter this queue: reciprocal importers need them to progress.
const CONTROL_QUEUE_CAPACITY: usize = 8;

enum ControlRequest {
    Inspect,
    Execute(Operation),
}

fn install_directory(
    identity: ParticipantId,
    participants: ParticipantDirectory,
) -> protocol::Result<()> {
    if participants.is_empty() || !participants.contains_key(&identity) {
        return Err(protocol::Error::Invalid(
            "rendezvous directory omits this participant",
        ));
    }
    let mut paths = std::collections::BTreeSet::new();
    for path in participants.values() {
        if !path.is_absolute()
            || !paths.insert(path)
            || std::os::unix::net::SocketAddr::from_pathname(path).is_err()
        {
            return Err(protocol::Error::Invalid(
                "invalid rendezvous participant directory",
            ));
        }
    }
    *state::participant_directory()
        .map_err(|_| protocol::Error::Invalid("cuinterpose state is unavailable"))?
        .lock()
        .map_err(|_| protocol::Error::Invalid("participant directory is poisoned"))? = participants;
    Ok(())
}

/// Private workers cannot dispatch until the single listener handoff succeeds.
/// Dropping this owner cancels them without joining: a caller may hold the
/// loader lock needed by a worker's Rust TLS startup or teardown.
pub struct PreparedWorkers {
    activation: mpsc::SyncSender<Socket<UnixListener>>,
    // Owned only between a successful bind and the worker handoff.
    listener: Option<Socket<UnixListener>>,
}

impl PreparedWorkers {
    pub fn prepare(identity: ParticipantId) -> Result<Option<Self>> {
        let (sender, receiver) =
            mpsc::sync_channel::<(Socket<UnixStream>, ControlRequest)>(CONTROL_QUEUE_CAPACITY);
        let (activation, parked) = mpsc::sync_channel::<Socket<UnixListener>>(1);
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
                CUDA_ERROR_NOT_INITIALIZED
            })?;
        if state::initialized() {
            return Ok(None);
        }
        let started = std::thread::Builder::new()
            .name("cuinterpose-peer".into())
            .spawn(move || {
                let Ok(listener) = parked.recv() else {
                    return;
                };
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
            // The failed closure drops the only control-queue sender.
            eprintln!("cuinterpose: peer listener startup failed: {error}");
            return Err(CUDA_ERROR_NOT_INITIALIZED.into());
        }
        Ok(Some(Self {
            activation,
            listener: None,
        }))
    }

    /// No spawn, blocking channel operation, formatting, or callback is allowed
    /// here. The caller holds the generation installation lock.
    /// With pinned Rust/glibc, mutexes and try_send wakeups use futexes and
    /// non-Drop TLS, not loader registration. The channel is preallocated;
    /// socket registration may grow its Vec using the ordinary glibc allocator.
    /// Eager ELF binding prevents first-use loader lookup in these libc calls.
    pub fn activate(&mut self, endpoint: &str) -> Result<()> {
        self.listener = Some(
            Socket::open(|| UnixListener::bind(endpoint))
                .map_err(|_| CUDA_ERROR_NOT_INITIALIZED)?,
        );
        let listener = self.listener.as_ref().unwrap();
        listener
            .set_nonblocking(true)
            .map_err(|_| CUDA_ERROR_NOT_INITIALIZED)?;
        std::fs::set_permissions(endpoint, std::fs::Permissions::from_mode(0o600))
            .map_err(|_| CUDA_ERROR_NOT_INITIALIZED)?;
        match self.activation.try_send(self.listener.take().unwrap()) {
            Ok(()) => Ok(()),
            Err(TrySendError::Full(listener) | TrySendError::Disconnected(listener)) => {
                self.listener = Some(listener);
                Err(CUDA_ERROR_NOT_INITIALIZED.into())
            }
        }
    }

    pub fn cleanup(&mut self, endpoint: &str) {
        // Only successfully bound, unpublished endpoints belong to this owner.
        // Cleanup is deliberately outside the installation lock.
        if let Some(listener) = self.listener.take() {
            drop(listener);
            let _ = std::fs::remove_file(endpoint);
        }
    }
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
        Request::Identify => true,
        Request::Rendezvous { participant, .. }
        | Request::Inspect { participant }
        | Request::Execute { participant, .. } => *participant == identity,
        Request::Export { allocation } => allocation.creator == identity,
    };
    if descriptor.is_some() || !identified {
        return refuse(&socket, identity, "invalid cuinterpose control request");
    }
    let request = match request {
        Request::Identify => {
            return protocol::send(
                &socket,
                &Response {
                    participant: identity,
                    result: Ok(Reply::Identified),
                },
                None,
            );
        }
        Request::Rendezvous { participants, .. } => {
            install_directory(identity, participants)?;
            return protocol::send(
                &socket,
                &Response {
                    participant: identity,
                    result: Ok(Reply::Ready),
                },
                None,
            );
        }
        Request::Inspect { .. } => ControlRequest::Inspect,
        Request::Execute { operation, .. } => ControlRequest::Execute(operation),
        Request::Export { allocation } => {
            if super::G_FAILED.load(Ordering::Acquire) {
                return refuse(&socket, identity, "cuinterpose state failed");
            }
            let lease = match state::cache().and_then(|cache| cache.acquire(&allocation.id)) {
                Ok(lease) => lease,
                Err(_) => return refuse(&socket, identity, "creator resource is unavailable"),
            };
            let reply = match lease.multicast_properties() {
                Some(properties) => Reply::MulticastExport {
                    devices: properties.numDevices,
                    size: properties.size as u64,
                    handle_types: properties.handleTypes,
                    flags: properties.flags,
                },
                None => Reply::UnicastExport,
            };
            return protocol::send(
                &socket,
                &Response {
                    participant: identity,
                    result: Ok(reply),
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
            ControlRequest::Inspect => {
                let live_raw_imports = state.live_raw_imports();
                let unsupported_creations = state.unsupported_exportable_creations();
                let records = state
                    .inspect()
                    .map_err(|_| "cannot inspect current CUDA state")?;
                Ok(Reply::Inspection {
                    entries: records,
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn disconnected_activation_retains_listener_for_unlocked_cleanup() {
        let directory =
            std::env::temp_dir().join(format!("cuinterpose-activation-{}", std::process::id()));
        std::fs::create_dir(&directory).unwrap();
        let endpoint = directory.join("control.sock");
        let endpoint = endpoint.to_str().unwrap();
        let (activation, receiver) = mpsc::sync_channel(1);
        drop(receiver);
        let mut workers = PreparedWorkers {
            activation,
            listener: None,
        };
        assert!(workers.activate(endpoint).is_err());
        assert!(workers.listener.is_some());
        workers.cleanup(endpoint);
        assert!(!std::path::Path::new(endpoint).exists());
        assert!(workers.listener.is_none());
        std::fs::remove_dir(directory).unwrap();
    }
}
