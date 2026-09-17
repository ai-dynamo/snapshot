// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Prevent Rust panics from crossing the C ABI.

use std::sync::atomic::{AtomicBool, Ordering};

/// Panics are bugs, not recoverable CUDA failures. Do not continue to mutate
/// state after one. Forget the payload because its destructor may itself panic.
/// This does not intercept the process panic hook or catch aborts/foreign throws.
pub fn call<T: Copy>(failed: &AtomicBool, error: T, operation: impl FnOnce() -> T) -> T {
    if failed.load(Ordering::Acquire) {
        return error;
    }
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(operation)) {
        Ok(value) => value,
        Err(payload) => {
            failed.store(true, Ordering::Release);
            std::mem::forget(payload);
            error
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn panic_poisoning_is_sticky() {
        let failed = AtomicBool::new(false);
        assert_eq!(call(&failed, 1, || panic!("injected")), 1);
        assert_eq!(call(&failed, 1, || 0), 1);
    }
}
