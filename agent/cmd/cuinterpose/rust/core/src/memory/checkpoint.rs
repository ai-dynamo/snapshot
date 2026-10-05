// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Validate, inspect, and update the local checkpoint state.

use super::vmm::{Allocation, Mapping, access_metadata};
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
    /// Validate local phase order before changing state and select the state to publish
    /// on success. Barriers between processes belong to the coordinator.
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
                shared: allocation.shared,
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

    /// The application has completed all CUDA work and remains paused through restore.
    /// Holding the mutex across both the state change and inspection keeps later phases
    /// consistent with the records returned to the coordinator.
    pub fn begin_checkpoint(&mut self) -> Result<Vec<cuinterpose_protocol::Record>> {
        if self.phase != Phase::Active || self.unlocked_driver_calls != 0 {
            return Err(CUDA_ERROR_NOT_READY.into());
        }
        let records = self.inspect()?;
        self.phase = Phase::Checkpointing;
        Ok(records)
    }

    /// Check the complete local plan before changing any state. The coordinator
    /// selects holders from the frozen group. Each participant verifies that the
    /// plan names exactly its shared allocations with their original identities.
    fn select_checkpoint_owners(
        &mut self,
        owners: &[cuinterpose_protocol::AllocationOwner],
    ) -> Result<()> {
        let mut selected = BTreeMap::new();
        for owner in owners {
            let allocation = self
                .memblocks
                .get(&owner.allocation.id)
                .and_then(Memblock::unicast)
                .ok_or(CUDA_ERROR_INVALID_HANDLE)?;
            if !allocation.shared
                || allocation.reference != owner.allocation
                || owner.owner_pid == 0
                || selected
                    .insert(owner.allocation.id, owner.owner_pid)
                    .is_some()
            {
                return Err(CUDA_ERROR_INVALID_VALUE.into());
            }
        }
        if selected.len()
            != self
                .memblocks
                .values()
                .filter_map(Memblock::unicast)
                .filter(|a| a.shared)
                .count()
        {
            return Err(CUDA_ERROR_INVALID_VALUE.into());
        }
        self.checkpoint_owners = selected;
        Ok(())
    }

    /// Call after phase validation. A failure during state changes terminates the
    /// process.
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
                    .filter(|a| {
                        a.shared
                            && self.checkpoint_owners.get(&a.reference.id)
                                == Some(&self.namespace_pid)
                    })
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
                    // These context-independent VMM operations run directly to avoid
                    // creating and destroying a primary context for every allocation in
                    // a process without a context.
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
                    .filter(|a| {
                        a.shared
                            && self.checkpoint_owners.get(&a.reference.id)
                                == Some(&self.namespace_pid)
                    })
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
                self.remap(Participants::Owners)?;
            }
            Operation::RestoreUnicast => {
                for allocation in self
                    .memblocks
                    .values_mut()
                    .filter_map(Memblock::unicast_mut)
                    .filter(|a| {
                        a.shared
                            && self.checkpoint_owners.get(&a.reference.id)
                                != Some(&self.namespace_pid)
                    })
                {
                    let owner = self.checkpoint_owners[&allocation.reference.id];
                    let (raw, metadata) = sharing::request_export(allocation.reference, owner)
                        .map_err(Error::PeerExport)?;
                    if !matches!(metadata, sharing::ExportMetadata::Unicast { size } if size == allocation.size)
                    {
                        return Err(Error::from(CUDA_ERROR_INVALID_HANDLE));
                    }
                    allocation.driver = Some(crate::driver::import_posix(raw.as_fd())?);
                }
                self.remap(Participants::Importers)?;
            }
            Operation::RestoreMulticastCreators
            | Operation::RestoreMulticastImporters
            | Operation::RestoreMulticastDevices
            | Operation::RestoreMulticastBindings => {}
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
            let owned_here =
                self.checkpoint_owners.get(&allocation.reference.id) == Some(&self.namespace_pid);
            if allocation.shared && owned_here == (participants == Participants::Owners) {
                restore_allocation(allocation, &self.mappings, participants)?;
            }
        }
        Ok(())
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Participants {
    Owners,
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
    if participants == Participants::Owners {
        let fd = crate::driver::export_posix(driver)?;
        export_cache()?.insert(
            allocation.reference,
            fd,
            sharing::ExportMetadata::Unicast {
                size: allocation.size,
            },
        )?;
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
    if operation == Operation::SaveAllocations {
        return Err("SaveAllocations requires the checkpoint owner plan".into());
    }
    let mut state = runtime::get().map_err(|_| "cuinterpose state is unavailable")?;
    state
        .phase
        .next(operation)
        .map_err(|error| format!("{operation:?}: {error}"))?;
    let bytes = runtime::must_complete(state.lifecycle(operation));
    Ok(Reply::Completed { operation, bytes })
}

pub(crate) fn save_allocations(
    owners: &[cuinterpose_protocol::AllocationOwner],
) -> std::result::Result<Reply, String> {
    let mut state = runtime::get().map_err(|_| "cuinterpose state is unavailable")?;
    let operation = Operation::SaveAllocations;
    state
        .phase
        .next(operation)
        .map_err(|error| format!("{operation:?}: {error}"))?;
    state
        .select_checkpoint_owners(owners)
        .map_err(|error| format!("{operation:?}: invalid owner plan: {error}"))?;
    let bytes = runtime::must_complete(state.lifecycle(operation));
    Ok(Reply::Completed { operation, bytes })
}

/// Keep the carrier until the successful LOAD reply has been sent.
pub(crate) fn load_acknowledged() {
    runtime::release_host_arena();
}

#[cfg(test)]
mod tests {
    use super::*;
    use cudarc::driver::sys::{
        CUmemAllocationHandleType, CUmemAllocationProp, CUmemAllocationType, CUmemLocation,
        CUmemLocationType,
    };
    use cuinterpose_protocol::{AllocationOwner, AllocationReference};

    fn add_allocation(state: &mut ProcessState, id: [u8; 16], shared: bool) -> AllocationReference {
        let reference = AllocationReference {
            id,
            creator_pid: 17,
        };
        state
            .adopt_unicast(Allocation {
                reference,
                refcounts: Default::default(),
                driver: None,
                size: 8192,
                properties: CUmemAllocationProp {
                    type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
                    requestedHandleTypes:
                        CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR,
                    location: CUmemLocation {
                        type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                        id: 0,
                    },
                    ..unsafe { std::mem::zeroed() }
                },
                shared,
                context: 0,
            })
            .unwrap();
        reference
    }

    #[test]
    fn invalid_owner_plans_leave_the_existing_plan_unchanged() {
        let mut state = ProcessState::new(41);
        let shared = add_allocation(&mut state, [1; 16], true);
        let private = add_allocation(&mut state, [2; 16], false);
        let owner = AllocationOwner {
            allocation: shared,
            owner_pid: 41,
        };
        state.select_checkpoint_owners(&[owner]).unwrap();
        let original = state.checkpoint_owners.clone();
        for plan in [
            vec![],
            vec![owner, owner],
            vec![AllocationOwner {
                owner_pid: 0,
                ..owner
            }],
            vec![AllocationOwner {
                allocation: AllocationReference {
                    creator_pid: 99,
                    ..shared
                },
                ..owner
            }],
            vec![AllocationOwner {
                allocation: AllocationReference {
                    id: [3; 16],
                    ..shared
                },
                ..owner
            }],
            vec![
                owner,
                AllocationOwner {
                    allocation: private,
                    ..owner
                },
            ],
        ] {
            assert!(
                state.select_checkpoint_owners(&plan).is_err(),
                "accepted {plan:?}"
            );
            assert_eq!(state.checkpoint_owners, original);
        }
    }

    #[test]
    fn owner_selection_preserves_mapping_only_importer_identity_and_extent() {
        for (size, offset) in [(8192, 0), (4096, 4096)] {
            let mut state = ProcessState::new(41);
            let reference = add_allocation(&mut state, [1; 16], true);
            let handle = *state.virtual_allocation_handles.keys().next().unwrap();
            state.virtual_allocation_handles.clear();
            let allocation = state
                .memblocks
                .get_mut(&reference.id)
                .unwrap()
                .unicast_mut()
                .unwrap();
            allocation.refcounts.handle_entries = 0;
            allocation.refcounts.mappings = 1;
            state.mappings.insert(
                4096,
                Mapping {
                    id: reference.id,
                    handle,
                    address: 4096,
                    size,
                    offset,
                    access: Vec::new(),
                    flags: 0,
                },
            );
            let before = state.inspect().unwrap();
            state
                .select_checkpoint_owners(&[AllocationOwner {
                    allocation: reference,
                    owner_pid: 41,
                }])
                .unwrap();
            assert_eq!(state.inspect().unwrap(), before);
            assert_eq!(state.checkpoint_owners[&reference.id], 41);
            assert!(before.iter().any(|record| matches!(record,
                cuinterpose_protocol::Record::Allocation {
                    allocation, size: 8192, virtual_allocation_handle_count: 0, shared: true, ..
                } if *allocation == reference
            )));
        }
    }

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
