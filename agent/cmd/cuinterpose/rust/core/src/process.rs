// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Atfork locks metadata, not every CUDA/resolver call. The caller must quiesce
//! CUDA and lifecycle operations before fork, as with the C implementation.

use std::os::fd::{AsRawFd, RawFd};
use std::sync::{
    Mutex, MutexGuard,
    atomic::{AtomicPtr, Ordering},
};

static G_SOCKETS: Mutex<Vec<RawFd>> = Mutex::new(Vec::new());
// Release publishes the complete atfork inventory; each AcqRel swap takes it
// exactly once in the parent or child, which have separate post-fork memory.
static G_SNAPSHOT: AtomicPtr<Snapshot> = AtomicPtr::new(std::ptr::null_mut());

struct Snapshot {
    // Release the registry before state/cache/initialization in the parent.
    sockets: Option<MutexGuard<'static, Vec<RawFd>>>,
    state: super::state::ForkState,
    descriptors: Vec<RawFd>,
}

/// Open/register and unregister/close share one short registry lock, so the
/// child cannot inherit a shim socket missing from the atfork FD inventory.
pub struct Socket<T: AsRawFd>(Option<T>);

impl<T: AsRawFd> Socket<T> {
    pub fn open(open: impl FnOnce() -> std::io::Result<T>) -> std::io::Result<Self> {
        let mut sockets = G_SOCKETS.lock().unwrap_or_else(|e| e.into_inner());
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

impl<T: AsRawFd> std::ops::DerefMut for Socket<T> {
    fn deref_mut(&mut self) -> &mut T {
        self.0.as_mut().expect("live registered socket")
    }
}

impl<T: AsRawFd> Drop for Socket<T> {
    fn drop(&mut self) {
        let mut sockets = G_SOCKETS.lock().unwrap_or_else(|e| e.into_inner());
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
        // Peer EXPORT never needs STATE; draining its leases cannot deadlock a
        // worker waiting for a creator while holding its local STATE.
        let state = super::state::fork_lock(&mut descriptors);
        let sockets = G_SOCKETS.lock().unwrap_or_else(|e| e.into_inner());
        descriptors.extend(sockets.iter().copied());
        G_SNAPSHOT.store(
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
    let snapshot = G_SNAPSHOT.swap(std::ptr::null_mut(), Ordering::AcqRel);
    if !snapshot.is_null() {
        drop(unsafe { Box::from_raw(snapshot) });
    }
}

pub unsafe extern "C" fn child() {
    let snapshot = G_SNAPSHOT.swap(std::ptr::null_mut(), Ordering::AcqRel);
    if !snapshot.is_null() {
        let snapshot = unsafe { &mut *snapshot };
        for fd in &snapshot.descriptors {
            unsafe { libc::syscall(libc::SYS_close, *fd) };
        }
        if let Some(arena) = snapshot
            .state
            .state
            .as_ref()
            .and_then(|state| state.arena.as_ref())
        {
            unsafe { libc::syscall(libc::SYS_munmap, arena.base, arena.size) };
        }
        // Abandon CUDA-bearing state/cache and their locked mutexes. Fresh child
        // activity allocates a new generation; no inherited CUDA Drop runs.
        std::mem::forget(snapshot.state.state.take());
        std::mem::forget(snapshot.state.cache.take());
        // The surviving thread acquired these two process-lifetime guards.
        // Clear before unlocking: a nested fork must not close a reused app FD.
        if let Some(mut sockets) = snapshot.sockets.take() {
            sockets.clear();
        }
        drop(snapshot.state.initializing.take());
        // The snapshot allocation is intentionally abandoned in the child.
    }
    super::state::fork_child();
}
