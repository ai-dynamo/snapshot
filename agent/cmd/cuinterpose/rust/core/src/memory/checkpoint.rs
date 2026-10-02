// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Local checkpoint validation, mutation, inspection, and completion.

use super::vmm::{Allocation, Mapping};
use super::vmm::access_metadata;
use super::{Memblock, ProcessState, sharing};
use crate::error::{Error, Result};
use crate::runtime;
use cudarc::driver::sys::CUresult::*;
use cuinterpose_protocol::Operation;
use cuinterpose_protocol::Reply;
use runtime::export_cache;
use std::collections::BTreeMap;
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
            return Err(Error::OutOfOrder {
                expected,
                actual: self,
            });
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
            return Err(Error::from(CUDA_ERROR_NOT_READY));
        }
        let mut records = Vec::new();
        for allocation in self.memblocks.values().filter_map(Memblock::unicast) {
            let virtual_allocation_handle_count = self
                .virtual_allocation_handles
                .values()
                .filter(|entry| entry.id == allocation.reference.id)
                .map(|entry| entry.references)
                .sum();
            let record = Record::Allocation {
                allocation: allocation.reference,
                checkpoint_via_host_carrier: allocation
                    .checkpoint_via_host_carrier(self.namespace_pid),
                size: allocation.size as u64,
                allocation_type: allocation.properties.type_ as u32,
                handle_types: allocation.properties.requestedHandleTypes.0,
                location: cuinterpose_protocol::MemoryLocation {
                    location_type: allocation.properties.location.type_ as u32,
                    id: allocation.properties.location.id,
                },
                virtual_allocation_handle_count,
            };
            records.push(record);
        }
        for mapping in self.mappings.values() {
            let record = Record::Mapping {
                allocation: self
                    .memblocks
                    .get(&mapping.id)
                    .and_then(Memblock::unicast)
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
        use super::host_carrier::{AllocationContent, Arena};
        let next_phase = self.phase.next(operation)?;
        let mut bytes = 0u64;
        match operation {
            Operation::PrepareMulticast => {}
            Operation::SaveAllocations => {
                let ids: Vec<_> = self
                    .memblocks
                    .values()
                    .filter_map(Memblock::unicast)
                    .filter(|a| a.checkpoint_via_host_carrier(self.namespace_pid))
                    .map(|a| a.reference.id)
                    .collect();
                let mut allocations = Vec::new();
                for id in ids {
                    let allocation = self
                        .memblocks
                        .get_mut(&id)
                        .and_then(Memblock::unicast_mut)
                        .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                    if allocation.driver.is_none() {
                        let mapping = self
                            .mappings
                            .values()
                            .find(|m| m.id == id)
                            .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
                        let mut driver = 0;
                        unsafe {
                            crate::driver::cuMemRetainAllocationHandle(
                                &mut driver,
                                mapping.address as usize as *mut c_void,
                            )
                        }?;
                        allocation.driver = Some(driver);
                    }
                    bytes = bytes
                        .checked_add(allocation.size as u64)
                        .ok_or(CUDA_ERROR_OUT_OF_MEMORY)?;
                    allocations.push(AllocationContent::from(&*allocation));
                }
                self.arena = Arena::save(&allocations)?;
            }
            Operation::PrepareUnicast => {
                export_cache()?.clear()?;
                for allocation in self
                    .memblocks
                    .values_mut()
                    .filter_map(Memblock::unicast_mut)
                    .filter(|a| a.shared)
                {
                    // VMM handles and mappings are context-independent. Retaining
                    // a primary here can initialize and tear down a GPU context
                    // for every allocation in a contextless process.
                    for mapping in self
                        .mappings
                        .values_mut()
                        .filter(|m| m.id == allocation.reference.id)
                    {
                        unsafe { crate::driver::cuMemUnmap(mapping.address, mapping.size) }?;
                    }
                    if let Some(driver) = allocation.driver {
                        unsafe { crate::driver::cuMemRelease(driver) }?;
                        allocation.driver = None;
                    }
                }
            }
            Operation::LoadAllocations => {
                // A restored process may now run on a different GPU or driver.
                self.malloc_layouts.clear();
                let mut allocations: Vec<_> = self
                    .memblocks
                    .values()
                    .filter_map(Memblock::unicast)
                    .filter(|a| a.checkpoint_via_host_carrier(self.namespace_pid))
                    .map(AllocationContent::from)
                    .collect();
                bytes = allocations.iter().try_fold(0u64, |sum, a| {
                    sum.checked_add(a.size as u64)
                        .ok_or(CUDA_ERROR_OUT_OF_MEMORY)
                })?;
                if let Some(arena) = &self.arena {
                    arena.load(&mut allocations)?;
                } else if !allocations.is_empty() {
                    return Err(Error::from(CUDA_ERROR_INVALID_VALUE));
                }
                for allocation in allocations {
                    self.memblocks
                        .get_mut(&allocation.id)
                        .and_then(Memblock::unicast_mut)
                        .ok_or(CUDA_ERROR_INVALID_HANDLE)?
                        .driver = allocation.driver;
                }
                self.remap(Participants::Creators)?;
            }
            Operation::RestoreUnicast => {
                for allocation in self
                    .memblocks
                    .values_mut()
                    .filter_map(Memblock::unicast_mut)
                    .filter(|a| a.reference.creator_pid != self.namespace_pid && a.shared)
                {
                    let (raw, properties) =
                        sharing::request_export(allocation.reference).map_err(Error::PeerExport)?;
                    if properties.is_some() {
                        return Err(Error::from(CUDA_ERROR_INVALID_HANDLE));
                    }
                    allocation.driver = Some(crate::driver::import_posix(raw.as_fd())?);
                }
                self.remap(Participants::Importers)?;
            }
            _ => return Err(Error::from(CUDA_ERROR_NOT_SUPPORTED)),
        }
        self.phase = next_phase;
        Ok(bytes)
    }

    fn remap(&mut self, participants: Participants) -> Result<()> {
        for allocation in self
            .memblocks
            .values_mut()
            .filter_map(Memblock::unicast_mut)
        {
            let created_here = allocation.reference.creator_pid == self.namespace_pid;
            if allocation.shared && created_here == (participants == Participants::Creators) {
                restore_allocation(allocation, &self.mappings, participants)?;
            }
        }
        Ok(())
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Participants {
    Creators,
    Importers,
}

fn restore_allocation(
    allocation: &mut Allocation,
    mappings: &BTreeMap<u64, Mapping>,
    participants: Participants,
) -> Result<()> {
    let driver = allocation.driver.ok_or(CUDA_ERROR_INVALID_HANDLE)?;
    for mapping in mappings
        .values()
        .filter(|m| m.id == allocation.reference.id)
    {
        unsafe {
            crate::driver::cuMemMap(mapping.address, mapping.size, mapping.offset, driver, 0)
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
    if participants == Participants::Creators {
        let fd = crate::driver::export_posix(driver)?;
        export_cache()?.insert(allocation.reference.id, fd, None)?;
    }
    if allocation.refcounts.handle_entries == 0 {
        unsafe { crate::driver::cuMemRelease(driver) }?;
        allocation.driver = None;
    }
    Ok(())
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
        .map_err(|error| format!("{operation:?}: {error}"))?;
    let bytes = runtime::must_complete(state.lifecycle(operation));
    Ok(Reply::Completed { operation, bytes })
}

/// The carrier must remain captured until the successful LOAD reply is sent.
pub(crate) fn load_acknowledged() {
    if let Ok(mut state) = runtime::get()
        && let Some(arena) = state.arena.take()
    {
        runtime::must_complete(arena.release());
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn inspect_rejects_mapping_without_backing() {
        let mut state = ProcessState::new(41);
        let id = [1; 16];
        let handle = super::super::VirtualAllocationHandle::from_raw(
            super::super::VirtualAllocationHandle::TAG | 1,
        )
        .unwrap();
        state.mappings.insert(
            4096,
            Mapping {
                id,
                handle,
                address: 4096,
                size: 4096,
                offset: 0,
                access: Vec::new(),
                flags: 0,
            },
        );
        assert!(matches!(
            state.inspect(),
            Err(Error::Cuda(CUDA_ERROR_INVALID_HANDLE))
        ));
    }

    #[test]
    fn checkpoint_entry_requires_idle_calls_and_runs_once() {
        let mut state = ProcessState::new(41);
        state.unlocked_driver_calls = 1;
        assert!(matches!(
            state.begin_checkpoint(),
            Err(Error::Cuda(CUDA_ERROR_NOT_READY))
        ));
        assert_eq!(state.phase, Phase::Active);
        state.unlocked_driver_calls = 0;
        assert!(state.begin_checkpoint().unwrap().is_empty());
        assert_eq!(state.phase, Phase::Checkpointing);
        assert!(matches!(
            state.begin_checkpoint(),
            Err(Error::Cuda(CUDA_ERROR_NOT_READY))
        ));
        assert!(matches!(
            state.new_reference(),
            Err(Error::Cuda(CUDA_ERROR_NOT_READY))
        ));
    }

    #[test]
    fn restore_invalidates_device_properties() {
        let mut state = ProcessState::new(41);
        state.malloc_layouts.insert(0, (4096, 1));
        state.phase = Phase::UnicastPrepared;
        state.lifecycle(Operation::LoadAllocations).unwrap();
        assert!(state.malloc_layouts.is_empty());
    }
}
