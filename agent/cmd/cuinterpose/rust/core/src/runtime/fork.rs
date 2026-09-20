// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Atfork locks metadata, not every CUDA/resolver call. The caller must quiesce
//! CUDA and lifecycle operations before fork, as with the C implementation.

use super::{INSTALL_LOCK, RUNTIME_FAILED, RUNTIME_PTR};
use crate::memory::{ProcessState, sharing};
use std::os::fd::{AsRawFd, RawFd};
use std::sync::{
    Mutex, MutexGuard,
    atomic::{AtomicPtr, Ordering},
};

static SHIM_SOCKETS: Mutex<Vec<RawFd>> = Mutex::new(Vec::new());
// Release publishes the complete atfork inventory; each AcqRel swap takes it
// exactly once in the parent or child, which have separate post-fork memory.
static FORK_SNAPSHOT: AtomicPtr<Snapshot> = AtomicPtr::new(std::ptr::null_mut());

struct Snapshot {
    // Release the registry before state/cache/initialization in the parent.
    sockets: Option<MutexGuard<'static, Vec<RawFd>>>,
    state: ForkState,
    descriptors: Vec<RawFd>,
}

/// Open/register and unregister/close share one short registry lock, so the
/// child cannot inherit a shim socket missing from the atfork FD inventory.
pub struct Socket<T: AsRawFd>(Option<T>);

impl<T: AsRawFd> Socket<T> {
    pub fn open(open: impl FnOnce() -> std::io::Result<T>) -> std::io::Result<Self> {
        let mut sockets = SHIM_SOCKETS.lock().expect("runtime lock poisoned");
        let socket = open()?;
        sockets.push(socket.as_raw_fd());
        Ok(Self(Some(socket)))
    }
}

impl<T: AsRawFd> std::ops::Deref for Socket<T> {
    type Target = T;
    fn deref(&self) -> &T {
        self.0.as_ref().expect("live registered socket")
    }
}

impl<T: AsRawFd> Drop for Socket<T> {
    fn drop(&mut self) {
        let mut sockets = SHIM_SOCKETS.lock().expect("runtime lock poisoned");
        if let Some(socket) = self.0.take() {
            sockets.retain(|fd| *fd != socket.as_raw_fd());
            drop(socket);
        }
    }
}

pub unsafe extern "C" fn prepare() {
    let result = std::panic::catch_unwind(|| {
        let mut descriptors = Vec::new();
        // Runtime order: initialization -> STATE -> cache -> socket registry.
        // Peer EXPORT never needs STATE; waiting for its send cannot deadlock a
        // worker waiting for a creator while holding its local STATE.
        let state = fork_lock(&mut descriptors);
        let sockets = SHIM_SOCKETS.lock().expect("runtime lock poisoned");
        descriptors.extend(sockets.iter().copied());
        FORK_SNAPSHOT.store(
            Box::into_raw(Box::new(Snapshot {
                state,
                sockets: Some(sockets),
                descriptors,
            })),
            Ordering::Release,
        );
    });
    if result.is_err() {
        unsafe { libc::_exit(127) };
    }
}

pub unsafe extern "C" fn parent() {
    let snapshot = FORK_SNAPSHOT.swap(std::ptr::null_mut(), Ordering::AcqRel);
    if !snapshot.is_null() {
        drop(unsafe { Box::from_raw(snapshot) });
    }
}

pub unsafe extern "C" fn child() {
    let snapshot = FORK_SNAPSHOT.swap(std::ptr::null_mut(), Ordering::AcqRel);
    if !snapshot.is_null() {
        let snapshot = unsafe { &mut *snapshot };
        for fd in &snapshot.descriptors {
            unsafe { libc::syscall(libc::SYS_close, *fd) };
        }
        // The surviving thread acquired these two process-lifetime guards.
        // Clear before unlocking: a nested fork must not close a reused app FD.
        if let Some(mut sockets) = snapshot.sockets.take() {
            sockets.clear();
        }
        snapshot.state.abandon();
        // The snapshot allocation is intentionally abandoned in the child.
    }
}

struct ForkState {
    // Field order releases locks in reverse acquisition order in the parent.
    export_cache: Option<MutexGuard<'static, sharing::Exports>>,
    state: Option<MutexGuard<'static, ProcessState>>,
    initializing: Option<MutexGuard<'static, ()>>,
}

impl ForkState {
    fn abandon(&mut self) {
        // These guards protect CUDA state belonging to the parent's runtime.
        // The child must neither unlock nor drop that state through Rust/CUDA.
        std::mem::forget(self.state.take());
        std::mem::forget(self.export_cache.take());
        drop(self.initializing.take());
        RUNTIME_PTR.store(std::ptr::null_mut(), Ordering::Release);
        RUNTIME_FAILED.store(false, Ordering::Release);
    }
}

fn fork_lock(descriptors: &mut Vec<i32>) -> ForkState {
    let initializing = INSTALL_LOCK.lock().expect("runtime lock poisoned");
    let pointer = RUNTIME_PTR.load(Ordering::Acquire);
    if pointer.is_null() {
        return ForkState {
            initializing: Some(initializing),
            state: None,
            export_cache: None,
        };
    }
    let runtime = unsafe { &*pointer };
    let state = runtime.state.lock().expect("runtime lock poisoned");
    let cache = runtime.export_cache.fork_lock(descriptors);
    ForkState {
        initializing: Some(initializing),
        state: Some(state),
        export_cache: Some(cache),
    }
}
