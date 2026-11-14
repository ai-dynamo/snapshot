// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! FABRIC handles stay real at runtime: an export returns the driver's handle and an
//! import goes to the driver, so the shim never sits on the application's import path.
//! The shim records every handle that a process exports or imports, so the handle joins
//! an importer to its creator in another process or Pod. Restore re-exports each saved
//! creator, and importers import the replacement that the coordinator passes on.

use super::checkpoint::Phase;
use super::multicast::MulticastObject;
use super::vmm::Allocation;
use super::{Memblock, ProcessState, VirtualAllocationHandle};
use crate::driver;
use crate::error::{Error, Result};
use crate::runtime;
use cudarc::driver::sys::CUresult::{CUDA_ERROR_NOT_READY, CUDA_ERROR_NOT_SUPPORTED};
use cudarc::driver::sys::{
    CUmemAllocationHandleType, CUmemAllocationProp, CUmemAllocationType, CUmemLocation,
    CUmemLocationType, CUmulticastObjectProp,
};
use cuinterpose_protocol::{AllocationReference, FabricHandle, FabricReplacement, Record};
use std::collections::BTreeMap;
use std::sync::MutexGuard;

pub(crate) const FABRIC: CUmemAllocationHandleType =
    CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_FABRIC;

/// The driver does not expose the kind of object that a fabric handle names. Current
/// drivers store it at this offset: 1 for memory and 3 for a multicast object. The layout
/// is private, so any other value refuses the import instead of guessing. A public query
/// would remove this dependency.
const KIND_OFFSET: usize = 44;
const KIND_MEMORY: u8 = 1;
const KIND_MULTICAST: u8 = 3;

fn is_multicast(handle: &FabricHandle) -> Result<bool> {
    match handle.0[KIND_OFFSET] {
        KIND_MEMORY => Ok(false),
        KIND_MULTICAST => Ok(true),
        _ => Err(CUDA_ERROR_NOT_SUPPORTED.into()),
    }
}

impl Memblock {
    /// The fabric handle that this process exported or imported, if any.
    pub(crate) fn fabric(&self) -> Option<FabricHandle> {
        match self {
            Self::Unicast(allocation) => allocation.fabric,
            Self::Multicast(object) => object.fabric,
        }
    }
}

/// A fabric handle does not name its creator, so an import is recorded with creator
/// namespace PID 0, which is never a process, and an ID derived from the handle.
pub(crate) fn importer_reference(handle: &FabricHandle) -> AllocationReference {
    AllocationReference {
        id: handle.digest(),
        creator_pid: 0,
    }
}

/// Export a tracked memblock as its real fabric handle and mark it shared. CUDA returns
/// the same handle for every export of one object, so later exports return the recorded
/// one.
pub(crate) fn export(memblock: &mut Memblock) -> Result<FabricHandle> {
    if let Some(handle) = memblock.fabric() {
        return Ok(handle);
    }
    let handle = driver::export_fabric(memblock.driver_handle()?)?;
    match memblock {
        Memblock::Unicast(allocation) => {
            if allocation.context == 0 {
                allocation.context = driver::context()?;
            }
            allocation.shared = true;
            allocation.fabric = Some(handle);
        }
        Memblock::Multicast(object) => {
            object.shared = true;
            object.fabric = Some(handle);
        }
    }
    Ok(handle)
}

/// Import a fabric handle. A handle that a restore replaced stands for its newest
/// replacement. A handle that this process already exported or imported returns another
/// virtual handle for its memblock. Any other handle is imported from the driver and
/// recorded under `importer_reference`.
pub(crate) fn import(
    mut state: MutexGuard<'static, ProcessState>,
    handle: FabricHandle,
) -> Result<(MutexGuard<'static, ProcessState>, u64)> {
    let handle = state.current_fabric_handle(handle);
    if let Some(virtual_handle) = state.import_known_fabric(handle, None)? {
        return Ok((state, virtual_handle));
    }
    let multicast = is_multicast(&handle)?;
    let context = driver::context()?;
    // An importer cannot observe the creator's properties. It records the device it
    // imports on, or device 0 without a current context, as a native import needs none.
    // The coordinator takes the location and the rest from the creator's records.
    let mut device = 0;
    if !multicast && context != 0 {
        unsafe { driver::cuCtxGetDevice(&mut device) }?;
    }
    // Imports can wait for the fabric manager, so the registry is released around them.
    let (mut state, imported) = runtime::call_unlocked(state, || driver::import_fabric(&handle))?;
    let imported = runtime::must_complete(VirtualAllocationHandle::from_driver(imported));
    if let Some(virtual_handle) = state.import_known_fabric(handle, Some(imported))? {
        return Ok((state, virtual_handle));
    }
    let reference = importer_reference(&handle);
    if multicast {
        let properties = CUmulticastObjectProp {
            numDevices: 0,
            size: 0,
            handleTypes: u64::from(FABRIC.0),
            flags: 0,
        };
        let mut object = MulticastObject::imported(reference, properties, imported, context);
        object.fabric = Some(handle);
        object.refcounts.handle_entries = 1;
        let virtual_handle =
            runtime::must_complete(state.mint_virtual_allocation_handle(reference.id));
        state
            .memblocks
            .insert(reference.id, Memblock::Multicast(object));
        return Ok((state, virtual_handle));
    }
    let properties = CUmemAllocationProp {
        type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
        requestedHandleTypes: FABRIC,
        location: CUmemLocation {
            type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
            id: device,
        },
        ..unsafe { std::mem::zeroed() }
    };
    let virtual_handle = runtime::must_complete(state.adopt_unicast(Allocation {
        reference,
        refcounts: Default::default(),
        driver: Some(imported),
        size: 0,
        properties,
        shared: true,
        peer_access_granted: false,
        // Imported backing already uses the host carrier.
        home_device: None,
        context,
        fabric: Some(handle),
    }));
    Ok((state, virtual_handle))
}

impl ProcessState {
    /// Mint another virtual handle for the memblock that `handle` already names. A mapping
    /// keeps a unicast memblock after the application or a restore releases its driver
    /// handle. Such a memblock takes `imported`, a new import of `handle`, and counts as
    /// unknown without one. A memblock that holds a driver handle releases `imported`.
    fn import_known_fabric(
        &mut self,
        handle: FabricHandle,
        imported: Option<u64>,
    ) -> Result<Option<u64>> {
        if self.phase != Phase::Active {
            return Err(CUDA_ERROR_NOT_READY.into());
        }
        let Some((&id, memblock)) = self
            .memblocks
            .iter_mut()
            .find(|(_, memblock)| memblock.fabric() == Some(handle))
        else {
            return Ok(None);
        };
        if let Memblock::Unicast(allocation) = memblock
            && allocation.driver.is_none()
        {
            if imported.is_none() {
                return Ok(None);
            }
            allocation.driver = imported;
        } else if let Some(imported) = imported {
            // Another thread imported the same handle while this one waited.
            unsafe { driver::cuMemRelease(imported) }?;
        }
        let virtual_handle = self.mint_virtual_allocation_handle(id)?;
        self.memblocks
            .get_mut(&id)
            .unwrap()
            .refcounts_mut()
            .handle_entries += 1;
        Ok(Some(virtual_handle))
    }

    /// Keep the replacements that a restore step received or returned. A handle that a
    /// later restore replaces again maps from every earlier handle to the newest one.
    pub(crate) fn record_fabric_replacements(&mut self, replacements: &[FabricReplacement]) {
        let replacements: BTreeMap<_, _> = replacements
            .iter()
            .map(|replacement| (replacement.old, replacement.new))
            .collect();
        for new in self.fabric_replacements.values_mut() {
            if let Some(newer) = replacements.get(new) {
                *new = *newer;
            }
        }
        self.fabric_replacements.extend(replacements);
    }

    /// The newest replacement of `handle`, or `handle` itself if no restore replaced it.
    fn current_fabric_handle(&self, handle: FabricHandle) -> FabricHandle {
        self.fabric_replacements
            .get(&handle)
            .copied()
            .unwrap_or(handle)
    }
}

/// Name the handle of every memblock that this process exported or imported, which is
/// how the coordinator joins importers to creators.
pub(crate) fn describe(state: &ProcessState, records: &mut Vec<Record>) {
    for memblock in state.memblocks.values() {
        let Some(handle) = memblock.fabric() else {
            continue;
        };
        let allocation = memblock.reference();
        records.push(if allocation.creator_pid == state.namespace_pid {
            Record::FabricExport { allocation, handle }
        } else {
            Record::FabricImport { allocation, handle }
        });
    }
}

/// The handle that a restored creator exported in place of `old`.
pub(crate) fn replacement(
    replacements: &[FabricReplacement],
    old: FabricHandle,
) -> Result<FabricHandle> {
    replacements
        .iter()
        .find(|replacement| replacement.old == old)
        .map(|replacement| replacement.new)
        .ok_or(Error::MissingFabricReplacement(old))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{LazyLock, Mutex};

    fn handle(byte: u8) -> FabricHandle {
        let mut bytes = [byte; 64];
        bytes[KIND_OFFSET] = KIND_MEMORY;
        FabricHandle(bytes)
    }

    fn allocation(reference: AllocationReference, fabric: Option<FabricHandle>) -> Allocation {
        Allocation {
            reference,
            refcounts: Default::default(),
            driver: Some(0x1000),
            size: 4096,
            properties: CUmemAllocationProp {
                type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
                requestedHandleTypes: FABRIC,
                ..unsafe { std::mem::zeroed() }
            },
            shared: fabric.is_some(),
            peer_access_granted: false,
            home_device: Some(0),
            context: 0,
            fabric,
        }
    }

    #[test]
    fn handle_kind_selects_memory_or_multicast_and_refuses_other_values() {
        let mut bytes = [0; 64];
        for (kind, expected) in [(1, Some(false)), (3, Some(true)), (0, None), (2, None)] {
            bytes[KIND_OFFSET] = kind;
            match (is_multicast(&FabricHandle(bytes)), expected) {
                (Ok(actual), Some(expected)) => assert_eq!(actual, expected, "{kind}"),
                (Err(Error::Cuda(CUDA_ERROR_NOT_SUPPORTED)), None) => {}
                (actual, _) => panic!("{kind}: {actual:?}"),
            }
        }
    }

    #[test]
    fn records_name_exported_and_imported_handles() {
        let mut state = ProcessState::new(41);
        let created = state.new_reference().unwrap();
        let unshared = state.new_reference().unwrap();
        let imported = importer_reference(&handle(2));
        for (reference, fabric) in [
            (created, Some(handle(1))),
            (unshared, None),
            (imported, Some(handle(2))),
        ] {
            state.adopt_unicast(allocation(reference, fabric)).unwrap();
        }
        let mut records = Vec::new();
        describe(&state, &mut records);
        records.sort();
        let mut expected = vec![
            Record::FabricExport {
                allocation: created,
                handle: handle(1),
            },
            Record::FabricImport {
                allocation: imported,
                handle: handle(2),
            },
        ];
        expected.sort();
        assert_eq!(records, expected);
    }

    #[test]
    fn export_returns_the_recorded_handle_without_the_driver() {
        let mut state = ProcessState::new(41);
        let reference = state.new_reference().unwrap();
        state
            .adopt_unicast(allocation(reference, Some(handle(1))))
            .unwrap();
        let memblock = state.memblocks.get_mut(&reference.id).unwrap();
        assert_eq!(export(memblock).unwrap(), handle(1));
    }

    #[test]
    fn known_handles_reuse_their_memblock_while_active() {
        let mut state = ProcessState::new(41);
        let reference = importer_reference(&handle(2));
        state
            .adopt_unicast(allocation(reference, Some(handle(2))))
            .unwrap();
        assert_eq!(state.import_known_fabric(handle(3), None).unwrap(), None);
        let again = state.import_known_fabric(handle(2), None).unwrap().unwrap();
        assert_eq!(
            state.resolve_virtual_handle(again).unwrap(),
            Some(reference.id)
        );
        assert_eq!(state.memblocks[&reference.id].refcounts().handle_entries, 2);
        state.phase = Phase::Checkpointing;
        assert!(matches!(
            state.import_known_fabric(handle(2), None),
            Err(Error::Cuda(CUDA_ERROR_NOT_READY))
        ));
    }

    #[test]
    fn known_handles_without_a_driver_handle_take_a_new_import() {
        // An application that imports, maps, and releases a handle leaves a mapped
        // memblock without a driver handle. Restore leaves the same state.
        let mut state = ProcessState::new(41);
        let reference = importer_reference(&handle(2));
        let mut mapped = allocation(reference, Some(handle(2)));
        mapped.driver = None;
        mapped.refcounts.mappings = 1;
        state
            .memblocks
            .insert(reference.id, Memblock::Unicast(mapped));
        assert_eq!(state.import_known_fabric(handle(2), None).unwrap(), None);
        let again = state
            .import_known_fabric(handle(2), Some(0x2000))
            .unwrap()
            .unwrap();
        assert_eq!(
            state.resolve_virtual_handle(again).unwrap(),
            Some(reference.id)
        );
        let memblock = &state.memblocks[&reference.id];
        assert_eq!(memblock.driver_handle().unwrap(), 0x2000);
        assert_eq!(memblock.refcounts().handle_entries, 1);
    }

    fn replaced(old: u8, new: u8) -> FabricReplacement {
        FabricReplacement {
            old: handle(old),
            new: handle(new),
        }
    }

    #[test]
    fn replaced_handles_stand_for_their_newest_replacement() {
        let mut state = ProcessState::new(41);
        // One restore replaces 1 with 2 and 5 with 6, and a later step passes 1 with 2
        // again. The next restore replaces 2 with 3.
        state.record_fabric_replacements(&[replaced(1, 2), replaced(5, 6)]);
        state.record_fabric_replacements(&[replaced(1, 2)]);
        state.record_fabric_replacements(&[replaced(2, 3)]);
        for (old, current) in [(1, 3), (2, 3), (3, 3), (5, 6), (6, 6), (7, 7)] {
            assert_eq!(
                state.current_fabric_handle(handle(old)),
                handle(current),
                "{old}"
            );
        }
    }

    #[test]
    fn imports_of_a_replaced_handle_return_the_memblock_of_its_replacement() {
        // `import` takes the guard of a static registry.
        static STATE: LazyLock<Mutex<ProcessState>> =
            LazyLock::new(|| Mutex::new(ProcessState::new(41)));
        let mut state = STATE.lock().unwrap();
        let reference = importer_reference(&handle(3));
        state
            .adopt_unicast(allocation(reference, Some(handle(3))))
            .unwrap();
        state.record_fabric_replacements(&[replaced(1, 2)]);
        state.record_fabric_replacements(&[replaced(2, 3)]);
        // Tests have no driver, so the import succeeds only through the known handle 3.
        let (state, imported) = import(state, handle(1)).unwrap();
        assert_eq!(
            state.resolve_virtual_handle(imported).unwrap(),
            Some(reference.id)
        );
    }

    #[test]
    fn replacement_requires_an_entry_for_the_old_handle() {
        let entry = FabricReplacement {
            old: handle(1),
            new: handle(2),
        };
        assert_eq!(replacement(&[entry], handle(1)).unwrap(), handle(2));
        assert!(matches!(
            replacement(&[entry], handle(2)),
            Err(Error::MissingFabricReplacement(old)) if old == handle(2)
        ));
    }
}
