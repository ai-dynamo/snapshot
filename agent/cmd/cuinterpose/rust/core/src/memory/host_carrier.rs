// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Canonical bytes in CRIU-captured memory. Unpublished backing is rolled back
//! explicitly; CUDA cleanup never runs from Drop or in a fork child.

use super::vmm::Allocation;
use crate::driver::CudaError;
use crate::driver::{Context, Result};
use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_OUT_OF_MEMORY,
    CUDA_ERROR_UNKNOWN,
};
use cudarc::driver::sys::{
    CU_MEMHOSTREGISTER_PORTABLE, CUmemAccess_flags, CUmemAccessDesc, CUmemAllocationProp,
    CUstream_flags,
};
use cuinterpose_protocol::AllocationId;
use std::collections::BTreeMap;
use std::ffi::c_void;

/// Only the inputs needed to move bytes; virtual handles and mapping topology stay in ProcessState.
#[derive(Clone)]
pub struct AllocationContent {
    pub id: AllocationId,
    pub driver: Option<u64>,
    pub size: usize,
    pub properties: CUmemAllocationProp,
    pub context: usize,
}

impl From<&Allocation> for AllocationContent {
    fn from(allocation: &Allocation) -> Self {
        Self {
            id: allocation.reference.id,
            driver: allocation.driver,
            size: allocation.size,
            properties: allocation.properties,
            context: allocation.context,
        }
    }
}

pub struct Arena {
    pub(crate) base: usize,
    pub(crate) size: usize,
    context: usize,
    device: i32,
    offsets: BTreeMap<AllocationId, usize>,
}

impl Arena {
    pub fn save(allocations: &[AllocationContent]) -> Result<Option<Self>> {
        if allocations.is_empty() {
            return Ok(None);
        }
        let mut offsets = BTreeMap::new();
        let mut size = 0usize;
        for allocation in allocations {
            if allocation.driver.is_none()
                || allocation.size == 0
                || offsets.insert(allocation.id, size).is_some()
            {
                return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
            }
            size = size
                .checked_add(allocation.size)
                .ok_or(CUDA_ERROR_OUT_OF_MEMORY)?;
        }
        let base = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                size,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS,
                -1,
                0,
            )
        };
        if base == libc::MAP_FAILED {
            return Err(CudaError::from(CUDA_ERROR_OUT_OF_MEMORY));
        }
        let first = &allocations[0];
        let arena = Self {
            base: base as usize,
            size,
            context: first.context,
            device: first.properties.location.id,
            offsets,
        };
        let context = match Context::enter(arena.context, arena.device) {
            Ok(context) => context,
            Err(error) => {
                unsafe {
                    libc::munmap(base, size);
                }
                return Err(error);
            }
        };
        let registered =
            unsafe { crate::driver::cuMemHostRegister_v2(base, size, CU_MEMHOSTREGISTER_PORTABLE) };
        let left = context.leave();
        if let Err(error) = registered {
            unsafe {
                libc::munmap(base, size);
            }
            return Err(error);
        }
        if let Err(error) = left {
            let _ = arena.release();
            return Err(error);
        }
        match arena.copy(allocations, false) {
            Ok(()) => Ok(Some(arena)),
            Err(error) => {
                let _ = arena.release();
                Err(error)
            }
        }
    }

    /// Recreate device backing from the captured host arena.
    pub fn load(&self, allocations: &mut [AllocationContent]) -> Result<()> {
        let mut fresh = allocations.to_vec();
        let mut size = 0usize;
        for allocation in &fresh {
            if allocation.driver.is_some() || self.offsets.get(&allocation.id) != Some(&size) {
                return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
            }
            size = size
                .checked_add(allocation.size)
                .ok_or(CUDA_ERROR_INVALID_VALUE)?;
        }
        if size != self.size {
            return Err(CudaError::from(CUDA_ERROR_INVALID_VALUE));
        }
        let loaded = (|| -> Result<()> {
            Context::run(self.context, self.device, || {
                let mut flags = 0u32;
                let valid = unsafe {
                    crate::driver::cuMemHostGetFlags(&mut flags, self.base as *mut c_void)
                }
                .is_ok();
                if !valid {
                    unsafe {
                        crate::driver::cuMemHostRegister_v2(
                            self.base as *mut c_void,
                            self.size,
                            CU_MEMHOSTREGISTER_PORTABLE,
                        )
                    }?;
                }
                Ok(())
            })?;
            for allocation in &mut fresh {
                Context::run(
                    allocation.context,
                    allocation.properties.location.id,
                    || {
                        let mut driver = 0;
                        unsafe {
                            crate::driver::cuMemCreate(
                                &mut driver,
                                allocation.size,
                                &allocation.properties,
                                0,
                            )
                        }?;
                        allocation.driver = Some(driver);
                        Ok(())
                    },
                )?;
            }
            self.copy(&fresh, true)
        })();
        loaded?;
        for (allocation, fresh) in allocations.iter_mut().zip(fresh) {
            allocation.driver = fresh.driver;
        }
        Ok(())
    }

    fn copy(&self, allocations: &[AllocationContent], load: bool) -> Result<()> {
        let mut groups: BTreeMap<(usize, i32), Vec<&AllocationContent>> = BTreeMap::new();
        for allocation in allocations {
            groups
                .entry((allocation.context, allocation.properties.location.id))
                .or_default()
                .push(allocation);
        }
        for ((context, device), group) in groups {
            let total = group.iter().try_fold(0usize, |sum, a| {
                sum.checked_add(a.size).ok_or(CUDA_ERROR_OUT_OF_MEMORY)
            })?;
            let mut mapped = Vec::new();
            mapped
                .try_reserve_exact(group.len())
                .map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
            let context = Context::enter(context, device)?;
            let mut reserved = None;
            let mut stream = None;
            let mut synchronized = false;
            let transfer = (|| -> Result<()> {
                let mut base = 0u64;
                unsafe { crate::driver::cuMemAddressReserve(&mut base, total, 0, 0, 0) }?;
                reserved = Some(base);
                let mut offset = 0usize;
                for allocation in &group {
                    let address = base
                        .checked_add(offset as u64)
                        .ok_or(CUDA_ERROR_INVALID_VALUE)?;
                    unsafe {
                        crate::driver::cuMemMap(
                            address,
                            allocation.size,
                            0,
                            allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                            0,
                        )
                    }?;
                    mapped.push((address, allocation.size));
                    let access = CUmemAccessDesc {
                        location: allocation.properties.location,
                        flags: CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE,
                    };
                    unsafe { crate::driver::cuMemSetAccess(address, allocation.size, &access, 1) }?;
                    offset += allocation.size;
                }
                let mut raw_stream = std::ptr::null_mut::<c_void>();
                unsafe {
                    crate::driver::cuStreamCreate(
                        &mut raw_stream,
                        CUstream_flags::CU_STREAM_NON_BLOCKING,
                    )
                }?;
                stream = Some(raw_stream);
                (|| -> Result<()> {
                    for (allocation, (address, _)) in group.iter().zip(&mapped) {
                        let offset = *self
                            .offsets
                            .get(&allocation.id)
                            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                        let host = self
                            .base
                            .checked_add(offset)
                            .ok_or(CUDA_ERROR_INVALID_VALUE)?
                            as *mut c_void;
                        if load {
                            unsafe {
                                crate::driver::cuMemcpyHtoDAsync_v2(
                                    *address,
                                    host,
                                    allocation.size,
                                    raw_stream,
                                )
                            }?;
                        } else {
                            unsafe {
                                crate::driver::cuMemcpyDtoHAsync_v2(
                                    host,
                                    *address,
                                    allocation.size,
                                    raw_stream,
                                )
                            }?;
                        }
                    }
                    unsafe { crate::driver::cuStreamSynchronize(raw_stream) }?;
                    synchronized = true;
                    Ok(())
                })()
            })();
            // Evaluate every cleanup even if an earlier one failed. Preserve
            // the original operation error; cleanup failures still fail-stop.
            let mut result = transfer;
            if let Some(stream) = stream {
                if !synchronized {
                    let drained = unsafe { crate::driver::cuStreamSynchronize(stream) };
                    if drained.is_err() {
                        // Completion is unknown: neither rollback nor returning
                        // to a caller may free DMA-referenced memory. Fail-stop
                        // the process without running Rust/CUDA cleanup.
                        let message = b"cuinterpose: CUDA copy completion unknown; terminating without cleanup\n";
                        unsafe {
                            libc::write(
                                libc::STDERR_FILENO,
                                message.as_ptr().cast(),
                                message.len(),
                            );
                            libc::_exit(127);
                        }
                    }
                }
                result = result.and(unsafe { crate::driver::cuStreamDestroy_v2(stream) });
            }
            for (address, size) in mapped {
                result = result.and(unsafe { crate::driver::cuMemUnmap(address, size) });
            }
            if let Some(address) = reserved {
                result = result.and(unsafe { crate::driver::cuMemAddressFree(address, total) });
            }
            result = result.and(context.leave());
            result?;
        }
        Ok(())
    }

    pub fn release(self) -> Result<()> {
        let context = Context::enter(self.context, self.device)?;
        let unregistered = unsafe { crate::driver::cuMemHostUnregister(self.base as *mut c_void) };
        let left = context.leave();
        // Do not unmap an arena still registered with CUDA. If unregister
        // succeeded, a context-restoration error must not prevent CPU cleanup.
        let unmapped = if unregistered.is_ok() {
            if unsafe { libc::munmap(self.base as *mut c_void, self.size) } == 0 {
                Ok(())
            } else {
                Err(CudaError::from(CUDA_ERROR_UNKNOWN))
            }
        } else {
            Ok(())
        };
        unregistered.and(left).and(unmapped)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use cudarc::driver::sys::CUresult::{CUDA_ERROR_INVALID_CONTEXT, CUDA_SUCCESS};
    use cudarc::driver::sys::{
        CUmemAllocationHandleType, CUmemAllocationProp_st__bindgen_ty_1, CUmemAllocationType,
        CUmemLocation, CUmemLocationType, CUresult,
    };
    use cuinterpose_abi::{ABI_VERSION, FrontendAbi};
    use std::ffi::{CStr, c_char};
    use std::sync::atomic::{AtomicUsize, Ordering};

    static G_REGISTERED: AtomicUsize = AtomicUsize::new(0);
    static G_REGISTER_CALLS: AtomicUsize = AtomicUsize::new(0);
    static G_RELEASED: AtomicUsize = AtomicUsize::new(0);
    static G_SWITCHED: AtomicUsize = AtomicUsize::new(0);

    unsafe extern "C" fn current(output: *mut *mut c_void) -> CUresult {
        unsafe {
            output.write(std::ptr::dangling_mut::<c_void>());
        }
        CUDA_SUCCESS
    }
    unsafe extern "C" fn switch(_: *mut c_void) -> CUresult {
        G_SWITCHED.fetch_add(1, Ordering::Relaxed);
        CUDA_ERROR_INVALID_CONTEXT
    }
    unsafe extern "C" fn retain(output: *mut *mut c_void, _: i32) -> CUresult {
        unsafe {
            output.write(2usize as *mut c_void);
        }
        CUDA_SUCCESS
    }
    unsafe extern "C" fn release(_: i32) -> CUresult {
        G_RELEASED.fetch_add(1, Ordering::Relaxed);
        CUDA_ERROR_INVALID_HANDLE
    }
    unsafe extern "C" fn register(_: *mut c_void, _: usize, _: u32) -> CUresult {
        G_REGISTERED.fetch_add(1, Ordering::Relaxed);
        G_REGISTER_CALLS.fetch_add(1, Ordering::Relaxed);
        CUDA_SUCCESS
    }
    unsafe extern "C" fn unregister(_: *mut c_void) -> CUresult {
        G_REGISTERED.fetch_sub(1, Ordering::Relaxed);
        CUDA_SUCCESS
    }
    unsafe extern "C" fn create(
        _: *mut u64,
        _: usize,
        _: *const CUmemAllocationProp,
        _: u64,
    ) -> CUresult {
        CUDA_ERROR_OUT_OF_MEMORY
    }
    unsafe extern "C" fn resolve(name: *const c_char) -> *mut c_void {
        match unsafe { CStr::from_ptr(name) }.to_bytes() {
            b"cuCtxGetCurrent" => current as *const () as *mut c_void,
            b"cuCtxSetCurrent" => switch as *const () as *mut c_void,
            b"cuDevicePrimaryCtxRetain" => retain as *const () as *mut c_void,
            b"cuDevicePrimaryCtxRelease_v2" => release as *const () as *mut c_void,
            b"cuMemHostRegister_v2" => register as *const () as *mut c_void,
            b"cuMemHostUnregister" => unregister as *const () as *mut c_void,
            b"cuMemCreate" => create as *const () as *mut c_void,
            // Deliberately absent, not merely a driver error.
            b"cuMemHostGetFlags" => std::ptr::null_mut(),
            _ => std::ptr::null_mut(),
        }
    }

    #[test]
    fn failed_setup_releases_temporary_resources() {
        // The frontend ABI is process-lifetime production state. Keep this fake
        // resolver out of ABI-prefix tests, which require it to remain unset.
        if std::env::var_os("CUINTERPOSE_CARRIER_UNIT_CHILD").is_none() {
            let status = std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "memory::host_carrier::tests::failed_setup_releases_temporary_resources",
                ])
                .env("CUINTERPOSE_CARRIER_UNIT_CHILD", "1")
                .status()
                .unwrap();
            assert!(status.success());
            return;
        }
        assert!(
            crate::G_FRONTEND_ABI
                .set(FrontendAbi {
                    version: ABI_VERSION,
                    size: size_of::<FrontendAbi>() as u32,
                    resolve,
                })
                .is_ok()
        );
        crate::driver::initialize();
        Context::enter(1, 0).unwrap().leave().unwrap();
        assert_eq!(G_SWITCHED.load(Ordering::Relaxed), 0);
        assert_eq!(
            Context::enter(0, 0).err(),
            Some(crate::driver::CudaError(CUDA_ERROR_INVALID_CONTEXT))
        );
        assert_eq!(G_RELEASED.load(Ordering::Relaxed), 1);
        let id: AllocationId = [1; 16];
        let arena = Arena {
            base: 0x1000,
            size: 4096,
            context: 1,
            device: 0,
            offsets: BTreeMap::from([(id, 0)]),
        };
        let mut allocations = [AllocationContent {
            id,
            driver: None,
            size: 4096,
            properties: CUmemAllocationProp {
                type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
                requestedHandleTypes:
                    CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
                location: CUmemLocation {
                    type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                    id: 0,
                },
                win32HandleMetaData: std::ptr::null_mut(),
                allocFlags: CUmemAllocationProp_st__bindgen_ty_1 {
                    compressionType: 0,
                    gpuDirectRDMACapable: 0,
                    usage: 0,
                    reserved: [0; 4],
                },
            },
            context: 1,
        }];
        assert_eq!(
            arena.load(&mut allocations),
            Err(crate::driver::CudaError(CUDA_ERROR_OUT_OF_MEMORY))
        );
        assert_eq!(G_REGISTER_CALLS.load(Ordering::Relaxed), 1);
        // Create failed after host registration. Fail-stop does not unwind it.
        assert_eq!(G_REGISTERED.load(Ordering::Relaxed), 1);
        assert_eq!(allocations[0].driver, None);
    }
}
