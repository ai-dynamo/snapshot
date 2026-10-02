// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Canonical bytes in CRIU-captured memory. Restore failures terminate the
//! process; CUDA cleanup never runs from Drop or in a fork child.

use super::vmm::{Allocation, context_device};
use crate::driver::Context;
use crate::error::{Error, Result};
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
    /// Any error is fatal to restore; the lifecycle caller terminates the process.
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
            let mut driver = 0;
            unsafe {
                crate::driver::cuMemCreate(&mut driver, allocation.size, &allocation.properties, 0)
            }?;
            allocation.driver = Some(driver);
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
                // Copy the full backing independently of application mappings,
                // which may be absent, partial, or lack the access we need.
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
