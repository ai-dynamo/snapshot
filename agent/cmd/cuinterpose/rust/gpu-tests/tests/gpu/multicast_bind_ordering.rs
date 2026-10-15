// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Multicast bind and unbind ordering while the driver call runs without ProcessState.
//!
//! Each scenario runs unshimmed and registers a test frontend with the core, so the
//! core's driver calls reach wrappers that can hold, observe, or fail them.

use cuinterpose_abi::BackendAbi;
use cuinterpose_gpu_tests::process::{Options, TIMEOUT, pid, run_with};
use cuinterpose_gpu_tests::sys::{
    self, CUcontext, CUdevice, CUmemAllocationProp, CUmemGenericAllocationHandle, CUresult,
};
use cuinterpose_gpu_tests::{core_abi, cu, cuda, shim, tracked};
use cuinterpose_protocol::{BindingSource, MemberRange, Record, Request};
use std::ffi::{CStr, c_char, c_void};
use std::panic;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Condvar, Mutex, OnceLock};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant};

type Handle = CUmemGenericAllocationHandle;

/// How long a call that must stay blocked is watched. Correct code never finishes it, so a
/// slow host can only hide a bug here, not fail the test.
const WINDOW: Duration = Duration::from_secs(1);

#[test]
fn bind_waits_until_concurrent_unbind_records_its_result() {
    shim::require_multicast();
    run_with(Options::unshimmed(), || {
        let backend = register_gated_frontend();
        let context = cuda::primary_contexts(1)[0];
        let (group, size) = multicast_group(backend, &[0, 1]);
        let properties = cuda::allocation_properties(0, false);
        let original = create(backend, size, &properties);
        // The size tells the replacement's record apart from the original's.
        let replacement = create(backend, 2 * size, &properties);
        tracked!(backend.cuMulticastBindMem(group, 0, original, 0, size, 0));
        let references: Vec<_> = shim::records(pid())
            .into_iter()
            .filter_map(|record| match record {
                Record::Allocation {
                    allocation,
                    size: recorded,
                    ..
                } if recorded == 2 * size as u64 => Some(allocation),
                _ => None,
            })
            .collect();
        let [reference] = references[..] else {
            panic!("{references:?}");
        };
        let gate = Gate::start();
        let unbind = in_thread(context, move || {
            tracked!(backend.cuMulticastUnbind(group, 0, 0, size))
        });
        assert!(gate.unbind_held(TIMEOUT), "native unbind did not return");
        // Unbind releases ProcessState while CUDA runs, so a call that needs ProcessState
        // but not this multicast object completes.
        let query = in_thread(context, move || {
            // SAFETY: CUmemAllocationProp is plain data.
            let mut properties: CUmemAllocationProp = unsafe { std::mem::zeroed() };
            tracked!(backend.cuMemGetAllocationPropertiesFromHandle(&mut properties, replacement));
            properties.location.id
        });
        assert_eq!(query.result(TIMEOUT), 0);
        assert_not_ready(
            Request::Inspect {
                namespace_pid: pid(),
            },
            INSPECT,
        );
        assert_not_ready(
            Request::BeginCheckpoint {
                namespace_pid: pid(),
            },
            BEGIN,
        );
        let rebind = in_thread(context, move || {
            tracked!(backend.cuMulticastBindMem(group, 0, replacement, 0, size, 0))
        });
        assert!(
            !gate.bind_entered(WINDOW),
            "bind entered CUDA before unbind committed its metadata"
        );
        assert!(!rebind.done(), "bind did not wait for unbind");
        assert_not_ready(
            Request::BeginCheckpoint {
                namespace_pid: pid(),
            },
            BEGIN,
        );
        drop(gate);
        unbind.result(TIMEOUT);
        rebind.result(TIMEOUT);
        let records = shim::records(pid());
        let member = MemberRange {
            allocation: reference,
            offset: 0,
        };
        let expected = (BindingSource::Memory(member), 0, 0, size as u64);
        assert_eq!(bindings(&records), [expected], "{records:?}");
        // A binding lock leaked by the rebind would deadlock this unbind.
        tracked!(backend.cuMulticastUnbind(group, 0, 0, size));
    });
}

#[test]
fn failed_unbind_returns_driver_error_and_keeps_binding_record() {
    shim::require_multicast();
    run_with(Options::unshimmed(), || {
        let backend = register_gated_frontend();
        cuda::primary_contexts(1);
        let (group, size) = multicast_group(backend, &[0, 1]);
        let member = create(backend, size, &cuda::allocation_properties(0, false));
        tracked!(backend.cuMulticastBindMem(group, 0, member, 0, size, 0));
        let before = shim::records(pid());
        assert!(
            matches!(bindings(&before)[..], [(_, 0, 0, bound)] if bound == size as u64),
            "{before:?}"
        );
        // The core sees a valid unbind of the bound range, and only the driver call fails.
        FAIL_UNBIND.store(true, Ordering::SeqCst);
        // SAFETY: the group and range are valid for cuMulticastUnbind.
        let status = unsafe { (backend.cuMulticastUnbind)(group, 0, 0, size) };
        let native = *FAILED_UNBIND
            .get()
            .expect("the unbind did not reach the driver");
        assert_ne!(
            native,
            CUresult::CUDA_SUCCESS,
            "the driver accepted device -1"
        );
        assert_eq!(status, native, "the core changed the driver's status");
        // Inspection reports NOT_READY if the failed call did not leave its unlocked window.
        assert_eq!(
            shim::records(pid()),
            before,
            "the failed unbind changed tracked state"
        );
        // A binding lock still held by the failed call would deadlock this unbind.
        tracked!(backend.cuMulticastUnbind(group, 0, 0, size));
    });
}

#[test]
fn add_device_proceeds_while_bind_waits_for_all_devices() {
    shim::require_multicast();
    run_with(Options::unshimmed(), || {
        let backend = register_gated_frontend();
        let context = cuda::primary_contexts(1)[0];
        let (group, size) = multicast_group(backend, &[0]);
        let member = create(backend, size, &cuda::allocation_properties(0, false));
        let gate = Gate::start();
        let binding = in_thread(context, move || {
            tracked!(backend.cuMulticastBindMem(group, 0, member, 0, size, 0))
        });
        assert!(gate.bind_entered(TIMEOUT), "native bind did not start");
        // CUDA holds the bind until every device has joined, so neither the binding lock
        // nor ProcessState may block cuMulticastAddDevice.
        assert!(
            !binding.finished_within(WINDOW),
            "bind completed before all devices joined"
        );
        in_thread(context, move || {
            tracked!(backend.cuMulticastAddDevice(group, 1))
        })
        .result(TIMEOUT);
        binding.result(TIMEOUT);
        drop(gate);
        let records = shim::records(pid());
        let mut devices: Vec<_> = records
            .iter()
            .filter_map(|record| match record {
                Record::MulticastDevice { device, .. } => Some(*device),
                _ => None,
            })
            .collect();
        devices.sort();
        assert_eq!(devices, [0, 1], "{records:?}");
        assert_eq!(bindings(&records).len(), 1, "{records:?}");
        // A binding lock leaked by the bind would deadlock this unbind.
        tracked!(backend.cuMulticastUnbind(group, 0, 0, size));
    });
}

/// Each binding record as `(source, device, offset, size)`.
fn bindings(records: &[Record]) -> Vec<(BindingSource, i32, u64, u64)> {
    records
        .iter()
        .filter_map(|record| match *record {
            Record::MulticastBinding {
                source,
                device,
                offset,
                size,
                ..
            } => Some((source, device, offset, size)),
            _ => None,
        })
        .collect()
}

/// Registers the gated frontend after the real cuInit, as the preloaded frontend does,
/// and starts the core's runtime.
fn register_gated_frontend() -> &'static BackendAbi {
    let (backend, status) = core_abi::start(resolve);
    assert_eq!(status, CUresult::CUDA_SUCCESS, "start the core's runtime");
    backend
}

/// Resolves the driver for the core, with the calls that the gate watches wrapped. The
/// wrappers call sys, which reaches the driver directly in this unshimmed process.
unsafe extern "C" fn resolve(name: *const c_char) -> *mut c_void {
    let name = unsafe { CStr::from_ptr(name) };
    match name.to_bytes() {
        b"cuMulticastUnbind" => gated_unbind as *mut c_void,
        b"cuMulticastBindMem" => announced_bind_mem as *mut c_void,
        _ => core_abi::driver_symbol(name),
    }
}

/// Makes the next unbind fail in the driver.
static FAIL_UNBIND: AtomicBool = AtomicBool::new(false);
/// The driver's status for that unbind.
static FAILED_UNBIND: OnceLock<CUresult> = OnceLock::new();

/// Calls the real unbind, then holds the first one that the gate sees until the gate
/// stops, before the core can record the result. With [`FAIL_UNBIND`] set, it instead
/// passes the driver an invalid device.
unsafe extern "C" fn gated_unbind(
    group: Handle,
    device: CUdevice,
    offset: usize,
    size: usize,
) -> CUresult {
    if FAIL_UNBIND.swap(false, Ordering::SeqCst) {
        // The driver rejects an invalid device. Unbinding a range other than the bound one
        // would instead be undefined behavior.
        let status = unsafe { sys::cuMulticastUnbind(group, -1, offset, size) };
        FAILED_UNBIND.set(status).unwrap();
        return status;
    }
    let status = unsafe { sys::cuMulticastUnbind(group, device, offset, size) };
    let mut seen = SEEN.lock().unwrap();
    if seen.watching && !seen.unbind_held {
        seen.unbind_held = true;
        CHANGED.notify_all();
        let _released = CHANGED.wait_while(seen, |seen| seen.watching).unwrap();
    }
    status
}

/// Reports entry to the gate, then calls the real bind.
unsafe extern "C" fn announced_bind_mem(
    group: Handle,
    offset: usize,
    member: Handle,
    member_offset: usize,
    size: usize,
    flags: u64,
) -> CUresult {
    update(|seen| seen.bind_entered |= seen.watching);
    unsafe { sys::cuMulticastBindMem(group, offset, member, member_offset, size, flags) }
}

/// What the gate has seen since it started.
struct Seen {
    watching: bool,
    unbind_held: bool,
    bind_entered: bool,
}

static SEEN: Mutex<Seen> = Mutex::new(Seen {
    watching: false,
    unbind_held: false,
    bind_entered: false,
});
static CHANGED: Condvar = Condvar::new();

fn update(change: impl FnOnce(&mut Seen)) {
    change(&mut SEEN.lock().unwrap());
    CHANGED.notify_all();
}

/// Holds the first unbind after CUDA returns and reports when a bind enters CUDA, from
/// start until drop. Every call and result passes through unchanged.
struct Gate;

impl Gate {
    fn start() -> Self {
        update(|seen| seen.watching = true);
        Self
    }

    /// Whether an unbind was held within `timeout`.
    fn unbind_held(&self, timeout: Duration) -> bool {
        Self::wait(timeout, |seen| seen.unbind_held)
    }

    /// Whether a bind entered CUDA within `timeout`.
    fn bind_entered(&self, timeout: Duration) -> bool {
        Self::wait(timeout, |seen| seen.bind_entered)
    }

    fn wait(timeout: Duration, flag: fn(&Seen) -> bool) -> bool {
        let seen = SEEN.lock().unwrap();
        let (seen, _) = CHANGED
            .wait_timeout_while(seen, timeout, |seen| !flag(seen))
            .unwrap();
        flag(&seen)
    }
}

impl Drop for Gate {
    /// Releases a held unbind and stops watching, also when an assertion fails.
    fn drop(&mut self) {
        update(|seen| seen.watching = false);
    }
}

/// A call on its own thread, which a failed scenario does not wait for.
struct Pending<T>(JoinHandle<T>);

impl<T> Pending<T> {
    fn done(&self) -> bool {
        self.0.is_finished()
    }

    /// Whether the call returns within `timeout`.
    fn finished_within(&self, timeout: Duration) -> bool {
        let deadline = Instant::now() + timeout;
        while !self.done() {
            if Instant::now() >= deadline {
                return false;
            }
            thread::sleep(Duration::from_millis(10));
        }
        true
    }

    fn result(self, timeout: Duration) -> T {
        assert!(
            self.finished_within(timeout),
            "call did not return within {timeout:?}"
        );
        self.0
            .join()
            .unwrap_or_else(|payload| panic::resume_unwind(payload))
    }
}

/// Runs `call` with `context` current on a new thread.
fn in_thread<T: Send + 'static>(
    context: CUcontext,
    call: impl FnOnce() -> T + Send + 'static,
) -> Pending<T> {
    let context = context as usize;
    Pending(thread::spawn(move || {
        cu!(cuCtxSetCurrent(context as CUcontext));
        call()
    }))
}

/// A multicast object as in `cuda::multicast_group`, created through the core.
fn multicast_group(backend: &BackendAbi, devices: &[i32]) -> (Handle, usize) {
    let properties = cuda::multicast_properties();
    let mut group = 0;
    tracked!(backend.cuMulticastCreate(&mut group, &properties));
    for &device in devices {
        tracked!(backend.cuMulticastAddDevice(group, device));
    }
    (group, properties.size)
}

fn create(backend: &BackendAbi, size: usize, properties: &CUmemAllocationProp) -> Handle {
    let mut handle = 0;
    tracked!(backend.cuMemCreate(&mut handle, size, properties, 0));
    handle
}

const INSPECT: &str = "cannot inspect current CUDA state";
const BEGIN: &str = "application is not ready for checkpoint";

/// Requires a request that needs tracked state to report NOT_READY while a call runs
/// without ProcessState.
fn assert_not_ready(request: Request, refusal: &str) {
    let error = shim::request(pid(), &request).expect_err("the request succeeded");
    assert_eq!(error, format!("{refusal}: CUDA error CUDA_ERROR_NOT_READY"));
}
