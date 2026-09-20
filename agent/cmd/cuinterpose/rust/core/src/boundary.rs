// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Prevent Rust panics from crossing the C ABI.

use std::sync::atomic::{AtomicBool, Ordering};

/// Panics are bugs and may follow a driver mutation. Terminate without
/// unwinding through C or running the panic payload destructor.
/// This does not intercept the process panic hook or catch aborts/foreign throws.
pub fn call<T: Copy>(failed: &AtomicBool, error: T, operation: impl FnOnce() -> T) -> T {
    if failed.load(Ordering::Acquire) {
        return error;
    }
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(operation)) {
        Ok(value) => value,
        Err(_payload) => std::process::abort(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn panic_terminates_the_process() {
        use std::os::unix::process::ExitStatusExt;
        if std::env::var_os("CUINTERPOSE_TEST_PANIC").is_some() {
            let limit = libc::rlimit {
                rlim_cur: 0,
                rlim_max: 0,
            };
            unsafe { libc::setrlimit(libc::RLIMIT_CORE, &limit) };
            call(&AtomicBool::new(false), (), || panic!("injected"));
            unreachable!("panic returned through the ABI");
        }
        let output = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "boundary::tests::panic_terminates_the_process"])
            .env("CUINTERPOSE_TEST_PANIC", "1")
            .output()
            .unwrap();
        assert_eq!(output.status.signal(), Some(libc::SIGABRT));
    }
}
