// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Process runtime publication, startup, and failure state.

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
    control_dir: PathBuf,
    socket_path: PathBuf,
}
// Published once after CUDA initialization; never reset or reused in a fork child.
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

pub fn control_dir() -> Result<&'static Path> {
    Ok(&process_runtime()?.control_dir)
}

pub(super) fn initialized() -> bool {
    RUNTIME.get().is_some()
}

pub fn initialize() -> Result<()> {
    if initialized() {
        return ready();
    }
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return Err(Error::RuntimeFailed);
    }
    thread_local! {
        // Reject same-thread re-entry while allowing other threads to prepare
        // candidates concurrently. A global guard could deadlock with loader
        // activity. Cell<bool> needs no TLS destructor or loader registration.
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
    // Thread creation/TLS registration must never own process-wide installation
    // exclusion. A constructor holding the loader lock can prepare its own
    // candidate while a different caller waits in Rust's spawn hooks.
    let mut candidate = RuntimeCandidate::prepare();
    {
        let _installation = INSTALL_LOCK
            .lock()
            .map_err(|_| Error::Startup("installation mutex poisoned"))?;
        // A healthy winner supersedes even a failed private candidate.
        let result = if RUNTIME_FAILED.load(Ordering::Acquire) {
            Err(Error::RuntimeFailed)
        } else if initialized() {
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
    // The installation guard drops before private workers and failed listeners.
}

// Borrow under INSTALL_LOCK so cleanup stays outside the installation mutex.
fn install_runtime(candidate: &mut RuntimeCandidate) -> Result<()> {
    let runtime = candidate.runtime.as_mut().unwrap();
    candidate.workers.activate(
        runtime
            .socket_path
            .to_str()
            .ok_or(Error::Startup("control socket path is not UTF-8"))?,
    )?;
    // Installation is serialized; OnceLock only publishes, never runs startup.
    if RUNTIME.set(*candidate.runtime.take().unwrap()).is_err() {
        unreachable!("runtime installed under installation lock");
    }
    Ok(())
}

struct RuntimeCandidate {
    // Taken only when ownership transfers to RUNTIME; losers retain cleanup.
    runtime: Option<Box<ProcessRuntime>>,
    workers: control::PreparedWorkers,
}

impl RuntimeCandidate {
    fn prepare() -> Result<Option<Self>> {
        crate::driver::initialize();
        let mut runtime = prepare_runtime()?;
        // None means another runtime won before we needed further workers.
        if initialized() {
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
    let directory =
        std::env::var("SNAPSHOT_CONTROL_DIR").unwrap_or_else(|_| "/snapshot-control".into());
    if !directory.starts_with('/') {
        return Err(Error::Startup("control directory must be absolute"));
    }
    let control_dir = PathBuf::from(directory);
    let socket_path = cuinterpose_protocol::socket_path(&control_dir, namespace_pid);
    std::os::unix::net::SocketAddr::from_pathname(&socket_path)
        .map_err(|error| Error::io("create control socket address", error))?;
    let state = ProcessState::new(namespace_pid);
    Ok(Box::new(ProcessRuntime {
        pid,
        state: Mutex::new(state),
        export_cache: sharing::ExportCache::default(),
        control_dir,
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
    // The peer service may have failed while this caller waited for the lock.
    // Check again before admitting work against the runtime.
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return Err(Error::RuntimeFailed);
    }
    Ok(state)
}

pub(super) fn active() -> Result<MutexGuard<'static, ProcessState>> {
    let state = get()?;
    if state.phase != Phase::Active {
        return Err(Error::from(CUDA_ERROR_NOT_READY));
    }
    Ok(state)
}

/// Once CUDA or tracking state has changed, failure is not recoverable. Do not
/// return to an application whose recorded state no longer matches the driver.
pub(crate) fn must_complete<T>(result: Result<T>) -> T {
    result.unwrap_or_else(|error| {
        eprintln!("cuinterpose: unrecoverable state change: {error}");
        std::process::abort();
    })
}

/// Allow other application threads to complete a blocking driver call. The
/// returned guard keeps recording its result atomic with checkpoint entry.
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
