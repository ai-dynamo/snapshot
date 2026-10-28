// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! CUDA Driver API helpers shared by scenarios, with or without the frontend.

use crate::sys::{
    self, CUcontext, CUdevice, CUdevice_attribute, CUdeviceptr, CUmemAccess_flags, CUmemAccessDesc,
    CUmemAllocationHandleType, CUmemAllocationProp, CUmemGenericAllocationHandle, CUmemLocation,
    CUmemLocationType,
};
use std::ffi::{CStr, c_void};
use std::os::fd::{FromRawFd, OwnedFd};

/// Calls a CUDA entry point and panics with its name and status unless it succeeds.
#[macro_export]
macro_rules! cu {
    ($function:ident($($argument:expr),* $(,)?)) => {{
        // SAFETY: callers pass arguments that are valid for this CUDA entry point.
        let status = unsafe { $crate::sys::$function($($argument),*) };
        assert_eq!(
            status,
            $crate::sys::CUresult::CUDA_SUCCESS,
            concat!(stringify!($function), " failed")
        );
    }};
}

pub const POSIX_FD: CUmemAllocationHandleType =
    CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
pub const READWRITE: CUmemAccess_flags = CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
pub const READ: CUmemAccess_flags = CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READ;
pub const NONE: CUmemAccess_flags = CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_NONE;

// The shim's virtual allocation handles carry this tag in the top 16 bits.
const VIRTUAL_HANDLE_TAG: u64 = 0xD94D_0000_0000_0000;
const VIRTUAL_HANDLE_MASK: u64 = 0xFFFF_0000_0000_0000;

/// Initializes CUDA, retains the first `count` primary contexts, and makes GPU 0's current.
pub fn primary_contexts(count: i32) -> Vec<CUcontext> {
    cu!(cuInit(0));
    let contexts: Vec<_> = (0..count)
        .map(|device| {
            let mut context = std::ptr::null_mut();
            cu!(cuDevicePrimaryCtxRetain(&mut context, device));
            context
        })
        .collect();
    cu!(cuCtxSetCurrent(contexts[0]));
    contexts
}

/// Retains `device`'s primary context and makes it current.
pub fn make_primary_current(device: CUdevice) -> CUcontext {
    let mut context = std::ptr::null_mut();
    cu!(cuDevicePrimaryCtxRetain(&mut context, device));
    cu!(cuCtxSetCurrent(context));
    context
}

/// Creates a context on `device` and makes it current.
pub fn create_context(device: CUdevice) -> CUcontext {
    let mut context = std::ptr::null_mut();
    cu!(cuCtxCreate_v4(
        &mut context,
        std::ptr::null_mut(),
        0,
        device
    ));
    context
}

pub fn assert_handle_namespace(handle: CUmemGenericAllocationHandle, virtual_: bool, stage: &str) {
    let tagged = handle & VIRTUAL_HANDLE_MASK == VIRTUAL_HANDLE_TAG;
    let expected = if virtual_ { "virtual" } else { "raw" };
    assert_eq!(
        tagged, virtual_,
        "{stage}: handle {handle:#x} is not {expected}"
    );
}

pub fn device_attribute(attribute: CUdevice_attribute, device: i32) -> i32 {
    let mut value = 0;
    cu!(cuDeviceGetAttribute(&mut value, attribute, device));
    value
}

/// The GPU's nearest host NUMA node. HOST_NUMA uses node 0 on hosts without NUMA.
fn host_numa_node(device: i32) -> i32 {
    device_attribute(CUdevice_attribute::CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID, device).max(0)
}

pub fn device(id: i32) -> CUmemLocation {
    CUmemLocation {
        type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
        id,
    }
}

/// Pinned POSIX-shareable properties on a GPU or on its nearest host NUMA node.
pub fn allocation_properties(device: i32, host_numa: bool) -> CUmemAllocationProp {
    // SAFETY: CUmemAllocationProp is plain data, and zero is the default of each field.
    let mut properties: CUmemAllocationProp = unsafe { std::mem::zeroed() };
    properties.type_ = sys::CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED;
    properties.location = if host_numa {
        CUmemLocation {
            type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA,
            id: host_numa_node(device),
        }
    } else {
        self::device(device)
    };
    properties.requestedHandleTypes = POSIX_FD;
    properties
}

/// The properties reported for `handle`.
pub fn properties_of(handle: CUmemGenericAllocationHandle) -> CUmemAllocationProp {
    // SAFETY: CUmemAllocationProp is plain data.
    let mut properties: CUmemAllocationProp = unsafe { std::mem::zeroed() };
    cu!(cuMemGetAllocationPropertiesFromHandle(
        &mut properties,
        handle
    ));
    properties
}

pub fn granularity(properties: &CUmemAllocationProp) -> usize {
    let mut granularity = 0;
    let minimum = sys::CUmemAllocationGranularity_flags::CU_MEM_ALLOC_GRANULARITY_MINIMUM;
    cu!(cuMemGetAllocationGranularity(
        &mut granularity,
        properties,
        minimum
    ));
    granularity
}

/// Applies `(location, flags)` grants to a mapped range in one call.
pub fn set_access(
    address: CUdeviceptr,
    size: usize,
    grants: &[(CUmemLocation, CUmemAccess_flags)],
) {
    let descriptors: Vec<_> = grants
        .iter()
        .map(|&(location, flags)| CUmemAccessDesc { location, flags })
        .collect();
    cu!(cuMemSetAccess(
        address,
        size,
        descriptors.as_ptr(),
        descriptors.len()
    ));
}

pub fn get_access(address: CUdeviceptr, device: i32) -> CUmemAccess_flags {
    let mut flags = 0;
    cu!(cuMemGetAccess(&mut flags, &self::device(device), address));
    [NONE, READ, READWRITE]
        .into_iter()
        .find(|access| *access as u64 == flags)
        .unwrap_or_else(|| panic!("unknown access flags {flags}"))
}

/// Reserves, maps, and grants read-write access to `location`.
pub fn map(
    handle: CUmemGenericAllocationHandle,
    size: usize,
    location: CUmemLocation,
) -> CUdeviceptr {
    let mut address = 0;
    // The largest power of two dividing size, which is size for multicast granules.
    cu!(cuMemAddressReserve(
        &mut address,
        size,
        size & size.wrapping_neg(),
        0,
        0
    ));
    cu!(cuMemMap(address, size, 0, handle, 0));
    set_access(address, size, &[(location, READWRITE)]);
    address
}

pub fn unmap(address: CUdeviceptr, size: usize) {
    cu!(cuMemUnmap(address, size));
    cu!(cuMemAddressFree(address, size));
}

/// Creates and maps an allocation of `size`, or of one granule.
pub fn create_mapped(
    properties: &CUmemAllocationProp,
    size: Option<usize>,
) -> (CUmemGenericAllocationHandle, CUdeviceptr, usize) {
    let size = size.unwrap_or_else(|| granularity(properties));
    let mut handle = 0;
    cu!(cuMemCreate(&mut handle, size, properties, 0));
    (handle, map(handle, size, properties.location), size)
}

pub fn malloc(size: usize) -> CUdeviceptr {
    let mut address = 0;
    cu!(cuMemAlloc_v2(&mut address, size));
    address
}

/// The base and size of the allocation that contains `address`.
pub fn address_range(address: CUdeviceptr) -> (CUdeviceptr, usize) {
    let (mut base, mut size) = (0, 0);
    cu!(cuMemGetAddressRange_v2(&mut base, &mut size, address));
    (base, size)
}

/// Retains the handle of the allocation mapped at `address`.
pub fn retain(address: CUdeviceptr) -> CUmemGenericAllocationHandle {
    let mut handle = 0;
    cu!(cuMemRetainAllocationHandle(
        &mut handle,
        address as *mut c_void
    ));
    handle
}

/// Asserts that `address` looks like cudaMalloc memory, or its IPC import, to libraries
/// that probe it. NCCL, for example, registers retainable memory through cuMem exports,
/// which a converted malloc must not enable.
pub fn assert_malloc_semantics(address: CUdeviceptr, import: bool, stage: &str) {
    let mut handle = 0;
    // SAFETY: handle is writable and the address is only inspected.
    let status = unsafe { sys::cuMemRetainAllocationHandle(&mut handle, address as *mut c_void) };
    assert_eq!(
        status,
        sys::CUresult::CUDA_ERROR_INVALID_VALUE,
        "{stage}: malloc {address:#x} was retainable"
    );
    let mut capable = 0u32;
    cu!(cuPointerGetAttribute(
        (&raw mut capable).cast(),
        sys::CUpointer_attribute::CU_POINTER_ATTRIBUTE_IS_LEGACY_CUDA_IPC_CAPABLE,
        address
    ));
    assert_eq!(
        capable != 0,
        !import,
        "{stage}: wrong legacy IPC capability for {address:#x}"
    );
}

/// Exports a POSIX descriptor. Under the shim, any export marks the backing shared.
pub fn export_fd(handle: CUmemGenericAllocationHandle) -> OwnedFd {
    let mut fd: i32 = -1;
    cu!(cuMemExportToShareableHandle(
        (&raw mut fd).cast(),
        handle,
        POSIX_FD,
        0
    ));
    // SAFETY: the driver returned a new descriptor that this process owns.
    unsafe { OwnedFd::from_raw_fd(fd) }
}

pub fn import_fd(fd: &OwnedFd) -> CUmemGenericAllocationHandle {
    use std::os::fd::AsRawFd;
    let mut handle = 0;
    let shareable = fd.as_raw_fd() as isize as *mut c_void;
    cu!(cuMemImportFromShareableHandle(
        &mut handle,
        shareable,
        POSIX_FD
    ));
    handle
}

/// Exports and closes a descriptor so the shim reconstructs this backing from a carrier.
pub fn mark_shared(handle: CUmemGenericAllocationHandle) {
    drop(export_fd(handle));
}

/// Properties of a two-GPU POSIX-shareable multicast object of one minimum granule.
pub fn multicast_properties() -> sys::CUmulticastObjectProp {
    let mut properties = sys::CUmulticastObjectProp {
        numDevices: 2,
        size: 0,
        handleTypes: u64::from(POSIX_FD.0),
        flags: 0,
    };
    let minimum = sys::CUmulticastGranularity_flags::CU_MULTICAST_GRANULARITY_MINIMUM;
    cu!(cuMulticastGetGranularity(
        &mut properties.size,
        &properties,
        minimum
    ));
    properties
}

/// A two-GPU POSIX-shareable multicast object of one minimum granule, with `devices`
/// added. Returns the handle and its size.
pub fn multicast_group(devices: &[i32]) -> (CUmemGenericAllocationHandle, usize) {
    let properties = multicast_properties();
    let mut group = 0;
    cu!(cuMulticastCreate(&mut group, &properties));
    for &device in devices {
        cu!(cuMulticastAddDevice(group, device));
    }
    (group, properties.size)
}

pub fn write_bytes(address: CUdeviceptr, data: &[u8]) {
    cu!(cuMemcpyHtoD_v2(address, data.as_ptr().cast(), data.len()));
}

pub fn read_bytes(address: CUdeviceptr, size: usize) -> Vec<u8> {
    let mut data = vec![0; size];
    cu!(cuMemcpyDtoH_v2(data.as_mut_ptr().cast(), address, size));
    data
}

pub fn assert_bytes(address: CUdeviceptr, expected: &[u8], stage: &str) {
    let actual = read_bytes(address, expected.len());
    assert_eq!(actual, expected, "{stage}");
}

pub fn read_u32(address: CUdeviceptr) -> u32 {
    u32::from_le_bytes(read_bytes(address, 4).try_into().unwrap())
}

// HOST_NUMA mappings grant access only to the CPU, so these bypass driver copies.

/// # Safety
/// `data.len()` bytes at `address` must be mapped with CPU write access.
pub unsafe fn write_host(address: CUdeviceptr, data: &[u8]) {
    unsafe { std::ptr::copy_nonoverlapping(data.as_ptr(), address as *mut u8, data.len()) };
}

/// # Safety
/// `size` bytes at `address` must be mapped with CPU read access.
pub unsafe fn read_host(address: CUdeviceptr, size: usize) -> Vec<u8> {
    unsafe { std::slice::from_raw_parts(address as *const u8, size) }.to_vec()
}

/// Runs a one-thread kernel that takes only 64-bit pointer parameters.
pub fn launch(ptx: &CStr, entry: &CStr, pointers: &[CUdeviceptr]) {
    let mut module = std::ptr::null_mut();
    cu!(cuModuleLoadData(&mut module, ptx.as_ptr().cast()));
    let mut kernel = std::ptr::null_mut();
    cu!(cuModuleGetFunction(&mut kernel, module, entry.as_ptr()));
    let mut values = pointers.to_vec();
    let mut parameters: Vec<*mut c_void> = values
        .iter_mut()
        .map(|value| (value as *mut CUdeviceptr).cast())
        .collect();
    let stream = std::ptr::null_mut();
    cu!(cuLaunchKernel(
        kernel,
        1,
        1,
        1,
        1,
        1,
        1,
        0,
        stream,
        parameters.as_mut_ptr(),
        std::ptr::null_mut()
    ));
    cu!(cuCtxSynchronize());
    cu!(cuModuleUnload(module));
}

const INCREMENT: &CStr = c"
.version 7.0
.target sm_75
.address_size 64
.visible .entry increment(.param .u64 address) {
    .reg .u64 pointer;
    .reg .u32 value;
    ld.param.u64 pointer, [address];
    ld.global.u32 value, [pointer];
    add.u32 value, value, 1;
    st.global.u32 [pointer], value;
    ret;
}
";

/// Increments a u32 from a kernel in the current context.
///
/// A kernel dereferences the pointer directly, whereas a driver copy can take another path
/// and succeed even when this GPU cannot access the memory.
pub fn increment(address: CUdeviceptr) {
    launch(INCREMENT, c"increment", &[address]);
}

// Ordinary loads from a multicast address are undefined, so read through multimem.
const MULTIMEM_SUM: &CStr = c"
.version 8.1
.target sm_90
.address_size 64
.visible .entry multimem_sum(.param .u64 source, .param .u64 destination) {
    .reg .b64 %source, %destination;
    .reg .b32 %value;
    ld.param.u64 %source, [source];
    ld.param.u64 %destination, [destination];
    multimem.ld_reduce.acquire.sys.global.add.u32 %value, [%source];
    st.global.u32 [%destination], %value;
    ret;
}
";

/// The sum over every bound member of the u32 at a multicast address.
pub fn multimem_sum(address: CUdeviceptr) -> u32 {
    let output = malloc(4);
    launch(MULTIMEM_SUM, c"multimem_sum", &[address, output]);
    let sum = read_u32(output);
    cu!(cuMemFree_v2(output));
    sum
}
