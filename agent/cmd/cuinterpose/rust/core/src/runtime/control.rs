// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Peer FD requests use a separate worker from serialized CUDA control so imports can
//! continue while a control operation waits. A full queue rejects requests before
//! mutation, and operations are never retried.

use crate::error::{Error, Result};
use crate::memory::checkpoint;
use crate::runtime as state;
use cuinterpose_protocol::{self as protocol, NamespacePid, Operation, Request, Response};
use rustix::event::{PollFd, PollFlags, poll};
use rustix::net::{AddressFamily, SocketAddrUnix, SocketFlags, SocketType, socket_with};
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::atomic::Ordering;
use std::sync::mpsc::{self, TrySendError};

// Allow one running control operation and at most eight waiting connections. Peer
// exports bypass this queue because importers can wait for each other.
const CONTROL_QUEUE_CAPACITY: usize = 8;

enum ControlRequest {
    BeginCheckpoint,
    Inspect,
    Execute(Operation),
}

/// Workers cannot handle requests until they receive the listener. Dropping this owner
/// cancels them without joining because the caller may hold the loader lock needed for
/// a worker's Rust TLS startup or cleanup.
pub struct PreparedWorkers {
    activation: mpsc::SyncSender<UnixListener>,
    // Owned after bind succeeds and until the worker receives the listener.
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
            .map_err(|error| Error::io("start control worker", error))?;
        if state::published() {
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
                        Err(rustix::io::Errno::NOMEM) => {
                            std::thread::sleep(std::time::Duration::from_millis(50));
                            continue;
                        }
                        Ok(_) if events[0].revents() == PollFlags::IN => {}
                        Err(error) => {
                            eprintln!("cuinterpose: listener poll failed: {error}");
                            super::RUNTIME_FAILED.store(true, Ordering::Release);
                            break;
                        }
                        Ok(_) => {
                            eprintln!(
                                "cuinterpose: unexpected listener poll events: {:?}",
                                events[0].revents()
                            );
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
                        Err(error) => {
                            eprintln!("cuinterpose: listener accept failed: {error}");
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
            // If thread creation fails, dropping the closure closes the only
            // control-queue sender.
            return Err(Error::io("start peer worker", error));
        }
        Ok(Some(Self {
            activation,
            listener: None,
        }))
    }

    /// Because the caller holds the runtime installation lock, this method must not
    /// spawn threads, block on channels, format values, or call callbacks. The pinned
    /// Rust standard library uses futexes and non-Drop TLS here. The workload supplies
    /// glibc and its loader. Preparation primes backend TLS, the channel is preallocated,
    /// and eager ELF binding resolves libc symbols before these calls run.
    pub fn activate(&mut self, endpoint: &str) -> Result<()> {
        self.listener =
            Some(bind_listener(endpoint).map_err(|error| Error::io("bind control socket", error))?);
        let listener = self.listener.as_ref().unwrap();
        // The socket cannot accept connections before listen, so its permissions can be
        // restricted first without changing the application's process umask.
        std::fs::set_permissions(endpoint, std::fs::Permissions::from_mode(0o600))
            .map_err(|error| Error::io("set control socket permissions", error))?;
        rustix::net::listen(listener, libc::SOMAXCONN)
            .map_err(|error| Error::io("listen on control socket", error))?;
        match self.activation.try_send(self.listener.take().unwrap()) {
            Ok(()) => Ok(()),
            Err(TrySendError::Full(listener) | TrySendError::Disconnected(listener)) => {
                self.listener = Some(listener);
                Err(Error::Startup("control listener handoff failed"))
            }
        }
    }

    pub fn cleanup(&mut self, endpoint: &str) {
        // This owner cleans up only successfully bound endpoints that were not
        // published, with cleanup kept outside the installation lock.
        if let Some(listener) = self.listener.take() {
            drop(listener);
            let _ = std::fs::remove_file(endpoint);
        }
    }
}

fn bind_listener(endpoint: &str) -> std::io::Result<UnixListener> {
    let address = SocketAddrUnix::new(endpoint)?;
    let listener = socket_with(
        AddressFamily::UNIX,
        SocketType::STREAM,
        SocketFlags::NONBLOCK | SocketFlags::CLOEXEC,
        None,
    )?;
    let error = match rustix::net::bind(&listener, &address) {
        Ok(()) => return Ok(listener.into()),
        Err(error @ rustix::io::Errno::ADDRINUSE) => std::io::Error::from(error),
        Err(error) => return Err(error.into()),
    };
    // Exec closes the listener but leaves its path, so a later installer may need to
    // remove a stale endpoint. Only the installer holding INSTALL_LOCK may do this,
    // after checking for a healthy runtime and only for this PID's endpoint. Other PID
    // namespaces must use different names.
    let previous = std::fs::symlink_metadata(endpoint)?;
    let uid = unsafe { libc::geteuid() };
    if !previous.file_type().is_socket() || previous.uid() != uid {
        return Err(error);
    }
    let probe = socket_with(
        AddressFamily::UNIX,
        SocketType::STREAM,
        SocketFlags::NONBLOCK | SocketFlags::CLOEXEC,
        None,
    )?;
    // No polling or protocol exchange can run under the installation lock. A full
    // backlog (EAGAIN), an active listener, or an ambiguous error does not establish
    // that the endpoint is stale, so its path must be preserved.
    if rustix::net::connect(&probe, &address) != Err(rustix::io::Errno::CONNREFUSED) {
        return Err(error);
    }
    let current = std::fs::symlink_metadata(endpoint)?;
    if current.dev() != previous.dev()
        || current.ino() != previous.ino()
        || !current.file_type().is_socket()
        || current.uid() != uid
    {
        return Err(error);
    }
    // The identity check and unlink are not atomic. INSTALL_LOCK serializes only
    // this process, so another process must not replace this PID-owned endpoint
    // between the check and removal.
    std::fs::remove_file(endpoint)?;
    rustix::net::bind(&listener, &address)?;
    Ok(listener.into())
}

fn dispatch(
    socket: UnixStream,
    namespace_pid: NamespacePid,
    sender: &mpsc::SyncSender<(UnixStream, ControlRequest)>,
) -> protocol::Result<()> {
    // The timeout applies to each socket read rather than the whole header, so a slow
    // peer can delay acceptance. Classification never waits for ProcessState or
    // lifecycle CUDA calls.
    let timeout = Some(cuinterpose_protocol::control_timeout());
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
    let loaded =
        matches!(request, ControlRequest::Execute(Operation::LoadAllocations)) && result.is_ok();
    protocol::send(
        &socket,
        &Response {
            namespace_pid,
            result,
        },
        None,
    )?;
    if loaded {
        checkpoint::load_acknowledged();
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bound_socket_cannot_queue_connections_before_listen() {
        let directory =
            std::env::temp_dir().join(format!("cuinterpose-bound-{}", std::process::id()));
        std::fs::create_dir(&directory).unwrap();
        let endpoint = directory.join("control.sock");
        let endpoint = endpoint.to_str().unwrap();
        let listener = bind_listener(endpoint).unwrap();
        // Changing this process's umask could affect files created by concurrent tests,
        // so model a permissive application umask without changing the process setting.
        std::fs::set_permissions(endpoint, std::fs::Permissions::from_mode(0o777)).unwrap();
        assert_eq!(
            UnixStream::connect(endpoint).unwrap_err().kind(),
            std::io::ErrorKind::ConnectionRefused
        );
        drop(listener);
        std::fs::remove_file(endpoint).unwrap();
        std::fs::remove_dir(directory).unwrap();
    }

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
