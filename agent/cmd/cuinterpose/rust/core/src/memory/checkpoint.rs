// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Local checkpoint validation, mutation, inspection, and completion.

use super::vmm::access_metadata;
use super::{ProcessState, Resource, sharing};
use crate::driver::Context;
use crate::driver::{CudaError, Result};
use crate::runtime;
use cudarc::driver::sys::CUresult::*;
use cuinterpose_protocol::Operation;
use cuinterpose_protocol::Reply;
use runtime::cache;
use std::ffi::c_void;
use std::os::fd::AsFd;
use std::sync::atomic::Ordering;

#[derive(Default)]
pub(crate) struct Transfer {
    pub bytes: u64,
    pub copy_us: u32,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    Active,
    MulticastPrepared,
    AllocationsSaved,
    UnicastPrepared,
    AllocationsLoaded,
    UnicastRestored,
    MulticastCreatorsRestored,
    MulticastImportersRestored,
    MulticastDevicesRestored,
    ReconstructingMulticast,
}

impl Phase {
    /// Validate ordering before mutation and determine the state to publish on
    /// success. The coordinator, not this local state, owns global barriers.
    pub(crate) fn next(self, operation: Operation) -> Result<Self> {
        let (expected, next) = match operation {
            Operation::PrepareMulticast => (Self::Active, Self::MulticastPrepared),
            Operation::SaveAllocations => (Self::MulticastPrepared, Self::AllocationsSaved),
            Operation::PrepareUnicast => (Self::AllocationsSaved, Self::UnicastPrepared),
            Operation::LoadAllocations => (Self::UnicastPrepared, Self::AllocationsLoaded),
            Operation::RestoreUnicast => (Self::AllocationsLoaded, Self::UnicastRestored),
            Operation::RestoreMulticastCreators => {
                (Self::UnicastRestored, Self::MulticastCreatorsRestored)
            }
            Operation::RestoreMulticastImporters => (
                Self::MulticastCreatorsRestored,
                Self::MulticastImportersRestored,
            ),
            Operation::RestoreMulticastDevices => (
                Self::MulticastImportersRestored,
                Self::MulticastDevicesRestored,
            ),
            Operation::RestoreMulticastBindings => (Self::MulticastDevicesRestored, Self::Active),
        };
        if self != expected {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        Ok(next)
    }
}

impl ProcessState {
    pub fn inspect(&self) -> Result<Vec<cuinterpose_protocol::Record>> {
        use cuinterpose_protocol::Record;
        if !matches!(self.phase, Phase::Active | Phase::UnicastPrepared) || self.inflight != 0 {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        let count = self
            .resources
            .values()
            .filter_map(Resource::unicast)
            .count()
            .checked_add(self.mappings.len())
            .ok_or(CUDA_ERROR_OUT_OF_MEMORY)?;
        let mut records = Vec::new();
        records
            .try_reserve_exact(count)
            .map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
        for allocation in self.resources.values().filter_map(Resource::unicast) {
            let virtual_allocation_handle_count = self
                .virtual_allocation_handles
                .values()
                .filter(|id| **id == allocation.reference.id)
                .count()
                .try_into()
                .map_err(|_| CUDA_ERROR_OUT_OF_MEMORY)?;
            let record = Record::Allocation {
                allocation: allocation.reference,
                content: allocation.owns_content(self.namespace_pid),
                size: allocation.size as u64,
                allocation_type: allocation.properties.type_ as u32,
                handle_types: allocation.properties.requestedHandleTypes.0,
                location: (
                    allocation.properties.location.type_ as u32,
                    allocation.properties.location.id,
                ),
                virtual_allocation_handle_count,
            };
            records.push(record);
        }
        for mapping in self.mappings.values() {
            let record = Record::Mapping {
                allocation: self.resources[&mapping.id]
                    .unicast()
                    .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                    .reference,
                address: mapping.address,
                size: mapping.size as u64,
                offset: mapping.offset as u64,
                access: access_metadata(&mapping.access),
            };
            records.push(record);
        }
        Ok(records)
    }

    pub fn validate_lifecycle(&self, operation: Operation) -> Result<()> {
        self.phase.next(operation)?;
        if self.inflight != 0 {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        if self.unsupported != 0 || !self.raw.is_empty() {
            return Err(CudaError::from(CUDA_ERROR_NOT_SUPPORTED));
        }
        if operation == Operation::PrepareMulticast {
            self.inspect()?;
        }
        Ok(())
    }

    /// Validation has completed without mutation. Failures here are fail-stop.
    pub fn lifecycle(&mut self, operation: Operation) -> Result<Transfer> {
        let next_phase = self.phase.next(operation)?;
        let bytes = 0u64;
        let copy_us = 0u32;
        match operation {
            Operation::PrepareMulticast => {}

            Operation::PrepareUnicast => {
                cache()?.clear()?;
                for allocation in self
                    .resources
                    .values_mut()
                    .filter_map(Resource::unicast_mut)
                    .filter(|a| a.shared)
                {
                    Context::run(
                        allocation.context,
                        allocation.properties.location.id,
                        || {
                            for mapping in self
                                .mappings
                                .values_mut()
                                .filter(|m| m.id == allocation.reference.id)
                            {
                                unsafe {
                                    crate::driver::cuMemUnmap(mapping.address, mapping.size)
                                }?;
                                mapping.checkpointed = true;
                            }
                            if let Some(driver) = allocation.driver {
                                unsafe { crate::driver::cuMemRelease(driver) }?;
                                allocation.driver = None;
                            }
                            allocation.checkpointed = true;
                            Ok(())
                        },
                    )?;
                }
            }

            Operation::RestoreUnicast => {
                for allocation in self
                    .resources
                    .values_mut()
                    .filter_map(Resource::unicast_mut)
                    .filter(|a| a.reference.creator_pid != self.namespace_pid && a.checkpointed)
                {
                    let (raw, properties) = sharing::request_export(allocation.reference)
                        .map_err(|_| CUDA_ERROR_INVALID_HANDLE)?;
                    if properties.is_some() {
                        return Err(CudaError::from(CUDA_ERROR_INVALID_HANDLE));
                    }
                    allocation.driver = Some(Context::run(
                        allocation.context,
                        allocation.properties.location.id,
                        || crate::driver::import_posix(raw.as_fd()),
                    )?);
                }
                self.remap(false)?;
            }
            _ => return Err(CudaError::from(CUDA_ERROR_NOT_SUPPORTED)),
        }
        self.phase = next_phase;
        Ok(Transfer { bytes, copy_us })
    }

    fn remap(&mut self, creator: bool) -> Result<()> {
        let namespace_pid = self.namespace_pid;
        for allocation in self
            .resources
            .values_mut()
            .filter_map(Resource::unicast_mut)
            .filter(|a| a.checkpointed && (a.reference.creator_pid == namespace_pid) == creator)
        {
            Context::run(
                allocation.context,
                allocation.properties.location.id,
                || {
                    for mapping in self
                        .mappings
                        .values_mut()
                        .filter(|m| m.id == allocation.reference.id && m.checkpointed)
                    {
                        unsafe {
                            crate::driver::cuMemMap(
                                mapping.address,
                                mapping.size,
                                mapping.offset,
                                allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                                0,
                            )
                        }?;
                        if !mapping.access.is_empty() {
                            unsafe {
                                crate::driver::cuMemSetAccess(
                                    mapping.address,
                                    mapping.size,
                                    mapping.access.as_ptr(),
                                    mapping.access.len(),
                                )
                            }?;
                        }
                        mapping.checkpointed = false;
                    }
                    if creator && allocation.shared {
                        let fd = crate::driver::export_posix(
                            allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                        )?;
                        cache()?.insert(allocation.reference.id, fd, None)?;
                    }
                    if !self
                        .virtual_allocation_handles
                        .values()
                        .any(|id| *id == allocation.reference.id)
                    {
                        unsafe {
                            crate::driver::cuMemRelease(
                                allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                            )
                        }?;
                        allocation.driver = None;
                    }
                    allocation.checkpointed = false;
                    allocation.content_saved = false;
                    Ok(())
                },
            )?;
        }
        Ok(())
    }
}

pub(crate) fn inspect() -> std::result::Result<Reply, String> {
    let state = runtime::get().map_err(|_| "cuinterpose state is unavailable")?;
    Ok(Reply::Inspection {
        records: state
            .inspect()
            .map_err(|_| "cannot inspect current CUDA state")?,
        live_raw_imports: state.live_raw_imports(),
        unsupported_creations: state.unsupported,
    })
}

pub(crate) fn execute(operation: Operation) -> std::result::Result<Reply, String> {
    let mut state = runtime::get().map_err(|_| "cuinterpose state is unavailable")?;
    state
        .validate_lifecycle(operation)
        .map_err(|_| "CUDA lifecycle operation refused without mutation")?;
    let result = state.lifecycle(operation);
    match result {
        Ok(transfer) => Ok(Reply::Completed {
            operation,
            bytes: transfer.bytes,
            copy_us: transfer.copy_us,
        }),
        Err(code) => {
            runtime::G_FAILED.store(true, Ordering::Release);
            Err(format!("CUDA lifecycle operation failed: {code}"))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lifecycle_transitions_require_each_local_milestone() {
        let transitions = [
            (
                Phase::Active,
                Operation::PrepareMulticast,
                Phase::MulticastPrepared,
            ),
            (
                Phase::MulticastPrepared,
                Operation::SaveAllocations,
                Phase::AllocationsSaved,
            ),
            (
                Phase::AllocationsSaved,
                Operation::PrepareUnicast,
                Phase::UnicastPrepared,
            ),
            (
                Phase::UnicastPrepared,
                Operation::LoadAllocations,
                Phase::AllocationsLoaded,
            ),
            (
                Phase::AllocationsLoaded,
                Operation::RestoreUnicast,
                Phase::UnicastRestored,
            ),
            (
                Phase::UnicastRestored,
                Operation::RestoreMulticastCreators,
                Phase::MulticastCreatorsRestored,
            ),
            (
                Phase::MulticastCreatorsRestored,
                Operation::RestoreMulticastImporters,
                Phase::MulticastImportersRestored,
            ),
            (
                Phase::MulticastImportersRestored,
                Operation::RestoreMulticastDevices,
                Phase::MulticastDevicesRestored,
            ),
            (
                Phase::MulticastDevicesRestored,
                Operation::RestoreMulticastBindings,
                Phase::Active,
            ),
        ];
        for (phase, operation, next) in transitions {
            assert_eq!(phase.next(operation), Ok(next));
            for (other, _, _) in transitions {
                if other != phase {
                    assert_eq!(
                        other.next(operation),
                        Err(CudaError::from(CUDA_ERROR_NOT_READY))
                    );
                }
            }
            assert_eq!(
                Phase::ReconstructingMulticast.next(operation),
                Err(CudaError::from(CUDA_ERROR_NOT_READY))
            );
        }
    }
}
