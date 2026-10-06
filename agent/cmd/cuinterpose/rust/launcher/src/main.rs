// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Run `make native` in `agent/cmd/cuinterpose` before workspace Cargo commands,
//! including Clippy and rust-analyzer checks. The build script hashes both finished
//! preload libraries, so they must exist before Cargo builds this crate.

use sha2::{Digest, Sha256};
use std::env;
use std::ffi::OsString;
use std::fs::File;
use std::io::ErrorKind;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::process::CommandExt;
use std::path::PathBuf;
use std::process::{Command, ExitCode};

include!(concat!(env!("OUT_DIR"), "/library_hashes.rs"));

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
    // A missing or corrupt preload can be only a loader warning. Require the exact
    // libraries built with this launcher before exec. Delivery stays read-only while
    // the workload starts, so the loader sees the bytes verified here.
    for (path, expected) in [
        (&library, &FRONTEND_SHA256),
        (
            &library.with_file_name("libcuinterpose_core.so"),
            &CORE_SHA256,
        ),
    ] {
        let result = (|| {
            // Reject FIFOs before open, which would otherwise wait for a writer.
            if !std::fs::metadata(path)?.is_file() {
                return Err(std::io::Error::new(
                    ErrorKind::InvalidInput,
                    "not a regular file",
                ));
            }
            let mut file = File::open(path)?;
            if !file.metadata()?.is_file() {
                return Err(std::io::Error::new(
                    ErrorKind::InvalidInput,
                    "not a regular file",
                ));
            }
            let mut digest = Sha256::new();
            std::io::copy(&mut file, &mut digest)?;
            let actual: [u8; 32] = digest.finalize().into();
            if actual != *expected {
                return Err(std::io::Error::new(
                    ErrorKind::InvalidData,
                    "SHA-256 mismatch with this launcher build",
                ));
            }
            Ok(())
        })();
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
