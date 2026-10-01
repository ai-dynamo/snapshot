// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Exercise real CPU bytes and alias mappings without a GPU. Unsupported device-copy
//! calls intentionally make accidental use of the device path fail.
use super::*;
use cudarc::driver::sys::{
    CUmemAllocationHandleType, CUmemAllocationType, CUmemLocation, CUresult,
    CUresult::{CUDA_ERROR_INVALID_DEVICE, CUDA_SUCCESS},
};
use cuinterpose_abi::{ABI_VERSION, FrontendAbi};
use cuinterpose_protocol::AllocationReference;
use std::ffi::{CStr, c_char};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

static CURRENT: AtomicUsize = AtomicUsize::new(0);
static REGISTRATIONS: AtomicUsize = AtomicUsize::new(0);
static HOST_ACCESS: AtomicUsize = AtomicUsize::new(0);
static PRIMARY_REFS: AtomicUsize = AtomicUsize::new(0);
static CREATES: AtomicUsize = AtomicUsize::new(0);
static HANDLES: AtomicUsize = AtomicUsize::new(0);
static RESERVATIONS: AtomicUsize = AtomicUsize::new(0);
static MAPPINGS: AtomicUsize = AtomicUsize::new(0);
static DEVICE_COPIES: AtomicUsize = AtomicUsize::new(0);
static MIXED: AtomicBool = AtomicBool::new(false);

unsafe extern "C" fn current(output: *mut *mut c_void) -> CUresult {
    unsafe { output.write(CURRENT.load(Ordering::Relaxed) as *mut c_void) };
    CUDA_SUCCESS
}
unsafe extern "C" fn switch(context: *mut c_void) -> CUresult {
    CURRENT.store(context as usize, Ordering::Relaxed);
    CUDA_SUCCESS
}
unsafe extern "C" fn retain(output: *mut *mut c_void, device: i32) -> CUresult {
    // NUMA IDs below deliberately exceed the GPU ordinal space.
    if device != 0 {
        return CUDA_ERROR_INVALID_DEVICE;
    }
    PRIMARY_REFS.fetch_add(1, Ordering::Relaxed);
    unsafe { output.write(std::ptr::dangling_mut::<c_void>()) };
    CUDA_SUCCESS
}
unsafe extern "C" fn release_context(_: i32) -> CUresult {
    if PRIMARY_REFS.fetch_sub(1, Ordering::Relaxed) == 1 {
        assert_eq!(REGISTRATIONS.load(Ordering::Relaxed), 0);
    }
    CUDA_SUCCESS
}
unsafe extern "C" fn register(_: *mut c_void, _: usize, _: u32) -> CUresult {
    assert!(PRIMARY_REFS.load(Ordering::Relaxed) > 0);
    assert_eq!(REGISTRATIONS.fetch_add(1, Ordering::Relaxed), 0);
    CUDA_SUCCESS
}
unsafe extern "C" fn unregister(_: *mut c_void) -> CUresult {
    assert!(PRIMARY_REFS.load(Ordering::Relaxed) > 0);
    assert_eq!(REGISTRATIONS.fetch_sub(1, Ordering::Relaxed), 1);
    CUDA_SUCCESS
}
unsafe extern "C" fn create(
    output: *mut u64,
    size: usize,
    properties: *const CUmemAllocationProp,
    _: u64,
) -> CUresult {
    let properties = unsafe { &*properties };
    // The second pass must recreate each allocation with its original NUMA
    // placement, even though both host nodes use GPU 0 for a fallback context.
    let mixed = MIXED.load(Ordering::Relaxed);
    let index = CREATES.fetch_add(1, Ordering::Relaxed) % if mixed { 3 } else { 2 };
    let expected_location = match (mixed, index) {
        (_, 0) => (CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA, 7),
        (true, 1) => (CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE, 0),
        _ => (CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA, 57),
    };
    if (properties.location.type_, properties.location.id) != expected_location
        || properties.type_ != CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED
        || properties.requestedHandleTypes
            != CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
    {
        return CUDA_ERROR_INVALID_VALUE;
    }
    let fd = unsafe { libc::memfd_create(c"host-vmm-test".as_ptr(), libc::MFD_CLOEXEC) };
    assert!(fd >= 0);
    assert_eq!(unsafe { libc::ftruncate(fd, size as libc::off_t) }, 0);
    HANDLES.fetch_add(1, Ordering::Relaxed);
    unsafe { output.write(fd as u64 + 1) };
    CUDA_SUCCESS
}
unsafe extern "C" fn release(handle: u64) -> CUresult {
    assert_eq!(unsafe { libc::close((handle - 1) as i32) }, 0);
    assert!(HANDLES.fetch_sub(1, Ordering::Relaxed) > 0);
    CUDA_SUCCESS
}
unsafe extern "C" fn reserve(output: *mut u64, size: usize, _: usize, _: u64, _: u64) -> CUresult {
    let address = unsafe {
        libc::mmap(
            std::ptr::null_mut(),
            size,
            libc::PROT_NONE,
            libc::MAP_ANONYMOUS | libc::MAP_PRIVATE,
            -1,
            0,
        )
    };
    assert_ne!(address, libc::MAP_FAILED);
    RESERVATIONS.fetch_add(1, Ordering::Relaxed);
    unsafe { output.write(address as u64) };
    CUDA_SUCCESS
}
unsafe extern "C" fn map(
    address: u64,
    size: usize,
    offset: usize,
    handle: u64,
    _: u64,
) -> CUresult {
    let result = unsafe {
        libc::mmap(
            address as *mut c_void,
            size,
            libc::PROT_NONE,
            libc::MAP_SHARED | libc::MAP_FIXED,
            (handle - 1) as i32,
            offset as libc::off_t,
        )
    };
    assert_eq!(result as u64, address);
    MAPPINGS.fetch_add(1, Ordering::Relaxed);
    CUDA_SUCCESS
}
unsafe extern "C" fn access(
    address: u64,
    size: usize,
    desc: *const CUmemAccessDesc,
    count: usize,
) -> CUresult {
    let desc = unsafe { &*desc };
    if count != 1 || desc.flags != CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE {
        return CUDA_ERROR_INVALID_VALUE;
    }
    match desc.location.type_ {
        CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA => {
            assert!([7, 57].contains(&desc.location.id));
            HOST_ACCESS.fetch_add(1, Ordering::Relaxed);
        }
        CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE if MIXED.load(Ordering::Relaxed) => {
            assert_eq!(desc.location.id, 0);
        }
        _ => return CUDA_ERROR_INVALID_VALUE,
    }
    assert_eq!(
        unsafe {
            libc::mprotect(
                address as *mut c_void,
                size,
                libc::PROT_READ | libc::PROT_WRITE,
            )
        },
        0
    );
    CUDA_SUCCESS
}
unsafe extern "C" fn unmap(address: u64, size: usize) -> CUresult {
    let result = unsafe {
        libc::mmap(
            address as *mut c_void,
            size,
            libc::PROT_NONE,
            libc::MAP_ANONYMOUS | libc::MAP_PRIVATE | libc::MAP_FIXED,
            -1,
            0,
        )
    };
    assert_eq!(result as u64, address);
    assert!(MAPPINGS.fetch_sub(1, Ordering::Relaxed) > 0);
    CUDA_SUCCESS
}
unsafe extern "C" fn free(address: u64, size: usize) -> CUresult {
    assert_eq!(unsafe { libc::munmap(address as *mut c_void, size) }, 0);
    assert!(RESERVATIONS.fetch_sub(1, Ordering::Relaxed) > 0);
    CUDA_SUCCESS
}
unsafe extern "C" fn stream(output: *mut *mut c_void, _: CUstream_flags) -> CUresult {
    unsafe { output.write(std::ptr::dangling_mut::<c_void>()) };
    CUDA_SUCCESS
}
unsafe extern "C" fn finish_stream(_: *mut c_void) -> CUresult {
    CUDA_SUCCESS
}
unsafe extern "C" fn to_host(
    host: *mut c_void,
    device: u64,
    size: usize,
    _: *mut c_void,
) -> CUresult {
    assert_eq!(REGISTRATIONS.load(Ordering::Relaxed), 1);
    unsafe { std::ptr::copy_nonoverlapping(device as *const u8, host.cast(), size) };
    DEVICE_COPIES.fetch_add(1, Ordering::Relaxed);
    CUDA_SUCCESS
}
unsafe extern "C" fn to_device(
    device: u64,
    host: *const c_void,
    size: usize,
    _: *mut c_void,
) -> CUresult {
    assert_eq!(REGISTRATIONS.load(Ordering::Relaxed), 1);
    unsafe { std::ptr::copy_nonoverlapping(host.cast::<u8>(), device as *mut u8, size) };
    DEVICE_COPIES.fetch_add(1, Ordering::Relaxed);
    CUDA_SUCCESS
}
unsafe extern "C" fn resolve(name: *const c_char) -> *mut c_void {
    match unsafe { CStr::from_ptr(name) }.to_bytes() {
        b"cuCtxGetCurrent" => current as *const () as *mut c_void,
        b"cuCtxSetCurrent" => switch as *const () as *mut c_void,
        b"cuDevicePrimaryCtxRetain" => retain as *const () as *mut c_void,
        b"cuDevicePrimaryCtxRelease_v2" => release_context as *const () as *mut c_void,
        b"cuMemHostRegister_v2" => register as *const () as *mut c_void,
        b"cuMemHostUnregister" => unregister as *const () as *mut c_void,
        b"cuMemCreate" => create as *const () as *mut c_void,
        b"cuMemRelease" => release as *const () as *mut c_void,
        b"cuMemAddressReserve" => reserve as *const () as *mut c_void,
        b"cuMemMap" => map as *const () as *mut c_void,
        b"cuMemSetAccess" => access as *const () as *mut c_void,
        b"cuMemUnmap" => unmap as *const () as *mut c_void,
        b"cuMemAddressFree" => free as *const () as *mut c_void,
        b"cuStreamCreate" if MIXED.load(Ordering::Relaxed) => stream as *const () as *mut c_void,
        b"cuStreamSynchronize" | b"cuStreamDestroy_v2" if MIXED.load(Ordering::Relaxed) => {
            finish_stream as *const () as *mut c_void
        }
        b"cuMemcpyHtoDAsync_v2" if MIXED.load(Ordering::Relaxed) => {
            to_device as *const () as *mut c_void
        }
        b"cuMemcpyDtoHAsync_v2" if MIXED.load(Ordering::Relaxed) => {
            to_host as *const () as *mut c_void
        }
        _ => crate::tests::unused_driver_symbol(),
    }
}

#[test]
fn host_numa_bytes_survive_backing_recreation() {
    roundtrip("host_numa_bytes_survive_backing_recreation", false);
}

#[test]
fn mixed_host_and_device_bytes_survive_backing_recreation() {
    roundtrip(
        "mixed_host_and_device_bytes_survive_backing_recreation",
        true,
    );
}

fn roundtrip(test: &str, mixed: bool) {
    if std::env::var_os("CUINTERPOSE_HOST_NUMA_UNIT_CHILD").is_none() {
        let status = std::process::Command::new(std::env::current_exe().unwrap())
            .arg("--exact")
            .arg(format!("memory::host_carrier::host_numa_tests::{test}"))
            .env("CUINTERPOSE_HOST_NUMA_UNIT_CHILD", "1")
            .status()
            .unwrap();
        assert!(status.success());
        return;
    }
    MIXED.store(mixed, Ordering::Relaxed);
    assert!(
        crate::G_FRONTEND_ABI
            .set(FrontendAbi {
                version: ABI_VERSION,
                size: size_of::<FrontendAbi>() as u32,
                resolve,
            })
            .is_ok()
    );
    crate::driver::initialize().unwrap();
    let mut locations = vec![
        (CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA, 7),
        (CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA, 57),
    ];
    if mixed {
        // Device 0 shares the host allocations' fallback CUDA context, but must
        // stay in a separate transfer group with its own copy mechanism.
        locations.insert(1, (CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE, 0));
    }
    let mut allocations = Vec::new();
    for (index, (location, location_id)) in locations.into_iter().enumerate() {
        let mut properties: CUmemAllocationProp = unsafe { std::mem::zeroed() };
        properties.type_ = CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED;
        properties.requestedHandleTypes =
            CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
        properties.location = CUmemLocation {
            type_: location,
            id: location_id,
        };
        let size = 4096 * (index + 1);
        let mut driver = 0;
        unsafe { crate::driver::cuMemCreate(&mut driver, size, &properties, 0) }.unwrap();
        let bytes = vec![0x31 + index as u8; size];
        assert_eq!(
            unsafe { libc::pwrite((driver - 1) as i32, bytes.as_ptr().cast(), size, 0) },
            size as isize
        );
        let mut allocation = Allocation {
            refcounts: Default::default(),
            reference: AllocationReference {
                creator_pid: 41,
                id: [index as u8 + 1; 16],
            },
            driver: Some(driver),
            size,
            properties,
            context: 0,
            shared: true,
        };
        let mut state = crate::memory::ProcessState::new(41);
        state.memblocks.insert(
            allocation.reference.id,
            crate::memory::Memblock::Unicast(allocation.clone()),
        );
        assert!(matches!(state.inspect().unwrap().as_slice(),
            [cuinterpose_protocol::Record::Allocation {
                location: cuinterpose_protocol::MemoryLocation { location_type: kind, id },
                checkpoint_via_host_carrier: true, ..
            }]
                if *kind == location as u32 && *id == location_id));
        assert!(allocation.checkpoint_via_host_carrier(41));
        assert!(!allocation.checkpoint_via_host_carrier(42));
        allocation.shared = false;
        assert!(!allocation.checkpoint_via_host_carrier(41));
        allocation.shared = true;
        allocations.push(AllocationContent::from(&allocation));
    }
    let arena = Arena::save(&allocations).unwrap().unwrap();
    assert_eq!(CREATES.load(Ordering::Relaxed), allocations.len());
    assert_copy_cleanup();
    // Destroy all original backing. Neither reusing old handles nor skipping
    // the content copy can satisfy the checks against the newly created files.
    for allocation in &mut allocations {
        unsafe { crate::driver::cuMemRelease(allocation.driver.take().unwrap()) }.unwrap();
    }
    assert_eq!(HANDLES.load(Ordering::Relaxed), 0);
    arena.load(&mut allocations).unwrap();
    assert_eq!(CREATES.load(Ordering::Relaxed), 2 * allocations.len());
    assert_copy_cleanup();
    for (index, allocation) in allocations.iter().enumerate() {
        let mut bytes = vec![0; allocation.size];
        let fd = (allocation.driver.unwrap() - 1) as i32;
        assert_eq!(
            unsafe { libc::pread(fd, bytes.as_mut_ptr().cast(), bytes.len(), 0) },
            bytes.len() as isize
        );
        assert_eq!(bytes, vec![0x31 + index as u8; allocation.size]);
        unsafe { crate::driver::cuMemRelease(allocation.driver.unwrap()) }.unwrap();
    }
    arena.release().unwrap();
    assert_eq!(HANDLES.load(Ordering::Relaxed), 0);
    assert_eq!(HOST_ACCESS.load(Ordering::Relaxed), 4);
    assert_eq!(
        DEVICE_COPIES.load(Ordering::Relaxed),
        if mixed { 2 } else { 0 }
    );
}

fn assert_copy_cleanup() {
    assert_eq!(REGISTRATIONS.load(Ordering::Relaxed), 0);
    assert_eq!(PRIMARY_REFS.load(Ordering::Relaxed), 0);
    assert_eq!(MAPPINGS.load(Ordering::Relaxed), 0);
    assert_eq!(RESERVATIONS.load(Ordering::Relaxed), 0);
    assert_eq!(CURRENT.load(Ordering::Relaxed), 0);
}
