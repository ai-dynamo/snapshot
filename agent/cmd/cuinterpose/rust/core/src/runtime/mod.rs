// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Process runtime publication, startup, and failure state.

mod control;

use crate::driver::{CudaError, Result};
use crate::memory::{ProcessState, sharing};
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
    let runtime = RUNTIME.get().ok_or(CUDA_ERROR_NOT_INITIALIZED)?;
    // Check ownership before touching any mutex inherited from another process.
    if runtime.pid != unsafe { libc::getpid() } {
        return Err(CUDA_ERROR_NOT_INITIALIZED.into());
    }
    Ok(runtime)
}

pub fn ready() -> Result<()> {
    process_runtime()?;
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return Err(CUDA_ERROR_NOT_READY.into());
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
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    thread_local! {
        // Non-Drop TLS: initialize this module's TLS before the commit lock,
        // without registering a destructor with the dynamic loader.
        static PREPARING: Cell<bool> = const { Cell::new(false) };
    }
    if PREPARING.replace(true) {
        return Err(CUDA_ERROR_NOT_INITIALIZED.into());
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
    let installing = INSTALL_LOCK.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
    let result = install_runtime(&mut candidate);
    if result.is_err() {
        RUNTIME_FAILED.store(true, Ordering::Release);
    }
    drop(installing);
    // Cancel/destroy private workers and failed listeners after releasing the
    // installation mutex. JoinHandle was detached when each spawn returned.
    drop(candidate);
    result
}

// Called with INSTALL_LOCK held. Borrow the candidate so even an early return
// leaves its cleanup to initialize(), after the installation lock is released.
fn install_runtime(candidate: &mut Result<Option<RuntimeCandidate>>) -> Result<()> {
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return Err(CUDA_ERROR_UNKNOWN.into());
    }
    // A private candidate is dispensable once a healthy runtime exists.
    // Never hide installed failure, or wait for an unfinished preparer.
    if initialized() {
        return Ok(());
    }
    let Some(candidate) = candidate.as_mut().map_err(|error| *error)? else {
        return Ok(());
    };
    let runtime = candidate.runtime.as_mut().unwrap();
    candidate.workers.activate(
        runtime
            .socket_path
            .to_str()
            .ok_or(CUDA_ERROR_INVALID_VALUE)?,
    )?;
    // Installation is serialized; OnceLock only publishes, never runs startup.
    if RUNTIME.set(*candidate.runtime.take().unwrap()).is_err() {
        unreachable!("runtime installed under installation lock");
    }
    Ok(())
}

struct RuntimeCandidate {
    // Taken only when ownership transfers to RUNTIME_PTR; losers retain cleanup.
    runtime: Option<Box<ProcessRuntime>>,
    workers: control::PreparedWorkers,
}

impl RuntimeCandidate {
    fn prepare() -> Result<Option<Self>> {
        let mut runtime = prepare_runtime()?;
        // None means another runtime won before we needed further workers.
        if initialized() {
            return Ok(None);
        }
        let namespace_pid = runtime
            .state
            .get_mut()
            .map_err(|_| CUDA_ERROR_UNKNOWN)?
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
    let namespace_pid = NamespacePid::try_from(pid).map_err(|_| CUDA_ERROR_INVALID_VALUE)?;
    let directory =
        std::env::var("SNAPSHOT_CONTROL_DIR").unwrap_or_else(|_| "/snapshot-control".into());
    if !directory.starts_with('/') {
        return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
    }
    let control_dir = PathBuf::from(directory);
    let socket_path = cuinterpose_protocol::socket_path(&control_dir, namespace_pid);
    std::os::unix::net::SocketAddr::from_pathname(&socket_path)
        .map_err(|_| CUDA_ERROR_INVALID_VALUE)?;
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
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    let state = runtime.state.lock().map_err(|_| CUDA_ERROR_UNKNOWN)?;
    // The peer service may have failed while this caller waited for the lock.
    // Check again before admitting work against the runtime.
    if RUNTIME_FAILED.load(Ordering::Acquire) {
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    Ok(state)
}
