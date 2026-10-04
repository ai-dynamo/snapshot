// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use std::env;
use std::ffi::OsString;
use std::fs::File;
use std::io::ErrorKind;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::process::CommandExt;
use std::path::PathBuf;
use std::process::{Command, ExitCode};

fn main() -> ExitCode {
    let mut args = env::args_os().skip(1);
    let option = args.next();
    let library = args.next().map(PathBuf::from);
    let separator = args.next();
    let command = args.next();
    let (Some(library), Some(command)) = (library, command) else {
        eprintln!(
            "cuinterpose-launch: usage: cuinterpose-launch --library PATH -- COMMAND [ARG...]"
        );
        return ExitCode::from(2);
    };
    if option.as_deref() != Some("--library".as_ref())
        || separator.as_deref() != Some("--".as_ref())
        || !library.is_absolute()
        || library
            .as_os_str()
            .as_bytes()
            .iter()
            .any(|byte| matches!(byte, b' ' | b':'))
    {
        eprintln!(
            "cuinterpose-launch: expected --library with an absolute path without LD_PRELOAD separators, followed by -- COMMAND [ARG...]"
        );
        return ExitCode::from(2);
    }
    // A missing preload is only a loader warning. Check delivery before exec so the
    // application cannot start merely because the loader ignored a missing library.
    for path in [&library, &library.with_file_name("libcuinterpose_core.so")] {
        let result = std::fs::metadata(path).and_then(|metadata| {
            if metadata.is_file() {
                File::open(path).map(|_| ())
            } else {
                Err(std::io::Error::new(
                    ErrorKind::InvalidInput,
                    "not a regular file",
                ))
            }
        });
        if let Err(error) = result {
            eprintln!(
                "cuinterpose-launch: cannot use library {}: {error}",
                path.display()
            );
            return ExitCode::FAILURE;
        }
    }
    // The runtime has already resolved the image environment, envFrom, and explicit Pod
    // values, so preserve those bytes when extending LD_PRELOAD, which accepts both
    // spaces and colons.
    let mut preload: OsString = library.into();
    if let Some(existing) = env::var_os("LD_PRELOAD").filter(|value| !value.is_empty()) {
        preload.push(":");
        preload.push(existing);
    }
    let error = Command::new(&command)
        .args(args)
        .env("LD_PRELOAD", preload)
        .exec();
    eprintln!("cuinterpose-launch: cannot execute {command:?}: {error}");
    ExitCode::from(if error.kind() == ErrorKind::NotFound {
        127
    } else {
        126
    })
}
