// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Local checkpoint validation, mutation, inspection, and completion.

use super::vmm::access_metadata;
use super::{Memblock, ProcessState, sharing};
use crate::driver::Context;
use crate::driver::{CudaError, Result};
use crate::runtime;
use cudarc::driver::sys::CUresult::*;
use cuinterpose_protocol::Operation;
use cuinterpose_protocol::Reply;
use runtime::export_cache;
use std::ffi::c_void;
use std::os::fd::AsFd;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    Active,
    Checkpointing,
    MulticastPrepared,
    AllocationsSaved,
    UnicastPrepared,
    AllocationsLoaded,
    UnicastRestored,
    MulticastCreatorsRestored,
    MulticastImportersRestored,
    MulticastDevicesRestored,
}

impl Phase {
    /// Validate ordering before mutation and determine the state to publish on
    /// success. The coordinator, not this local state, owns global barriers.
    pub(crate) fn next(self, operation: Operation) -> Result<Self> {
        let (expected, next) = match operation {
            Operation::PrepareMulticast => (Self::Checkpointing, Self::MulticastPrepared),
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
        if !matches!(
            self.phase,
            Phase::Active | Phase::Checkpointing | Phase::UnicastPrepared
        ) || self.unlocked_driver_calls != 0
        {
            return Err(CudaError::from(CUDA_ERROR_NOT_READY));
        }
        let mut records = Vec::new();
        for allocation in self.memblocks.values().filter_map(Memblock::unicast) {
            let virtual_allocation_handle_count = self
                .virtual_allocation_handles
                .values()
                .filter(|id| **id == allocation.reference.id)
                .count() as u64;
            let record = Record::Allocation {
                allocation: allocation.reference,
                content: allocation.needs_content_checkpoint(self.namespace_pid),
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
                allocation: self.memblocks[&mapping.id]
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

    /// The application has drained CUDA work and stays parked through restore.
    /// Hold the mutex across the state change and inspection so later phases
    /// operate on exactly the records returned to the coordinator.
    pub fn begin_checkpoint(&mut self) -> Result<Vec<cuinterpose_protocol::Record>> {
        if self.phase != Phase::Active || self.unlocked_driver_calls != 0 {
            return Err(CUDA_ERROR_NOT_READY.into());
        }
        let records = self.inspect()?;
        self.phase = Phase::Checkpointing;
        Ok(records)
    }

    /// Called after phase validation; mutation failures terminate the process.
    pub fn lifecycle(&mut self, operation: Operation) -> Result<u64> {
        let next_phase = self.phase.next(operation)?;
        let bytes = 0u64;
        match operation {
            Operation::PrepareMulticast => {}

            Operation::PrepareUnicast => {
                export_cache()?.clear()?;
                for allocation in self
                    .memblocks
                    .values_mut()
                    .filter_map(Memblock::unicast_mut)
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
                            }
                            if let Some(driver) = allocation.driver {
                                unsafe { crate::driver::cuMemRelease(driver) }?;
                                allocation.driver = None;
                            }
                            Ok(())
                        },
                    )?;
                }
            }

            Operation::RestoreUnicast => {
                for allocation in self
                    .memblocks
                    .values_mut()
                    .filter_map(Memblock::unicast_mut)
                    .filter(|a| a.reference.creator_pid != self.namespace_pid && a.shared)
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
        Ok(bytes)
    }

    fn remap(&mut self, creator: bool) -> Result<()> {
        let namespace_pid = self.namespace_pid;
        for allocation in self
            .memblocks
            .values_mut()
            .filter_map(Memblock::unicast_mut)
            .filter(|a| a.shared && (a.reference.creator_pid == namespace_pid) == creator)
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
                    }
                    if creator && allocation.shared {
                        let fd = crate::driver::export_posix(
                            allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?,
                        )?;
                        export_cache()?.insert(allocation.reference.id, fd, None)?;
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
    })
}

pub(crate) fn begin() -> std::result::Result<Reply, String> {
    let mut state = runtime::get().map_err(|_| "cuinterpose state is unavailable")?;
    let records = state
        .begin_checkpoint()
        .map_err(|_| "application is not ready for checkpoint")?;
    Ok(Reply::Inspection { records })
}

pub(crate) fn execute(operation: Operation) -> std::result::Result<Reply, String> {
    let mut state = runtime::get().map_err(|_| "cuinterpose state is unavailable")?;
    state
        .phase
        .next(operation)
        .map_err(|_| "CUDA lifecycle operation out of order")?;
    let bytes = runtime::must_complete(state.lifecycle(operation));
    Ok(Reply::Completed { operation, bytes })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn checkpoint_entry_requires_idle_calls_and_runs_once() {
        let mut state = ProcessState::new(41);
        state.unlocked_driver_calls = 1;
        assert_eq!(state.begin_checkpoint(), Err(CUDA_ERROR_NOT_READY.into()));
        assert_eq!(state.phase, Phase::Active);
        state.unlocked_driver_calls = 0;
        assert!(state.begin_checkpoint().unwrap().is_empty());
        assert_eq!(state.phase, Phase::Checkpointing);
        assert_eq!(state.begin_checkpoint(), Err(CUDA_ERROR_NOT_READY.into()));
        assert_eq!(state.new_reference(), Err(CUDA_ERROR_NOT_READY.into()));
    }
}
