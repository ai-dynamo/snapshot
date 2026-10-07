// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Process runtime initialization, publication, and failure state.

mod control;

use crate::error::{Error, Result};
use crate::memory::{ProcessState, checkpoint::Phase, sharing};
use cudarc::driver::sys::CUresult::*;
use cuinterpose_protocol::NamespacePid;
use std::cell::Cell;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Mutex, MutexGuard, OnceLock};

pub(crate) static RUNTIME_FAILED: AtomicBool = AtomicBool::new(false);

struct ProcessRuntime {
    pid: libc::pid_t,
    state: Mutex<ProcessState>,
    export_cache: sharing::ExportCache,
    socket_dir: PathBuf,
    socket_path: PathBuf,
}
// Once published after CUDA initialization, this runtime belongs to its creating
// process and must never be reset or reused in a fork child.
static RUNTIME: OnceLock<ProcessRuntime> = OnceLock::new();
static INSTALL_LOCK: Mutex<()> = Mutex::new(());

fn process_runtime() -> Result<&'static ProcessRuntime> {
    let runtime = RUNTIME
        .get()
        .ok_or(Error::Startup("runtime is not initialized"))?;
    // Check ownership before touching any mutex inherited from another process.
    if runtime.pid != unsafe { libc::getpid() } {
        return Err(Error::Startup("runtime belongs to another process"));
    }
    Ok(runtime)
}

pub fn ready() -> Result<()> {
    process_runtime()?;
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return Err(Error::RuntimeFailed);
    }
    Ok(())
}

pub fn export_cache() -> Result<&'static sharing::ExportCache> {
    Ok(&process_runtime()?.export_cache)
}

pub fn socket_dir() -> Result<&'static Path> {
    Ok(&process_runtime()?.socket_dir)
}

fn published() -> bool {
    RUNTIME.get().is_some()
}

pub fn initialize() -> Result<()> {
    if published() {
        return ready();
    }
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return Err(Error::RuntimeFailed);
    }
    thread_local! {
        // A thread-local guard rejects reentry without making other threads wait to
        // prepare runtime candidates. A global guard could deadlock with the loader,
        // while Cell<bool> also avoids TLS destructors and loader registration.
        static PREPARING: Cell<bool> = const { Cell::new(false) };
    }
    if PREPARING.replace(true) {
        return Err(Error::Startup("recursive runtime initialization"));
    }
    struct Reset;
    impl Drop for Reset {
        fn drop(&mut self) {
            PREPARING.set(false);
        }
    }
    let _reset = Reset;
    // Do not hold the installation lock while creating threads or registering TLS. A
    // constructor that holds the loader lock can prepare its own runtime candidate
    // while another caller waits in Rust's thread startup.
    let mut candidate = RuntimeCandidate::prepare();
    {
        let _installation = INSTALL_LOCK
            .lock()
            .map_err(|_| Error::Startup("installation mutex poisoned"))?;
        // Use an already published healthy runtime even if this candidate failed.
        let result = if RUNTIME_FAILED.load(Ordering::Acquire) {
            Err(Error::RuntimeFailed)
        } else if published() {
            Ok(())
        } else {
            match candidate {
                Ok(Some(ref mut candidate)) => install_runtime(candidate),
                Ok(None) => Ok(()),
                Err(error) => Err(error),
            }
        };
        if result.is_err() {
            RUNTIME_FAILED.store(true, Ordering::Release);
        }
        result
    }
    // Release the installation lock before dropping unused workers and failed
    // listeners.
}

// Borrowing the candidate under INSTALL_LOCK keeps its cleanup outside the installation
// mutex.
fn install_runtime(candidate: &mut RuntimeCandidate) -> Result<()> {
    let runtime = candidate.runtime.as_mut().unwrap();
    candidate.workers.activate(
        runtime
            .socket_path
            .to_str()
            .ok_or(Error::Startup("control socket path is not UTF-8"))?,
    )?;
    // The installation lock serializes installation. OnceLock only publishes the
    // runtime.
    if RUNTIME.set(*candidate.runtime.take().unwrap()).is_err() {
        unreachable!("runtime installed under installation lock");
    }
    Ok(())
}

struct RuntimeCandidate {
    // Ownership moves out of the candidate only when the runtime is published in
    // RUNTIME, leaving unused candidates responsible for cleanup.
    runtime: Option<Box<ProcessRuntime>>,
    workers: control::PreparedWorkers,
}

impl RuntimeCandidate {
    fn prepare() -> Result<Option<Self>> {
        crate::driver::initialize()?;
        let mut runtime = prepare_runtime()?;
        // None means another caller published the runtime before more workers were
        // needed.
        if published() {
            return Ok(None);
        }
        let namespace_pid = runtime
            .state
            .get_mut()
            .map_err(|_| Error::Startup("CUDA state mutex poisoned"))?
            .namespace_pid;
        let Some(workers) = control::PreparedWorkers::prepare(namespace_pid)? else {
            return Ok(None);
        };
        Ok(Some(Self {
            runtime: Some(runtime),
            workers,
        }))
    }
}

impl Drop for RuntimeCandidate {
    fn drop(&mut self) {
        if let Some(runtime) = &mut self.runtime
            && let Some(path) = runtime.socket_path.to_str()
        {
            self.workers.cleanup(path);
        }
    }
}

fn prepare_runtime() -> Result<Box<ProcessRuntime>> {
    let pid = unsafe { libc::getpid() };
    let namespace_pid =
        NamespacePid::try_from(pid).map_err(|_| Error::Startup("invalid namespace PID"))?;
    // Endpoints live in the container's /tmp, so they need no Snapshot volume. Tests
    // override the directory to isolate concurrent runs.
    let directory = std::env::var("CUINTERPOSE_SOCKET_DIR").unwrap_or_else(|_| "/tmp".into());
    if !directory.starts_with('/') {
        return Err(Error::Startup("socket directory must be absolute"));
    }
    let socket_dir = PathBuf::from(directory);
    let socket_path = cuinterpose_protocol::socket_path(&socket_dir, namespace_pid);
    std::os::unix::net::SocketAddr::from_pathname(&socket_path)
        .map_err(|error| Error::io("create control socket address", error))?;
    let state = ProcessState::new(namespace_pid);
    Ok(Box::new(ProcessRuntime {
        pid,
        state: Mutex::new(state),
        export_cache: sharing::ExportCache::default(),
        socket_dir,
        socket_path,
    }))
}

pub fn get() -> Result<MutexGuard<'static, ProcessState>> {
    let runtime = process_runtime()?;
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return Err(Error::RuntimeFailed);
    }
    let state = runtime
        .state
        .lock()
        .map_err(|_| Error::Startup("CUDA state mutex poisoned"))?;
    // The peer service may have failed while this caller waited for the lock. Check
    // again before allowing runtime operations.
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return Err(Error::RuntimeFailed);
    }
    Ok(state)
}

/// LOAD already finished using the carrier. Release it even if the peer worker
/// subsequently failed, while retaining the ownership check before taking the mutex.
pub(crate) fn release_host_arena() {
    if let Ok(runtime) = process_runtime()
        && let Ok(mut state) = runtime.state.lock()
        && let Some(arena) = state.arena.take()
    {
        must_complete(arena.release());
    }
}

#[cfg(test)]
pub(crate) fn install_for_test(state: ProcessState) {
    let mut runtime = prepare_runtime().unwrap();
    runtime.state = Mutex::new(state);
    assert!(RUNTIME.set(*runtime).is_ok());
}

pub(super) fn active() -> Result<MutexGuard<'static, ProcessState>> {
    let state = get()?;
    if state.phase != Phase::Active {
        return Err(Error::from(CUDA_ERROR_NOT_READY));
    }
    Ok(state)
}

/// After CUDA or tracking state changes, a failure is not recoverable. Do not return to
/// an application if its recorded state no longer matches the driver.
pub(crate) fn must_complete<T>(result: Result<T>) -> T {
    result.unwrap_or_else(|error| {
        eprintln!("cuinterpose: unrecoverable state change: {error}");
        std::process::abort();
    })
}

/// The state lock is released around a blocking driver call so other threads can make
/// progress, then reacquired before the result is recorded. The returned guard keeps
/// result recording atomic with checkpoint entry.
pub(crate) fn call_unlocked<T>(
    mut state: MutexGuard<'static, ProcessState>,
    operation: impl FnOnce() -> Result<T>,
) -> Result<(MutexGuard<'static, ProcessState>, T)> {
    state.unlocked_driver_calls += 1;
    drop(state);
    let result = operation();
    let mut state = must_complete(get());
    state.unlocked_driver_calls -= 1;
    result.map(|value| (state, value))
}
