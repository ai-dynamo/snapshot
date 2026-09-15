// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Fork quiescence is outside the loader bootstrap. Only operations touching
//! mutable frontend/core state enter this gate; ordinary dlsym remains usable
//! by allocator and loader initialization.

use cuinterpose_abi::{Core, NOT_SUPPORTED, SUCCESS};
use std::cell::Cell;
use std::sync::atomic::{AtomicI32, AtomicPtr, AtomicU32, Ordering};

const WRITER: u32 = 1 << 31;
static GATE: AtomicU32 = AtomicU32::new(0);
static PID: AtomicI32 = AtomicI32::new(0);
pub static ORIGIN_PID: AtomicI32 = AtomicI32::new(0);
static API: AtomicPtr<Core> = AtomicPtr::new(std::ptr::null_mut());
thread_local! {
    static DEPTH: Cell<u32> = const { Cell::new(0) };
    static FORKING: Cell<bool> = const { Cell::new(false) };
    static PREPARED: Cell<bool> = const { Cell::new(false) };
}

pub struct Guard;

impl Guard {
    pub fn enter() -> Option<Self> {
        if unsafe { enter() } == SUCCESS {
            Some(Self)
        } else {
            None
        }
    }
}

impl Drop for Guard {
    fn drop(&mut self) {
        unsafe {
            leave();
        }
    }
}

pub unsafe extern "C" fn enter() -> i32 {
    if PID.load(Ordering::Acquire) != unsafe { libc::getpid() } || FORKING.with(Cell::get) {
        return NOT_SUPPORTED;
    }
    DEPTH.with(|depth| {
        let count = depth.get();
        if count == u32::MAX {
            return NOT_SUPPORTED;
        }
        if count == 0 {
            loop {
                let value = GATE.load(Ordering::Acquire);
                if value & WRITER != 0 {
                    // Do not wait while the caller may own a loader lock that
                    // libc's fork needs. CUDA calls racing the closed barrier
                    // are refused; admitted operations always finish first.
                    return NOT_SUPPORTED;
                } else if value & !WRITER == WRITER - 1 {
                    return NOT_SUPPORTED;
                } else if GATE
                    .compare_exchange_weak(value, value + 1, Ordering::Acquire, Ordering::Relaxed)
                    .is_ok()
                {
                    break;
                }
            }
        }
        depth.set(count + 1);
        SUCCESS
    })
}

pub unsafe extern "C" fn leave() {
    DEPTH.with(|depth| {
        let count = depth.get();
        if count == 0 {
            return;
        }
        depth.set(count - 1);
        if count == 1 {
            GATE.fetch_sub(1, Ordering::Release);
        }
    });
}

pub fn publish(api: *const Core) {
    API.store(api.cast_mut(), Ordering::Release);
}

unsafe extern "C" fn prepare() {
    // A fork invoked from a driver/constructor while this thread holds a read
    // lease cannot quiesce itself. Our public fork wrapper refuses it. A libc
    // internal bypass is fail-stop in the child rather than reusing live locks.
    if DEPTH.with(|depth| depth.get() != 0) {
        return;
    }
    // Public fork already owns admission. A libc-internal bypass must also
    // try, never wait: this caller may hold the loader lock needed by a reader.
    if !FORKING.with(Cell::get) {
        if GATE
            .compare_exchange(0, WRITER, Ordering::AcqRel, Ordering::Relaxed)
            .is_err()
        {
            return;
        }
        FORKING.with(|forking| forking.set(true));
    }
    let api = API.load(Ordering::Acquire);
    if !api.is_null() {
        unsafe {
            ((*api).fork_prepare)();
        }
    }
    PREPARED.with(|prepared| prepared.set(true));
}

unsafe extern "C" fn parent() {
    if !PREPARED.with(|prepared| prepared.replace(false)) {
        return;
    }
    let api = API.load(Ordering::Acquire);
    if !api.is_null() {
        unsafe {
            ((*api).fork_parent)();
        }
    }
    GATE.store(0, Ordering::Release);
    FORKING.with(|forking| forking.set(false));
}

unsafe extern "C" fn child() {
    if !PREPARED.with(|prepared| prepared.replace(false)) {
        unsafe {
            libc::_exit(127);
        }
    }
    let api = API.load(Ordering::Acquire);
    if !api.is_null() {
        unsafe {
            ((*api).fork_child)();
        }
    }
    PID.store(unsafe { libc::getpid() }, Ordering::Release);
    GATE.store(0, Ordering::Release);
    FORKING.with(|forking| forking.set(false));
}

unsafe extern "C" fn initialize() {
    PID.store(unsafe { libc::getpid() }, Ordering::Release);
    ORIGIN_PID.store(unsafe { libc::getpid() }, Ordering::Release);
    if unsafe { libc::pthread_atfork(Some(prepare), Some(parent), Some(child)) } != 0 {
        super::loader::FAILED.store(true, Ordering::Release);
    }
}

#[used]
#[unsafe(link_section = ".init_array")]
static INITIALIZE: unsafe extern "C" fn() = initialize;

#[unsafe(no_mangle)]
pub unsafe extern "C" fn fork() -> libc::pid_t {
    cuinterpose_abi::boundary(&super::loader::FAILED, -1, || unsafe { fork_inner() })
}

unsafe fn fork_inner() -> libc::pid_t {
    if DEPTH.with(|depth| depth.get() != 0) || FORKING.with(Cell::get) {
        unsafe {
            *libc::__errno_location() = libc::EDEADLK;
        }
        return -1;
    }
    // glibc's public fork implementation runs all registered atfork handlers.
    // Resolve from this DSO so the next fork interposer remains in the chain.
    let Some(resolve) = super::loader::real() else {
        unsafe {
            *libc::__errno_location() = libc::ENOSYS;
        }
        return -1;
    };
    let address = unsafe { resolve(libc::RTLD_NEXT, c"fork".as_ptr()) };
    if address.is_null() {
        unsafe {
            *libc::__errno_location() = libc::ENOSYS;
        }
        return -1;
    }
    let function: unsafe extern "C" fn() -> libc::pid_t = unsafe { std::mem::transmute(address) };
    // Refuse before libc's non-failable atfork callbacks. Waiting for readers
    // here can invert loader-lock order when fork is called by a constructor.
    if GATE
        .compare_exchange(0, WRITER, Ordering::AcqRel, Ordering::Relaxed)
        .is_err()
    {
        unsafe {
            *libc::__errno_location() = libc::EAGAIN;
        }
        return -1;
    }
    FORKING.with(|forking| forking.set(true));
    let result = unsafe { function() };
    // A later fork interposer can refuse without calling libc's callbacks.
    // Do not leave admission closed in that case.
    if FORKING.with(|forking| forking.replace(false)) {
        GATE.store(0, Ordering::Release);
    }
    result
}
