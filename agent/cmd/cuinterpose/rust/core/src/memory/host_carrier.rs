// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The host carrier stores allocation bytes in memory that CRIU captures. Restore
//! failures terminate the process, and CUDA cleanup must never run from Drop or in a
//! fork child.

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

/// Inputs needed to copy allocation bytes. ProcessState retains virtual handles and
/// mapping relationships.
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

    /// Recreate shared backing from the captured host arena. The lifecycle caller
    /// terminates the process if any operation fails.
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
            // CUDA reports NONE on an imported handle. Reconstruction still needs
            // exportable backing so other holders can import this owner's new copy.
            let mut properties = allocation.properties;
            properties.requestedHandleTypes =
                cudarc::driver::sys::CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
            unsafe { crate::driver::cuMemCreate(&mut driver, allocation.size, &properties, 0) }?;
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
            // Leaving this scope can release the last reference to a fallback primary
            // context, so host registration must end within the same scope. PORTABLE
            // lets one registration cover every copy group.
            unsafe {
                crate::driver::cuMemHostRegister_v2(
                    self.base as *mut c_void,
                    self.size,
                    CU_MEMHOSTREGISTER_PORTABLE,
                )
            }?;
            let result = self.copy_groups(allocations, load);
            // If cleanup fails, do not let the save error path unmap memory that CUDA
            // still considers registered.
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
                // An elected importer may execute on a different GPU from the backing.
                // The transfer stream needs access from the current context's device.
                let mut transfer_device = 0;
                unsafe { crate::driver::cuCtxGetDevice(&mut transfer_device) }?;
                let mut base = 0u64;
                unsafe { crate::driver::cuMemAddressReserve(&mut base, total, 0, 0, 0) }?;
                reserved = Some(base);
                let mut offset = 0usize;
                // A separate mapping lets us copy the full backing even when
                // application mappings are absent, partial, or lack the required
                // access.
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
                        location: cudarc::driver::sys::CUmemLocation {
                            type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                            id: transfer_device,
                        },
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
            // Cleanup attempts every step even if an earlier one failed, while
            // preserving the original operation error. A cleanup failure still
            // terminates the process.
            let mut result = transfer;
            if let Some(stream) = stream {
                if !synchronized {
                    let drained = unsafe { crate::driver::cuStreamSynchronize(stream) };
                    if drained.is_err() {
                        // Because DMA may still access this memory, cleanup must not
                        // free it. Terminate the process without running Rust or CUDA
                        // cleanup when completion is unknown.
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

    /// Application mappings may be partial or lack host access, so the full allocation
    /// is copied through an alias with CPU access. All writers remain paused, and the
    /// application mappings' addresses and permissions stay unchanged.
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
            // All CUDA work completes before the lifecycle starts. The alias and arena
            // are separate CPU mappings, so no device copy is needed.
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
    use crate::memory::{ProcessState, checkpoint};
    use crate::runtime;
    use std::sync::atomic::Ordering;

    #[test]
    fn load_acknowledged_releases_arena_after_runtime_failure() {
        const CHILD: &str = "CUINTERPOSE_ARENA_CLEANUP_TEST";
        if std::env::var_os(CHILD).is_none() {
            let output = std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "memory::host_carrier::tests::load_acknowledged_releases_arena_after_runtime_failure",
                ])
                .env(CHILD, "1")
                .env("SNAPSHOT_CONTROL_DIR", "/snapshot-control")
                .output()
                .unwrap();
            assert!(output.status.success(), "{output:?}");
            return;
        }

        let size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) } as usize;
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
        assert_ne!(base, libc::MAP_FAILED);
        let mut state = ProcessState::new(unsafe { libc::getpid() } as u32);
        state.arena = Some(Arena {
            base: base as usize,
            size,
            offsets: BTreeMap::new(),
        });
        runtime::install_for_test(state);

        // Hold the inherited mutex: a fork child must reject ownership before
        // attempting to lock it or releasing its copy of the arena.
        let guard = runtime::get().unwrap();
        runtime::RUNTIME_FAILED.store(true, Ordering::Release);
        let child = unsafe { libc::fork() };
        assert!(child >= 0);
        if child == 0 {
            unsafe { libc::alarm(2) };
            checkpoint::load_acknowledged();
            let mut resident = 0;
            let mapped = unsafe { libc::mincore(base, size, &mut resident) } == 0;
            unsafe { libc::_exit(i32::from(!mapped)) };
        }
        let mut status = 0;
        assert_eq!(unsafe { libc::waitpid(child, &mut status, 0) }, child);
        assert_eq!(status, 0, "fork child must leave the arena and mutex alone");
        drop(guard);

        assert!(matches!(runtime::ready(), Err(Error::RuntimeFailed)));
        assert!(matches!(runtime::get(), Err(Error::RuntimeFailed)));
        checkpoint::load_acknowledged();
        let mut resident = 0;
        assert_eq!(unsafe { libc::mincore(base, size, &mut resident) }, -1);
        assert_eq!(
            std::io::Error::last_os_error().raw_os_error(),
            Some(libc::ENOMEM)
        );
        checkpoint::load_acknowledged();
        assert!(matches!(runtime::ready(), Err(Error::RuntimeFailed)));
        assert!(matches!(runtime::get(), Err(Error::RuntimeFailed)));
    }
}
