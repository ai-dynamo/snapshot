// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! C-style metadata locking for quiescent fork, not a CUDA-operation barrier.
//! Fork inside interception, initialization, or concurrent CUDA is unsupported.

use std::sync::atomic::{AtomicI32, Ordering};

pub static G_ORIGIN_PID: AtomicI32 = AtomicI32::new(0);

unsafe extern "C" fn prepare() {
    if let Some(Some(api)) = super::loader::G_CORE.get() {
        unsafe { (api.fork_prepare)() };
    }
    // Core operations can resolve providers while holding STATE, so acquire
    // the provider-reference lock last. Never hold it over a CUDA call/dlopen.
    super::loader::fork_prepare();
}

unsafe extern "C" fn parent() {
    super::loader::fork_unlock();
    if let Some(Some(api)) = super::loader::G_CORE.get() {
        unsafe { (api.fork_parent)() };
    }
}

unsafe extern "C" fn child() {
    super::loader::fork_unlock();
    if let Some(Some(api)) = super::loader::G_CORE.get() {
        unsafe { (api.fork_child)() };
    }
}

unsafe extern "C" fn initialize() {
    G_ORIGIN_PID.store(unsafe { libc::getpid() }, Ordering::Release);
    if unsafe { libc::pthread_atfork(Some(prepare), Some(parent), Some(child)) } != 0 {
        super::loader::G_FAILED.store(true, Ordering::Release);
    }
}

#[used]
#[unsafe(link_section = ".init_array")]
static G_INITIALIZE: unsafe extern "C" fn() = initialize;
