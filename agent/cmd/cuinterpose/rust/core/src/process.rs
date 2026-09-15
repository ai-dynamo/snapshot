// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The frontend quiesces all operations before these hooks run. Prepare may
//! allocate in the parent; child uses only its immutable snapshot and syscalls.

use cuinterpose_abi::SUCCESS;
use std::os::fd::RawFd;
use std::sync::{
    Mutex,
    atomic::{AtomicBool, AtomicPtr, Ordering},
};

static SOCKETS: Mutex<Vec<RawFd>> = Mutex::new(Vec::new());
static SNAPSHOT: AtomicPtr<Snapshot> = AtomicPtr::new(std::ptr::null_mut());
static SOCKETS_ABANDONED: AtomicBool = AtomicBool::new(false);

struct Snapshot {
    descriptors: Vec<RawFd>,
    arena: Option<(usize, usize)>,
}

pub struct Guard;

impl Guard {
    pub fn enter() -> Option<Self> {
        let host = super::HOST.get()?;
        // Listener threads hold no loader/CUDA locks here. They can wait for
        // admission to reopen, unlike arbitrary application resolver callers.
        loop {
            if unsafe { (host.enter)() } == SUCCESS {
                return Some(Self);
            }
            std::thread::yield_now();
        }
    }
}

impl Drop for Guard {
    fn drop(&mut self) {
        if let Some(host) = super::HOST.get() {
            unsafe {
                (host.leave)();
            }
        }
    }
}

/// A socket may wait for a request without blocking fork. Its raw descriptor
/// stays registered until closed under the operation gate.
pub struct Socket<T: std::os::fd::AsRawFd>(Option<T>);

impl<T: std::os::fd::AsRawFd> Socket<T> {
    pub fn new(socket: T) -> Self {
        // Registration and the actual accept/open are covered by one outer
        // lease; fork cannot snapshot the interval between them.
        SOCKETS
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(socket.as_raw_fd());
        Self(Some(socket))
    }
}

impl<T: std::os::fd::AsRawFd> std::ops::Deref for Socket<T> {
    type Target = T;
    fn deref(&self) -> &T {
        self.0.as_ref().expect("live registered socket")
    }
}

impl<T: std::os::fd::AsRawFd> std::ops::DerefMut for Socket<T> {
    fn deref_mut(&mut self) -> &mut T {
        self.0.as_mut().expect("live registered socket")
    }
}

impl<T: std::os::fd::AsRawFd> Drop for Socket<T> {
    fn drop(&mut self) {
        let _guard = Guard::enter();
        let mut sockets = SOCKETS.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(socket) = self.0.take() {
            sockets.retain(|fd| *fd != socket.as_raw_fd());
            drop(socket);
        }
    }
}

pub unsafe extern "C" fn prepare() {
    // A panic terminates the process rather than unwinding through pthread_atfork
    // or continuing with an incomplete inventory of inherited resources.
    let result = std::panic::catch_unwind(|| {
        let mut descriptors = if SOCKETS_ABANDONED.load(Ordering::Acquire) {
            Vec::new()
        } else {
            SOCKETS.lock().unwrap_or_else(|e| e.into_inner()).clone()
        };
        let arena = super::state::fork_snapshot(&mut descriptors);
        let snapshot = Box::new(Snapshot { descriptors, arena });
        SNAPSHOT.store(Box::into_raw(snapshot), Ordering::Release);
    });
    if let Err(payload) = result {
        std::mem::forget(payload);
        unsafe {
            libc::_exit(127);
        }
    }
}

pub unsafe extern "C" fn parent() {
    let snapshot = SNAPSHOT.swap(std::ptr::null_mut(), Ordering::AcqRel);
    if !snapshot.is_null() {
        drop(unsafe { Box::from_raw(snapshot) });
    }
}

pub unsafe extern "C" fn child() {
    let snapshot = SNAPSHOT.swap(std::ptr::null_mut(), Ordering::AcqRel);
    if !snapshot.is_null() {
        let snapshot = unsafe { &*snapshot };
        for fd in &snapshot.descriptors {
            unsafe {
                libc::syscall(libc::SYS_close, *fd);
            }
        }
        if let Some((base, size)) = snapshot.arena {
            unsafe {
                libc::syscall(libc::SYS_munmap, base, size);
            }
        }
        // Snapshot and old generation intentionally remain allocated in this
        // child. Never run inherited Rust destructors or CUDA cleanup here.
    }
    super::state::fork_child();
    // Invalidate immediately: the child can reuse an old listener number and
    // fork again before any ordinary shim activity clears the inherited Vec.
    SOCKETS_ABANDONED.store(true, Ordering::Release);
}

pub fn reset_sockets() {
    SOCKETS.lock().unwrap_or_else(|e| e.into_inner()).clear();
    SOCKETS_ABANDONED.store(false, Ordering::Release);
}
