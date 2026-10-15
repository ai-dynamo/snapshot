// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! When and how the frontend's cuInit starts the process's control socket.

use cuinterpose_gpu_tests::loader::{self, frontend_address, global, proc_address};
use cuinterpose_gpu_tests::process::{self, Options, endpoint, run, run_with, socket_dir};
use cuinterpose_gpu_tests::sys::CUresult;
use cuinterpose_gpu_tests::{fixtures, shim};
use rustix::net::{AddressFamily, SocketAddrUnix, SocketType, bind, connect, listen, socket};
use std::collections::BTreeMap;
use std::fs;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};

const SUCCESS: CUresult = CUresult::CUDA_SUCCESS;

fn cu_init(flags: u32) -> CUresult {
    let initialize: unsafe extern "C" fn(u32) -> CUresult = global(c"cuInit");
    unsafe { initialize(flags) }
}

fn assert_no_records() {
    assert_eq!(shim::records(process::pid()), []);
}

/// Runs `body` in a fork child and requires it to return.
fn in_fork_child(body: fn()) {
    match unsafe { libc::fork() } {
        0 => {
            let code = std::panic::catch_unwind(body).map_or(1, |()| 0);
            unsafe { libc::_exit(code) }
        }
        pid => {
            let mut status = 0;
            assert_eq!(unsafe { libc::waitpid(pid, &mut status, 0) }, pid);
            assert!(
                libc::WIFEXITED(status) && libc::WEXITSTATUS(status) == 0,
                "fork child failed"
            );
        }
    }
}

#[test]
fn cuinit_creates_socket_with_mode_0600_under_umask_0() {
    run(|| {
        assert!(!endpoint().exists());
        // Under umask 0, only an explicit chmod makes the socket private.
        unsafe { libc::umask(0) };
        assert_eq!(cu_init(0), SUCCESS);
        let metadata = fs::metadata(endpoint()).unwrap();
        let mode = metadata.permissions().mode() & 0o7777;
        assert!(
            metadata.file_type().is_socket() && mode == 0o600,
            "{mode:o}"
        );
        assert_eq!(
            unsafe { libc::umask(0) },
            0,
            "runtime startup changed the umask"
        );
        assert_no_records();
    });
}

#[test]
fn failed_cuinit_creates_no_socket() {
    run(|| {
        assert_eq!(cu_init(1), CUresult::CUDA_ERROR_INVALID_VALUE);
        assert!(!endpoint().exists());
        assert_eq!(cu_init(0), SUCCESS);
        assert_no_records();
    });
}

#[test]
fn relative_ld_preload_finds_core_after_chdir() {
    let frontend = &shim::artifacts().frontend;
    let name = frontend.file_name().unwrap().to_str().unwrap();
    let options = Options::default()
        .env("LD_PRELOAD", format!("./{name}"))
        .cwd(frontend.parent().unwrap());
    run_with(options, || {
        std::env::set_current_dir(process::work_dir()).unwrap();
        // The relative preload no longer resolves, so only $ORIGIN can locate the core.
        assert!(!Path::new(&std::env::var("LD_PRELOAD").unwrap()).exists());
        assert_eq!(cu_init(0), SUCCESS);
        assert_no_records();
    });
}

#[test]
fn proc_address_query_before_cuinit_returns_wrapper_without_creating_socket() {
    run(|| {
        // The first query loads the driver. The _ptsz query forwards to _v2.
        assert_eq!(
            proc_address(c"cuGetProcAddress_v2_ptsz", c"cuMemCreate"),
            frontend_address(c"cuMemCreate")
        );
        assert!(!endpoint().exists());
        assert_eq!(cu_init(0), SUCCESS);
    });
}

#[test]
fn cuinit_from_library_constructor_does_not_deadlock() {
    let library = fixtures::cuinit_constructor();
    run(move || {
        // The constructor calls cuInit while holding the loader lock.
        loader::open(&library, libc::RTLD_NOW);
        assert_no_records();
    });
}

#[test]
fn child_forked_before_cuinit_gets_its_own_socket() {
    run(|| {
        in_fork_child(initialize_endpoint);
        initialize_endpoint();
    });
}

fn initialize_endpoint() {
    assert_eq!(cu_init(0), SUCCESS);
    assert_no_records();
}

#[test]
fn child_forked_after_cuinit_gets_not_initialized() {
    let stderr = run(|| {
        assert_eq!(cu_init(0), SUCCESS);
        in_fork_child(use_parent_runtime);
        assert_no_records();
    });
    assert!(
        stderr.contains("runtime belongs to another process"),
        "{stderr}"
    );
}

fn use_parent_runtime() {
    // The driver rejects cuInit in a fork child before the shim runs. A tracked call
    // reaches the shim, whose ownership check writes the diagnostic.
    let release: unsafe extern "C" fn(u64) -> CUresult = global(c"cuMemRelease");
    assert_eq!(cu_init(0), CUresult::CUDA_ERROR_NOT_INITIALIZED);
    assert_eq!(unsafe { release(42) }, CUresult::CUDA_ERROR_NOT_INITIALIZED);
    assert!(!endpoint().exists());
}

#[test]
fn cuinit_after_exec_replaces_stale_socket_at_same_pid() {
    if process::role().as_deref() == Some("after-exec") {
        // The old image's listener closed on exec and left a stale socket at this path.
        assert!(fs::metadata(endpoint()).unwrap().file_type().is_socket());
        assert_eq!(cu_init(0), SUCCESS);
        assert_no_records();
        return;
    }
    run(|| {
        assert_eq!(cu_init(0), SUCCESS);
        process::exec("after-exec");
    });
}

#[derive(Clone, Copy)]
enum Existing {
    RegularFile,
    SymlinkToStaleSocket,
    LiveListener,
    ListenerWithFullBacklog,
}

#[test]
fn cuinit_fails_without_touching_existing_regular_file() {
    cuinit_fails_without_touching(Existing::RegularFile);
}

#[test]
fn cuinit_fails_without_touching_symlink_to_stale_socket() {
    cuinit_fails_without_touching(Existing::SymlinkToStaleSocket);
}

#[test]
fn cuinit_fails_without_touching_live_listener() {
    cuinit_fails_without_touching(Existing::LiveListener);
}

#[test]
fn cuinit_fails_without_touching_listener_with_full_backlog() {
    cuinit_fails_without_touching(Existing::ListenerWithFullBacklog);
}

fn cuinit_fails_without_touching(existing: Existing) {
    let stderr = run(move || {
        let path = endpoint();
        let _sockets = occupy(&path, existing);
        let before = socket_dir_entries();
        assert_eq!(cu_init(0), CUresult::CUDA_ERROR_OPERATING_SYSTEM);
        assert_eq!(socket_dir_entries(), before);
        if let Existing::RegularFile = existing {
            assert_eq!(fs::read_to_string(&path).unwrap(), "preserve this file");
        }
        // Procedure queries do not depend on the failed runtime.
        let query = proc_address(c"cuGetProcAddress_v2", c"cuMemCreate");
        assert_eq!(query, frontend_address(c"cuMemCreate"));
        fs::remove_file(&path).unwrap();
        // The startup failure is permanent even once the path is free.
        assert_eq!(cu_init(0), CUresult::CUDA_ERROR_NOT_INITIALIZED);
        assert!(!path.exists());
    });
    assert!(stderr.contains("bind control socket"), "{stderr}");
    assert!(
        stderr.contains(&format!("os error {}", libc::EADDRINUSE)),
        "{stderr}"
    );
}

/// Puts `existing` at `path` and returns the sockets that must stay open.
fn occupy(path: &Path, existing: Existing) -> Vec<rustix::fd::OwnedFd> {
    let listener = |path: &Path, backlog| {
        let fd = socket(AddressFamily::UNIX, SocketType::STREAM, None).unwrap();
        bind(&fd, &SocketAddrUnix::new(path).unwrap()).unwrap();
        listen(&fd, backlog).unwrap();
        fd
    };
    match existing {
        Existing::RegularFile => {
            fs::write(path, "preserve this file").unwrap();
            vec![]
        }
        Existing::SymlinkToStaleSocket => {
            // The link points at a stale socket, so following it would make the path
            // look replaceable.
            let target = path.with_extension("target");
            drop(listener(&target, 1));
            std::os::unix::fs::symlink(&target, path).unwrap();
            vec![]
        }
        Existing::LiveListener => vec![listener(path, 1)],
        Existing::ListenerWithFullBacklog => {
            // A blocking probe would hang here while holding the installation lock.
            let live = listener(path, 0);
            let queued = socket(AddressFamily::UNIX, SocketType::STREAM, None).unwrap();
            connect(&queued, &SocketAddrUnix::new(path).unwrap()).unwrap();
            vec![live, queued]
        }
    }
}

/// Maps each entry to its own inode and mode, so a replaced path or link shows up.
fn socket_dir_entries() -> BTreeMap<PathBuf, (u64, u32)> {
    fs::read_dir(socket_dir())
        .unwrap()
        .map(|entry| {
            let path = entry.unwrap().path();
            let metadata = fs::symlink_metadata(&path).unwrap();
            (path, (metadata.ino(), metadata.mode()))
        })
        .collect()
}
