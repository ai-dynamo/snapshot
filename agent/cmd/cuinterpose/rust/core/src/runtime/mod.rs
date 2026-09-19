// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Process-generation publication, startup, and failure state.

mod control;
pub(crate) mod fork;

use crate::driver::{CudaError, Result};
use crate::memory::{ProcessState, checkpoint::Phase, sharing};
use cudarc::driver::sys::CUresult::*;
use cuinterpose_protocol::NamespacePid;
use std::cell::Cell;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicPtr, Ordering};
use std::sync::{Mutex, MutexGuard};

pub(crate) static G_FAILED: AtomicBool = AtomicBool::new(false);

struct Generation {
    state: Mutex<ProcessState>,
    cache: sharing::ExportCache,
    control_dir: PathBuf,
    socket_path: PathBuf,
}
// Release publication follows successful worker startup; Acquire readers may
// then borrow the generation for its process lifetime. Only a quiescent fork
// child abandons the inherited pointer; it never frees the parent's generation.
static G_STATE: AtomicPtr<Generation> = AtomicPtr::new(std::ptr::null_mut());
static G_INITIALIZING: Mutex<()> = Mutex::new(());
static G_CHILD: AtomicBool = AtomicBool::new(false);

pub fn cache() -> Result<&'static sharing::ExportCache> {
    let pointer = G_STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return Err(CudaError::from(CUDA_ERROR_NOT_INITIALIZED));
    }
    Ok(&unsafe { &*pointer }.cache)
}

pub fn control_dir() -> Result<&'static Path> {
    let pointer = G_STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return Err(CudaError::from(CUDA_ERROR_NOT_INITIALIZED));
    }
    Ok(&unsafe { &*pointer }.control_dir)
}

pub(super) fn initialized() -> bool {
    !G_STATE.load(Ordering::Acquire).is_null()
}

pub fn initialize() -> Result<()> {
    if G_FAILED.load(Ordering::Acquire) {
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    if initialized() {
        return Ok(());
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
    let installing = match G_INITIALIZING.lock() {
        Ok(guard) => guard,
        Err(poison) if G_CHILD.load(Ordering::Acquire) => poison.into_inner(),
        Err(_) => return Err(CUDA_ERROR_UNKNOWN.into()),
    };
    let result = install_generation(&mut candidate);
    if result.is_err() {
        G_FAILED.store(true, Ordering::Release);
    }
    drop(installing);
    // Cancel/destroy private workers and failed listeners after releasing the
    // installation mutex. JoinHandle was detached when each spawn returned.
    drop(candidate);
    result
}

// Called with G_INITIALIZING held. Borrow the candidate so even an early return
// leaves its cleanup to initialize(), after the installation lock is released.
fn install_generation(candidate: &mut Result<Option<RuntimeCandidate>>) -> Result<()> {
    if G_FAILED.load(Ordering::Acquire) {
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
    let generation = candidate.generation.as_mut().unwrap();
    candidate.workers.activate(
        generation
            .socket_path
            .to_str()
            .ok_or(CUDA_ERROR_INVALID_VALUE)?,
    )?;
    G_STATE.store(
        Box::into_raw(candidate.generation.take().unwrap()),
        Ordering::Release,
    );
    Ok(())
}

struct RuntimeCandidate {
    // Taken only when ownership transfers to G_STATE; losers retain cleanup.
    generation: Option<Box<Generation>>,
    workers: control::PreparedWorkers,
}

impl RuntimeCandidate {
    fn prepare() -> Result<Option<Self>> {
        let mut generation = prepare_generation()?;
        // None means another runtime won before we needed further workers.
        if initialized() {
            return Ok(None);
        }
        let namespace_pid = generation
            .state
            .get_mut()
            .map_err(|_| CUDA_ERROR_UNKNOWN)?
            .namespace_pid;
        let Some(workers) = control::PreparedWorkers::prepare(namespace_pid)? else {
            return Ok(None);
        };
        Ok(Some(Self {
            generation: Some(generation),
            workers,
        }))
    }
}

impl Drop for RuntimeCandidate {
    fn drop(&mut self) {
        if let Some(generation) = &mut self.generation
            && let Some(path) = generation.socket_path.to_str()
        {
            self.workers.cleanup(path);
        }
    }
}

fn prepare_generation() -> Result<Box<Generation>> {
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
    Ok(Box::new(Generation {
        state: Mutex::new(state),
        cache: sharing::ExportCache::default(),
        control_dir,
        socket_path,
    }))
}

pub fn get() -> Result<MutexGuard<'static, ProcessState>> {
    if G_FAILED.load(Ordering::Acquire) {
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    let pointer = G_STATE.load(Ordering::Acquire);
    if pointer.is_null() {
        return Err(CudaError::from(CUDA_ERROR_NOT_INITIALIZED));
    }
    let state = unsafe { &*pointer }
        .state
        .lock()
        .map_err(|_| CUDA_ERROR_UNKNOWN)?;
    // A caller may have waited behind a failed lifecycle operation. Do not
    // admit queued mutations using only the pre-lock check.
    if G_FAILED.load(Ordering::Acquire) {
        return Err(CudaError::from(CUDA_ERROR_UNKNOWN));
    }
    Ok(state)
}

pub(super) fn active() -> Result<MutexGuard<'static, ProcessState>> {
    let state = get()?;
    if state.phase != Phase::Active {
        return Err(CudaError::from(CUDA_ERROR_NOT_READY));
    }
    Ok(state)
}
