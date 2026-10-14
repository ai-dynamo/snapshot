// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! CUDA Driver API helpers shared by scenarios, with or without the frontend.

use crate::sys::{
    self, CUcontext, CUdevice, CUdevice_attribute, CUdeviceptr, CUmemAccess_flags, CUmemAccessDesc,
    CUmemAllocationHandleType, CUmemAllocationProp, CUmemGenericAllocationHandle, CUmemLocation,
    CUmemLocationType,
};
use std::ffi::c_void;
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
