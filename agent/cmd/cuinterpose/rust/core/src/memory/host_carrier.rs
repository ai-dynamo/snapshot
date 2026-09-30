// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Canonical bytes in CRIU-captured memory. Unpublished backing is rolled back
//! explicitly; CUDA cleanup never runs from Drop or in a fork child.

use super::vmm::{Allocation, context_device};
use crate::error::Error;
use crate::driver::Context;
use crate::error::{Result};
use cudarc::driver::sys::CUresult::{
    CUDA_ERROR_INVALID_HANDLE, CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_OUT_OF_MEMORY,
    CUDA_ERROR_UNKNOWN,
};
use cudarc::driver::sys::{
    CU_MEMHOSTREGISTER_PORTABLE, CUmemAccess_flags, CUmemAccessDesc, CUmemAllocationProp,
    CUmemLocationType, CUstream_flags,
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
                return Err(Error::from(CUDA_ERROR_INVALID_VALUE));
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
            return Err(Error::from(CUDA_ERROR_OUT_OF_MEMORY));
        }
        let arena = Self {
            base: base as usize,
            size,
            offsets,
        };
        match arena.copy(allocations, false) {
            Ok(()) => Ok(Some(arena)),
            Err(error) => {
                let _ = arena.release();
                Err(error)
            }
        }
    }

    /// Recreate shared backing from the captured host arena.
    pub fn load(&self, allocations: &mut [AllocationContent]) -> Result<()> {
        let mut fresh = allocations.to_vec();
        let mut size = 0usize;
        for allocation in &fresh {
            if allocation.driver.is_some() || self.offsets.get(&allocation.id) != Some(&size) {
                return Err(Error::from(CUDA_ERROR_INVALID_VALUE));
            }
            size = size
                .checked_add(allocation.size)
                .ok_or(CUDA_ERROR_INVALID_VALUE)?;
        }
        if size != self.size {
            return Err(Error::from(CUDA_ERROR_INVALID_VALUE));
        }
        for allocation in &mut fresh {
            Context::run(
                allocation.context,
                context_device(&allocation.properties),
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
        self.copy(&fresh, true)?;
        for (allocation, fresh) in allocations.iter_mut().zip(fresh) {
            allocation.driver = fresh.driver;
        }
        Ok(())
    }

    fn copy(&self, allocations: &[AllocationContent], load: bool) -> Result<()> {
        let first = allocations.first().ok_or(CUDA_ERROR_INVALID_VALUE)?;
        Context::run(first.context, context_device(&first.properties), || {
            // A fallback primary may lose its final retain when we leave. Keep
            // registration in this scope; PORTABLE covers every copy group.
            unsafe {
                crate::driver::cuMemHostRegister_v2(
                    self.base as *mut c_void,
                    self.size,
                    CU_MEMHOSTREGISTER_PORTABLE,
                )
            }?;
            let result = self.copy_groups(allocations, load);
            // Failed cleanup must not let save's error path unmap storage that
            // CUDA still considers registered.
            crate::runtime::must_complete(unsafe {
                crate::driver::cuMemHostUnregister(self.base as *mut c_void)
            });
            result
        })
    }

    fn copy_groups(&self, allocations: &[AllocationContent], load: bool) -> Result<()> {
        let mut groups: BTreeMap<(usize, i32), Vec<&AllocationContent>> = BTreeMap::new();
        for allocation in allocations {
            if allocation.properties.location.type_
                == CUmemLocationType::CU_MEM_LOCATION_TYPE_HOST_NUMA
            {
                self.copy_host(allocation, load)?;
                continue;
            }
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

    /// Copy host backing through a CPU-accessible alias of the full allocation.
    /// Application mappings may be partial or have no host access; leave their
    /// addresses and permissions untouched while all writers are parked.
    fn copy_host(&self, allocation: &AllocationContent, load: bool) -> Result<()> {
        let offset = *self
            .offsets
            .get(&allocation.id)
            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
        let host = self
            .base
            .checked_add(offset)
            .ok_or(CUDA_ERROR_INVALID_VALUE)?;
        let mut address = 0;
        unsafe { crate::driver::cuMemAddressReserve(&mut address, allocation.size, 0, 0, 0) }?;
        let mut mapped = false;
        let transfer = (|| -> Result<()> {
            unsafe {
                crate::driver::cuMemMap(
                    address,
                    allocation.size,
                    0,
                    allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                    0,
                )
            }?;
            mapped = true;
            let access = CUmemAccessDesc {
                location: allocation.properties.location,
                flags: CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE,
            };
            unsafe { crate::driver::cuMemSetAccess(address, allocation.size, &access, 1) }?;
            // CUDA work is drained before the lifecycle starts. The alias and
            // arena are disjoint CPU mappings, so no device copy is needed.
            let (source, destination) = if load {
                (host, address as usize)
            } else {
                (address as usize, host)
            };
            unsafe {
                std::ptr::copy_nonoverlapping(
                    source as *const u8,
                    destination as *mut u8,
                    allocation.size,
                );
            }
            Ok(())
        })();
        let mut result = transfer;
        if mapped {
            result = result.and(unsafe { crate::driver::cuMemUnmap(address, allocation.size) });
        }
        result.and(unsafe { crate::driver::cuMemAddressFree(address, allocation.size) })
    }

    pub fn release(self) -> Result<()> {
        if unsafe { libc::munmap(self.base as *mut c_void, self.size) } == 0 {
            Ok(())
        } else {
            Err(Error::from(CUDA_ERROR_UNKNOWN))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use cudarc::driver::sys::CUresult::{CUDA_ERROR_INVALID_CONTEXT, CUDA_SUCCESS};
    use cudarc::driver::sys::{
        CUmemAllocationHandleType, CUmemAllocationProp_st__bindgen_ty_1, CUmemAllocationType,
        CUmemLocation, CUresult,
    };
    use cuinterpose_abi::{ABI_VERSION, FrontendAbi};
    use std::ffi::{CStr, c_char};
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    static G_REGISTERED: AtomicUsize = AtomicUsize::new(0);
    static G_REGISTER_CALLS: AtomicUsize = AtomicUsize::new(0);
    static G_RELEASED: AtomicUsize = AtomicUsize::new(0);
    static G_SWITCHED: AtomicUsize = AtomicUsize::new(0);
    static G_TRANSFER: AtomicBool = AtomicBool::new(false);
    static G_CURRENT: AtomicUsize = AtomicUsize::new(1);
    static G_PRIMARY_REFS: AtomicUsize = AtomicUsize::new(0);
    static G_COPIES: AtomicUsize = AtomicUsize::new(0);
    static G_FAIL_COPY: AtomicBool = AtomicBool::new(false);
    static G_FAIL_REGISTER: AtomicBool = AtomicBool::new(false);

    unsafe extern "C" fn current(output: *mut *mut c_void) -> CUresult {
        unsafe {
            output.write(G_CURRENT.load(Ordering::Relaxed) as *mut c_void);
        }
        CUDA_SUCCESS
    }
    unsafe extern "C" fn switch(context: *mut c_void) -> CUresult {
        G_SWITCHED.fetch_add(1, Ordering::Relaxed);
        if G_TRANSFER.load(Ordering::Relaxed) {
            G_CURRENT.store(context as usize, Ordering::Relaxed);
            CUDA_SUCCESS
        } else {
            CUDA_ERROR_INVALID_CONTEXT
        }
    }
    unsafe extern "C" fn retain(output: *mut *mut c_void, _: i32) -> CUresult {
        G_PRIMARY_REFS.fetch_add(1, Ordering::Relaxed);
        unsafe {
            output.write(2usize as *mut c_void);
        }
        CUDA_SUCCESS
    }
    unsafe extern "C" fn release(_: i32) -> CUresult {
        G_RELEASED.fetch_add(1, Ordering::Relaxed);
        if G_TRANSFER.load(Ordering::Relaxed) {
            if G_PRIMARY_REFS.fetch_sub(1, Ordering::Relaxed) == 1 {
                assert_eq!(
                    G_REGISTERED.load(Ordering::Relaxed),
                    0,
                    "final primary release invalidates host registration"
                );
            }
            CUDA_SUCCESS
        } else {
            CUDA_ERROR_INVALID_HANDLE
        }
    }
    unsafe extern "C" fn register(_: *mut c_void, _: usize, _: u32) -> CUresult {
        if G_FAIL_REGISTER.load(Ordering::Relaxed) {
            return CUDA_ERROR_OUT_OF_MEMORY;
        }
        G_REGISTERED.fetch_add(1, Ordering::Relaxed);
        G_REGISTER_CALLS.fetch_add(1, Ordering::Relaxed);
        CUDA_SUCCESS
    }
    unsafe extern "C" fn unregister(_: *mut c_void) -> CUresult {
        assert_eq!(G_REGISTERED.fetch_sub(1, Ordering::Relaxed), 1);
        assert!(G_PRIMARY_REFS.load(Ordering::Relaxed) > 0);
        CUDA_SUCCESS
    }
    unsafe extern "C" fn create(
        output: *mut u64,
        _: usize,
        _: *const CUmemAllocationProp,
        _: u64,
    ) -> CUresult {
        if G_TRANSFER.load(Ordering::Relaxed) {
            unsafe { output.write(42) };
            CUDA_SUCCESS
        } else {
            CUDA_ERROR_OUT_OF_MEMORY
        }
    }
    unsafe extern "C" fn reserve(out: *mut u64, _: usize, _: usize, _: u64, _: u64) -> CUresult {
        unsafe { out.write(0x10000) };
        CUDA_SUCCESS
    }
    unsafe extern "C" fn map(_: u64, _: usize, _: usize, _: u64, _: u64) -> CUresult {
        CUDA_SUCCESS
    }
    unsafe extern "C" fn access(_: u64, _: usize, _: *const CUmemAccessDesc, _: usize) -> CUresult {
        CUDA_SUCCESS
    }
    unsafe extern "C" fn free_mapping(_: u64, _: usize) -> CUresult {
        CUDA_SUCCESS
    }
    unsafe extern "C" fn stream(out: *mut *mut c_void, _: CUstream_flags) -> CUresult {
        unsafe { out.write(std::ptr::dangling_mut::<c_void>()) };
        CUDA_SUCCESS
    }
    unsafe extern "C" fn finish_stream(_: *mut c_void) -> CUresult {
        CUDA_SUCCESS
    }
    fn copied() -> CUresult {
        assert_eq!(G_REGISTERED.load(Ordering::Relaxed), 1);
        assert!(G_PRIMARY_REFS.load(Ordering::Relaxed) > 0);
        G_COPIES.fetch_add(1, Ordering::Relaxed);
        if G_FAIL_COPY.load(Ordering::Relaxed) {
            CUDA_ERROR_INVALID_VALUE
        } else {
            CUDA_SUCCESS
        }
    }
    unsafe extern "C" fn to_host(_: *mut c_void, _: u64, _: usize, _: *mut c_void) -> CUresult {
        copied()
    }
    unsafe extern "C" fn to_device(_: u64, _: *const c_void, _: usize, _: *mut c_void) -> CUresult {
        copied()
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
            b"cuMemAddressReserve" => reserve as *const () as *mut c_void,
            b"cuMemMap" => map as *const () as *mut c_void,
            b"cuMemSetAccess" => access as *const () as *mut c_void,
            b"cuMemUnmap" | b"cuMemAddressFree" => free_mapping as *const () as *mut c_void,
            b"cuStreamCreate" => stream as *const () as *mut c_void,
            b"cuStreamSynchronize" | b"cuStreamDestroy_v2" => {
                finish_stream as *const () as *mut c_void
            }
            b"cuMemcpyHtoDAsync_v2" => to_device as *const () as *mut c_void,
            b"cuMemcpyDtoHAsync_v2" => to_host as *const () as *mut c_void,
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
        assert!(matches!(
            Context::enter(0, 0).err(),
            Some(crate::error::Error::Cuda(CUDA_ERROR_INVALID_CONTEXT))
        ));
        assert_eq!(G_RELEASED.load(Ordering::Relaxed), 1);
        let id: AllocationId = [1; 16];
        let arena = Arena {
            base: 0x1000,
            size: 4096,
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
        assert!(matches!(
            arena.load(&mut allocations),
            Err(crate::error::Error::Cuda(CUDA_ERROR_OUT_OF_MEMORY))
        ));
        // No transfer means no host registration to clean up.
        assert_eq!(G_REGISTER_CALLS.load(Ordering::Relaxed), 0);
        assert_eq!(G_REGISTERED.load(Ordering::Relaxed), 0);
        assert_eq!(allocations[0].driver, None);
    }

    #[test]
    fn fallback_registration_lives_only_during_transfer() {
        if std::env::var_os("CUINTERPOSE_CARRIER_UNIT_CHILD").is_none() {
            let status = std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "memory::host_carrier::tests::fallback_registration_lives_only_during_transfer",
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
        G_TRANSFER.store(true, Ordering::Relaxed);
        let mut allocations: Vec<_> = [0, 3]
            .into_iter()
            .enumerate()
            .map(|(i, context)| AllocationContent {
                id: [i as u8 + 1; 16],
                driver: Some(42),
                size: 4096,
                properties: CUmemAllocationProp {
                    type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
                    location: CUmemLocation {
                        type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                        id: i as i32,
                    },
                    ..unsafe { std::mem::zeroed() }
                },
                context,
            })
            .collect();
        let arena = Arena::save(&allocations).unwrap().unwrap();
        assert_eq!(
            G_REGISTER_CALLS.load(Ordering::Relaxed),
            1,
            "portable registration spans both context groups"
        );
        assert_eq!(G_COPIES.load(Ordering::Relaxed), 2);
        assert_eq!(G_REGISTERED.load(Ordering::Relaxed), 0);
        assert_eq!(G_PRIMARY_REFS.load(Ordering::Relaxed), 0);
        assert_eq!(G_CURRENT.load(Ordering::Relaxed), 1);
        for allocation in &mut allocations {
            allocation.driver = None;
        }
        arena.load(&mut allocations).unwrap();
        assert!(
            allocations
                .iter()
                .all(|allocation| allocation.driver == Some(42))
        );
        assert_eq!(G_REGISTER_CALLS.load(Ordering::Relaxed), 2);
        assert_eq!(G_COPIES.load(Ordering::Relaxed), 4);
        assert_eq!(G_REGISTERED.load(Ordering::Relaxed), 0);
        assert_eq!(G_PRIMARY_REFS.load(Ordering::Relaxed), 0);
        assert_eq!(G_CURRENT.load(Ordering::Relaxed), 1);
        arena.release().unwrap();

        G_FAIL_COPY.store(true, Ordering::Relaxed);
        assert!(matches!(
            Arena::save(&allocations).err(),
            Some(Error::Cuda(CUDA_ERROR_INVALID_VALUE))
        ));
        assert_eq!(G_REGISTERED.load(Ordering::Relaxed), 0);
        assert_eq!(G_PRIMARY_REFS.load(Ordering::Relaxed), 0);
        assert_eq!(G_CURRENT.load(Ordering::Relaxed), 1);
        G_FAIL_COPY.store(false, Ordering::Relaxed);
        G_FAIL_REGISTER.store(true, Ordering::Relaxed);
        assert!(matches!(
            Arena::save(&allocations).err(),
            Some(Error::Cuda(CUDA_ERROR_OUT_OF_MEMORY))
        ));
        assert_eq!(G_REGISTERED.load(Ordering::Relaxed), 0);
        assert_eq!(G_PRIMARY_REFS.load(Ordering::Relaxed), 0);
        assert_eq!(G_CURRENT.load(Ordering::Relaxed), 1);
    }
}

#[cfg(test)]
#[path = "host_carrier_host_tests.rs"]
mod host_numa_tests;
