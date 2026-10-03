// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use std::env;
use std::ffi::OsString;
use std::os::unix::process::CommandExt;
use std::process::{Command, ExitCode};

fn main() -> ExitCode {
    let mut args = env::args_os().skip(1);
    let Some(command) = args.next() else {
        eprintln!(
            "cuinterpose-launch: missing command; usage: cuinterpose-launch COMMAND [ARG...]"
        );
        return ExitCode::from(2);
    };
    // The runtime has already resolved the image environment, envFrom, and explicit Pod
    // values, so preserve those bytes when extending LD_PRELOAD, which accepts both
    // spaces and colons.
    let mut preload = OsString::from("/tmp/snapshot-cuda/libcuinterpose.so");
    if let Some(existing) = env::var_os("LD_PRELOAD").filter(|value| !value.is_empty()) {
        preload.push(":");
        preload.push(existing);
    }
    let error = Command::new(&command)
        .args(args)
        .env("LD_PRELOAD", preload)
        .exec();
    eprintln!("cuinterpose-launch: cannot execute {command:?}: {error}");
    ExitCode::FAILURE
}
