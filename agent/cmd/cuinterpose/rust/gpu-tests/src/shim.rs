// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The packaged artifacts under test, the control endpoint, and the coordinator.

use crate::process::{self, Child, socket_dir, work_dir};
use crate::{core_abi, cu, cuda, loader, sys};
use cuinterpose_protocol::{
    Manifest, MemoryLocation, Record, Reply, Request, Response, socket_path,
};
use std::env;
use std::ffi::{CStr, c_int};
use std::fs;
use std::net::Shutdown;
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::{OnceLock, mpsc};
use std::thread;

pub(crate) const BUILD_DIR: &str = "CUINTERPOSE_BUILD_DIR";
const STATE_FILENAME: &str = "cuinterpose.state";

pub struct Artifacts {
    pub frontend: PathBuf,
    pub core: PathBuf,
    pub coordinator: PathBuf,
}

/// One matching packaged build from `CUINTERPOSE_BUILD_DIR`, the test binary's own
/// directory when `make gpu-tests` placed it there, or `agent/cmd/cuinterpose/build`.
pub fn artifacts() -> &'static Artifacts {
    static ARTIFACTS: OnceLock<Artifacts> = OnceLock::new();
    ARTIFACTS.get_or_init(|| {
        let beside_binary = env::current_exe().unwrap().parent().unwrap().to_owned();
        let build = match env::var_os(BUILD_DIR) {
            Some(directory) => PathBuf::from(directory),
            None if beside_binary.join("libcuinterpose.so").is_file() => beside_binary,
            None => Path::new(env!("CARGO_MANIFEST_DIR")).join("../../build"),
        };
        let build = build.canonicalize().unwrap_or(build);
        let artifacts = Artifacts {
            frontend: build.join("libcuinterpose.so"),
            core: build.join("libcuinterpose_core.so"),
            coordinator: build.join("cuinterpose-coordinator"),
        };
        for path in [&artifacts.frontend, &artifacts.core, &artifacts.coordinator] {
            assert!(
                path.is_file(),
                "missing packaged artifact {}",
                path.display()
            );
        }
        artifacts
    })
}

/// Checks that the artifacts, driver, and GPUs meet the suite's requirements.
pub(crate) fn require_test_environment() {
    artifacts();
    cu!(cuInit(0));
    let mut version = 0;
    cu!(cuDriverGetVersion(&mut version));
    assert!(
        version >= 13010,
        "requires a CUDA 13.1+ driver, found {version}"
    );
    let mut count = 0;
    cu!(cuDeviceGetCount(&mut count));
    assert!(count >= 2, "requires two GPUs, found {count}");
    for (device, peer) in [(0, 1), (1, 0)] {
        let mut capable = 0;
        cu!(cuDeviceCanAccessPeer(&mut capable, device, peer));
        assert!(capable != 0, "GPU {device} needs peer access to GPU {peer}");
    }
}

/// Requires CUDA multicast on both GPUs. Scenario processes skip the check, so a test can
/// call this after dispatching its roles.
pub fn require_multicast() {
    if process::role().is_some() {
        return;
    }
    require_test_environment();
    let attribute = sys::CUdevice_attribute::CU_DEVICE_ATTRIBUTE_MULTICAST_SUPPORTED;
    for device in 0..2 {
        assert!(
            cuda::device_attribute(attribute, device) != 0,
            "GPU {device} does not support CUDA multicast"
        );
    }
}

/// Requires POSIX-shareable HOST_NUMA VMM. Scenario processes skip the check.
pub fn require_host_numa() {
    if process::role().is_some() {
        return;
    }
    require_test_environment();
    let properties = cuda::allocation_properties(0, true);
    let mut handle = 0;
    let status =
        unsafe { sys::cuMemCreate(&mut handle, cuda::granularity(&properties), &properties, 0) };
    assert_eq!(
        status,
        sys::CUresult::CUDA_SUCCESS,
        "requires POSIX-shareable HOST_NUMA VMM"
    );
    cu!(cuMemRelease(handle));
}

/// Sends one control request to `pid` and returns its result.
pub fn request(pid: u32, request: &Request) -> Result<Reply, String> {
    let connection = connect(pid);
    cuinterpose_protocol::send(&connection, request, None).unwrap();
    let (response, _) = cuinterpose_protocol::receive::<Response>(&connection).unwrap();
    assert_eq!(response.namespace_pid, pid);
    response.result
}

/// Sends one control request whose reply cannot be delivered.
pub fn request_without_reply(pid: u32, request: &Request) {
    let connection = connect(pid);
    // The endpoint has received the complete request when its reply fails.
    connection.shutdown(Shutdown::Read).unwrap();
    cuinterpose_protocol::send(&connection, request, None).unwrap();
}

fn connect(pid: u32) -> UnixStream {
    let path = socket_path(&socket_dir(), pid);
    let connection = cuinterpose_protocol::connect(&path, process::TIMEOUT).unwrap();
    connection.set_read_timeout(Some(process::TIMEOUT)).unwrap();
    connection
}

/// The records `pid` currently reports.
pub fn records(pid: u32) -> Vec<Record> {
    match request(pid, &Request::Inspect { namespace_pid: pid }) {
        Ok(Reply::Inspection { records }) => records,
        other => panic!("inspect {pid}: {other:?}"),
    }
}

/// The recorded location of the allocation behind the tracked mapping at `address`.
/// Converted mallocs and their IPC imports cannot be retained, so tests read their
/// tracking from the shim's records.
pub fn mapped_location(pid: u32, address: sys::CUdeviceptr) -> MemoryLocation {
    let records = records(pid);
    let mapped = records
        .iter()
        .find_map(|record| match record {
            Record::Mapping {
                allocation,
                address: start,
                ..
            } if *start == address => Some(*allocation),
            _ => None,
        })
        .unwrap_or_else(|| panic!("no tracked mapping at {address:#x}"));
    records
        .into_iter()
        .find_map(|record| match record {
            Record::Allocation {
                allocation,
                location,
                ..
            } if allocation == mapped => Some(location),
            _ => None,
        })
        .unwrap_or_else(|| panic!("no allocation record for the mapping at {address:#x}"))
}

pub struct Coordinated {
    pub success: bool,
    pub stderr: String,
}

/// Runs one coordinator action for `pids` without checking its result.
pub fn coordinate(action: &str, pids: &[u32], checkpoint_dir: Option<&Path>) -> Coordinated {
    let mut command = Command::new(&artifacts().coordinator);
    command
        .env_remove("LD_PRELOAD")
        .arg(action)
        .arg("--socket-dir")
        .arg(socket_dir());
    if let Some(directory) = checkpoint_dir {
        command.arg("--checkpoint-dir").arg(directory);
    }
    for pid in pids {
        command.arg("--process").arg(pid.to_string());
    }
    let mut coordinator = Child::start(command, format!("coordinator {action}"), None);
    let (status, _, stderr) = coordinator.finish();
    Coordinated {
        success: status.success(),
        stderr,
    }
}

/// Prepares and restores `pids`, with a native CUDA checkpoint in between if requested,
/// and returns the saved records. Every target must be idle in CUDA. A native
/// checkpoint locks the targets' CUDA, so only the test process can run one.
pub fn checkpoint(pids: &[u32], native: bool) -> Manifest {
    let directory = tempfile::Builder::new()
        .prefix("checkpoint-")
        .tempdir_in(work_dir())
        .unwrap();
    for action in ["--prepare", "--restore"] {
        let result = coordinate(action, pids, Some(directory.path()));
        assert!(
            result.success,
            "coordinator {action} failed:\n{}",
            result.stderr
        );
        if action == "--prepare" && native {
            native_checkpoint(pids);
        }
    }
    let state = fs::read(directory.path().join(STATE_FILENAME)).unwrap();
    cuinterpose_protocol::decode(&state).unwrap()
}

type Step<T> = unsafe extern "C" fn(c_int, *mut T) -> sys::CUresult;

/// Locks, checkpoints, restores, and unlocks every target in turn.
fn native_checkpoint(pids: &[u32]) {
    assert!(
        process::role().is_none(),
        "run native checkpoints from the test process"
    );
    let targets = pids.to_vec();
    let (done, finished) = mpsc::channel();
    // A driver call that never returns must fail the test instead of hanging the binary.
    thread::spawn(move || {
        let lock = sys::CUcheckpointLockArgs {
            timeoutMs: 60_000,
            reserved0: 0,
            reserved1: [0; 7],
        };
        // SAFETY: all-zero values are the default arguments of the remaining steps.
        let (checkpoint, restore, unlock) = unsafe { std::mem::zeroed() };
        step(c"cuCheckpointProcessLock", &targets, lock);
        step::<sys::CUcheckpointCheckpointArgs>(
            c"cuCheckpointProcessCheckpoint",
            &targets,
            checkpoint,
        );
        step::<sys::CUcheckpointRestoreArgs>(c"cuCheckpointProcessRestore", &targets, restore);
        step::<sys::CUcheckpointUnlockArgs>(c"cuCheckpointProcessUnlock", &targets, unlock);
        done.send(()).unwrap();
    });
    finished
        .recv_timeout(process::TIMEOUT)
        .unwrap_or_else(|error| panic!("native checkpoint of {pids:?} did not finish: {error}"));
}

fn step<T>(name: &CStr, pids: &[u32], mut arguments: T) {
    // cudarc does not bind the cuCheckpointProcess* entry points.
    let address = core_abi::driver_symbol(name);
    assert!(!address.is_null(), "missing {name:?}");
    let function: Step<T> = loader::cast(address);
    for &pid in pids {
        let status = unsafe { function(pid as c_int, &mut arguments) };
        assert_eq!(
            status,
            sys::CUresult::CUDA_SUCCESS,
            "{name:?} for PID {pid}"
        );
    }
}
