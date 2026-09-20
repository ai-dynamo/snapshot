// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Two prestarted workers separate peer FD service from serialized CUDA control.
//! Queue pressure refuses requests before mutation; no operation is retried.

use crate::driver::Result;
use crate::memory::checkpoint;
use crate::runtime as state;
use cudarc::driver::sys::CUresult::CUDA_ERROR_NOT_INITIALIZED;
use cuinterpose_protocol::{self as protocol, NamespacePid, Operation, Request, Response};
use rustix::event::{PollFd, PollFlags, poll};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::atomic::Ordering;
use std::sync::mpsc::{self, TrySendError};

// One running control operation and at most eight waiting connections. Peer
// exports never enter this queue: reciprocal importers need them to progress.
const CONTROL_QUEUE_CAPACITY: usize = 8;

enum ControlRequest {
    BeginCheckpoint,
    Inspect,
    Execute(Operation),
}

/// Private workers cannot dispatch until the single listener handoff succeeds.
/// Dropping this owner cancels them without joining: a caller may hold the
/// loader lock needed by a worker's Rust TLS startup or teardown.
pub struct PreparedWorkers {
    activation: mpsc::SyncSender<UnixListener>,
    // Owned only between a successful bind and the worker handoff.
    listener: Option<UnixListener>,
}

impl PreparedWorkers {
    pub fn prepare(namespace_pid: NamespacePid) -> Result<Option<Self>> {
        let (sender, receiver) =
            mpsc::sync_channel::<(UnixStream, ControlRequest)>(CONTROL_QUEUE_CAPACITY);
        let (activation, parked) = mpsc::sync_channel::<UnixListener>(1);
        let _worker = std::thread::Builder::new()
            .name("cuinterpose-control".into())
            .spawn(move || {
                while let Ok((socket, request)) = receiver.recv() {
                    if !super::RUNTIME_FAILED.load(Ordering::Acquire) {
                        let _ = serve(socket, request, namespace_pid);
                    }
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
                    let mut events = [PollFd::new(&listener, PollFlags::IN)];
                    match poll(&mut events, None) {
                        Err(rustix::io::Errno::INTR) => continue,
                        Ok(_) if events[0].revents() == PollFlags::IN => {}
                        _ => {
                            super::RUNTIME_FAILED.store(true, Ordering::Release);
                            break;
                        }
                    }
                    let socket = match listener.accept() {
                        Ok((socket, _)) => socket,
                        Err(error)
                            if matches!(
                                error.kind(),
                                std::io::ErrorKind::Interrupted
                                    | std::io::ErrorKind::WouldBlock
                                    | std::io::ErrorKind::ConnectionAborted
                            ) =>
                        {
                            continue;
                        }
                        Err(error)
                            if matches!(
                                error.raw_os_error(),
                                Some(libc::EMFILE | libc::ENFILE | libc::ENOBUFS | libc::ENOMEM)
                            ) =>
                        {
                            // A queued connection stays readable while resources are exhausted.
                            std::thread::sleep(std::time::Duration::from_millis(50));
                            continue;
                        }
                        Err(_) => {
                            super::RUNTIME_FAILED.store(true, Ordering::Release);
                            break;
                        }
                    };
                    if !super::RUNTIME_FAILED.load(Ordering::Acquire) {
                        let _ = dispatch(socket, namespace_pid, &sender);
                    }
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
    /// here. The caller holds the runtime installation lock.
    /// With pinned Rust/glibc, mutexes and try_send wakeups use futexes and
    /// non-Drop TLS, not loader registration. The channel is preallocated.
    /// Eager ELF binding prevents first-use loader lookup in these libc calls.
    pub fn activate(&mut self, endpoint: &str) -> Result<()> {
        self.listener = Some(UnixListener::bind(endpoint).map_err(|_| CUDA_ERROR_NOT_INITIALIZED)?);
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
    socket: UnixStream,
    namespace_pid: NamespacePid,
    sender: &mpsc::SyncSender<(UnixStream, ControlRequest)>,
) -> protocol::Result<()> {
    // Classification uses per-I/O socket timeouts, not a total header deadline.
    // A slow peer can delay acceptance, but never waits on STATE or lifecycle
    // CUDA calls.
    let timeout = Some(cuinterpose_protocol::timeout(None));
    socket.set_read_timeout(timeout)?;
    socket.set_write_timeout(timeout)?;
    let (request, descriptor): (Request, _) = protocol::receive(&socket)?;
    let addressed = match &request {
        Request::BeginCheckpoint {
            namespace_pid: target,
        }
        | Request::Inspect {
            namespace_pid: target,
        }
        | Request::Execute {
            namespace_pid: target,
            ..
        } => *target == namespace_pid,
        Request::Export { allocation } => allocation.creator_pid == namespace_pid,
    };
    if descriptor.is_some() || !addressed {
        return refuse(
            &socket,
            namespace_pid,
            "invalid cuinterpose control request",
        );
    }
    let request = match request {
        Request::BeginCheckpoint { .. } => ControlRequest::BeginCheckpoint,
        Request::Inspect { .. } => ControlRequest::Inspect,
        Request::Execute { operation, .. } => ControlRequest::Execute(operation),
        Request::Export { allocation } => {
            if super::RUNTIME_FAILED.load(Ordering::Acquire) {
                return refuse(&socket, namespace_pid, "cuinterpose state failed");
            }
            let cache = match state::export_cache() {
                Ok(cache) => cache,
                Err(_) => {
                    return refuse(&socket, namespace_pid, "creator resource is unavailable");
                }
            };
            return cache.send(&socket, namespace_pid, &allocation.id);
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
            refuse(&socket, namespace_pid, message)
        }
    }
}

fn refuse(socket: &UnixStream, namespace_pid: NamespacePid, message: &str) -> protocol::Result<()> {
    protocol::send(
        socket,
        &Response {
            namespace_pid,
            result: Err(message.into()),
        },
        None,
    )
}

fn serve(
    socket: UnixStream,
    request: ControlRequest,
    namespace_pid: NamespacePid,
) -> protocol::Result<()> {
    let result = match request {
        ControlRequest::BeginCheckpoint => checkpoint::begin(),
        ControlRequest::Inspect => checkpoint::inspect(),
        ControlRequest::Execute(operation) => checkpoint::execute(operation),
    };
    protocol::send(
        &socket,
        &Response {
            namespace_pid,
            result,
        },
        None,
    )?;
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
