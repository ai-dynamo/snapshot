// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! FABRIC handles stay real at runtime: an export returns the driver's handle and an
//! import goes to the driver, so the shim never sits on the application's import path.
//! The shim records every handle that a process exports or imports, so the handle joins
//! an importer to its creator in another process or Pod. Restore re-exports each saved
//! creator, and importers import the replacement that the coordinator passes on.
//!
//! A fabric handle does not say whether it names memory or a multicast object, so an
//! import starts without a kind. The first call that only one kind accepts classifies
//! it once the driver accepts the call, and BeginCheckpoint classifies the rest with the
//! kinds that their creators recorded.

use super::checkpoint::Phase;
use super::multicast::MulticastObject;
use super::vmm::Allocation;
use super::{Memblock, ProcessState, Refcounts, VirtualAllocationHandle};
use crate::driver;
use crate::error::{Error, Result};
use crate::runtime;
use cudarc::driver::sys::CUresult::CUDA_ERROR_NOT_READY;
use cudarc::driver::sys::{
    CUmemAllocationHandleType, CUmemAllocationProp, CUmemAllocationType, CUmemLocation,
    CUmulticastObjectProp,
};
use cuinterpose_protocol::{
    AllocationId, AllocationReference, FabricHandle, FabricKind, FabricReplacement, Record,
};
use std::collections::BTreeMap;
use std::sync::{Arc, Mutex, MutexGuard};

pub(crate) const FABRIC: CUmemAllocationHandleType =
    CUmemAllocationHandleType::CU_MEM_HANDLE_TYPE_FABRIC;

/// A FABRIC import whose kind the process has not learned. It keeps only what both kinds
/// need, since `ProcessState::classify` turns it into the bookkeeping of its kind. Its
/// mappings, like those of either kind, are in `ProcessState::mappings`.
#[derive(Clone)]
pub struct Unclassified {
    pub reference: AllocationReference,
    pub refcounts: Refcounts,
    /// A multicast object needs its driver handle while it is mapped, so the import keeps
    /// the handle until it has neither virtual handles nor mappings.
    pub driver: u64,
    pub context: usize,
    pub fabric: FabricHandle,
    /// The location that CUDA reports for the import, which memory keeps.
    pub location: CUmemLocation,
    /// Bind and unbind calls that may classify the import keep their order on the
    /// multicast object that it becomes.
    pub binding_lock: Arc<Mutex<()>>,
}

impl Memblock {
    /// The fabric handle that this process exported or imported, if any.
    pub(crate) fn fabric(&self) -> Option<FabricHandle> {
        match self {
            Self::Unicast(allocation) => allocation.fabric,
            Self::Multicast(object) => object.fabric,
            Self::Unclassified(import) => Some(import.fabric),
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
        // An import returned its handle above.
        Memblock::Unclassified(_) => {}
    }
    Ok(handle)
}

/// Import a fabric handle. A handle that a restore replaced stands for its newest
/// replacement. A handle that this process already exported or imported returns another
/// virtual handle for its memblock. Any other handle is imported from the driver and
/// recorded under `importer_reference`, without a kind.
pub(crate) fn import(
    mut state: MutexGuard<'static, ProcessState>,
    handle: FabricHandle,
) -> Result<(MutexGuard<'static, ProcessState>, u64)> {
    let handle = state.current_fabric_handle(handle);
    if let Some(virtual_handle) = state.import_known_fabric(handle, None)? {
        return Ok((state, virtual_handle));
    }
    let context = driver::context()?;
    // Imports can wait for the fabric manager, so the registry is released around them.
    let (mut state, imported) = runtime::call_unlocked(state, || driver::import_fabric(&handle))?;
    let imported = runtime::must_complete(VirtualAllocationHandle::from_driver(imported));
    if let Some(virtual_handle) = state.import_known_fabric(handle, Some(imported))? {
        return Ok((state, virtual_handle));
    }
    // An importer cannot observe the creator's size, so the coordinator takes it and the
    // location from the creator's records. Memory keeps the location that CUDA reports
    // for the imported handle, as a POSIX import does, since a V1 multicast binding of
    // the import names that device. CUDA answers the query alike for both kinds, so the
    // answer cannot tell them apart. The query, like the import, needs no current context.
    let mut reported = std::mem::MaybeUninit::<CUmemAllocationProp>::zeroed();
    if let Err(error) =
        unsafe { driver::cuMemGetAllocationPropertiesFromHandle(reported.as_mut_ptr(), imported) }
    {
        runtime::must_complete(unsafe { driver::cuMemRelease(imported) });
        return Err(error);
    }
    let reference = importer_reference(&handle);
    let virtual_handle = runtime::must_complete(state.mint_virtual_allocation_handle(reference.id));
    let import = Unclassified {
        reference,
        refcounts: Refcounts {
            handle_entries: 1,
            mappings: 0,
        },
        driver: imported,
        context,
        fabric: handle,
        location: unsafe { reported.assume_init() }.location,
        binding_lock: Arc::default(),
    };
    state
        .memblocks
        .insert(reference.id, Memblock::Unclassified(import));
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

    /// Give the FABRIC import `id` the bookkeeping of `kind` if its kind was unknown, and
    /// leave any other memblock as it is. Mappings do not depend on the kind, so they stay.
    pub(crate) fn classify(&mut self, id: AllocationId, kind: FabricKind) {
        let Some(memblock) = self.memblocks.get_mut(&id) else {
            return;
        };
        let Memblock::Unclassified(import) = memblock else {
            return;
        };
        let import = import.clone();
        *memblock = match kind {
            FabricKind::Memory => {
                let mut allocation = Allocation {
                    reference: import.reference,
                    refcounts: import.refcounts,
                    driver: Some(import.driver),
                    size: 0,
                    properties: CUmemAllocationProp {
                        type_: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED,
                        requestedHandleTypes: FABRIC,
                        location: import.location,
                        ..unsafe { std::mem::zeroed() }
                    },
                    shared: true,
                    peer_access_granted: false,
                    // Imported backing already uses the host carrier.
                    home_device: None,
                    context: import.context,
                    fabric: Some(import.fabric),
                };
                // The application may have granted access before the kind was known.
                allocation.peer_access_granted = self.mappings.values().any(|mapping| {
                    mapping.id == id && allocation.grants_peer_access(&mapping.access)
                });
                Memblock::Unicast(allocation)
            }
            FabricKind::Multicast => {
                let properties = CUmulticastObjectProp {
                    numDevices: 0,
                    size: 0,
                    handleTypes: u64::from(FABRIC.0),
                    flags: 0,
                };
                let mut object = MulticastObject::imported(
                    import.reference,
                    properties,
                    import.driver,
                    import.context,
                );
                object.refcounts = import.refcounts;
                object.fabric = Some(import.fabric);
                object.binding_lock = import.binding_lock;
                Memblock::Multicast(object)
            }
        };
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
/// how the coordinator joins importers to creators, and the kind where it is known. A
/// creator always knows it.
pub(crate) fn describe(state: &ProcessState, records: &mut Vec<Record>) {
    for memblock in state.memblocks.values() {
        let Some(handle) = memblock.fabric() else {
            continue;
        };
        let allocation = memblock.reference();
        let kind = match memblock {
            Memblock::Unicast(_) => Some(FabricKind::Memory),
            Memblock::Multicast(_) => Some(FabricKind::Multicast),
            Memblock::Unclassified(_) => None,
        };
        records.push(match kind {
            Some(kind) if allocation.creator_pid == state.namespace_pid => Record::FabricExport {
                allocation,
                handle,
                kind,
            },
            kind => Record::FabricImport {
                allocation,
                handle,
                kind,
            },
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
    use crate::handlers;
    use crate::memory::vmm::Mapping;
    use cudarc::driver::sys::CUresult::{CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_SUPPORTED};
    use cudarc::driver::sys::{CUmemAccess_flags, CUmemAccessDesc, CUmemLocationType};
    use cuinterpose_protocol::{
        BindingSource, BindingVersion, ImportKind, MAX_MESSAGE_BYTES, MemberRange, MemoryAccess,
        MemoryLocation, MulticastProperties, Reply,
    };
    use std::sync::LazyLock;

    fn handle(byte: u8) -> FabricHandle {
        FabricHandle([byte; 64])
    }

    /// An import of `handle(byte)` whose kind is unknown, with one virtual handle.
    fn unclassified(state: &mut ProcessState, byte: u8) -> AllocationReference {
        let reference = importer_reference(&handle(byte));
        state.mint_virtual_allocation_handle(reference.id).unwrap();
        let import = Unclassified {
            reference,
            refcounts: Refcounts {
                handle_entries: 1,
                mappings: 0,
            },
            driver: 0x1000 | u64::from(byte),
            context: 0x77,
            fabric: handle(byte),
            location: CUmemLocation {
                type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                id: 1,
            },
            binding_lock: Arc::default(),
        };
        state
            .memblocks
            .insert(reference.id, Memblock::Unclassified(import));
        reference
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
        let unknown = unclassified(&mut state, 3);
        let mut records = Vec::new();
        describe(&state, &mut records);
        records.sort();
        let mut expected = vec![
            Record::FabricExport {
                allocation: created,
                handle: handle(1),
                kind: FabricKind::Memory,
            },
            Record::FabricImport {
                allocation: imported,
                handle: handle(2),
                kind: Some(FabricKind::Memory),
            },
            Record::FabricImport {
                allocation: unknown,
                handle: handle(3),
                kind: None,
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

    fn read_write(device: i32) -> CUmemAccessDesc {
        CUmemAccessDesc {
            location: CUmemLocation {
                type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                id: device,
            },
            flags: CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE,
        }
    }

    fn sorted(mut records: Vec<Record>) -> Vec<Record> {
        records.sort();
        records
    }

    #[test]
    fn classification_keeps_earlier_mappings_and_takes_the_kind() {
        let mut state = ProcessState::new(41);
        let memory = unclassified(&mut state, 2);
        let object = unclassified(&mut state, 1);
        // cuMemMap and cuMemSetAccess record the same mapping for either kind.
        for (reference, address) in [(memory, 0x10000), (object, 0x20000)] {
            state.mappings.insert(
                address,
                Mapping {
                    id: reference.id,
                    handle: VirtualAllocationHandle::from_raw(VirtualAllocationHandle::TAG | 1)
                        .unwrap(),
                    address,
                    size: 4096,
                    offset: 0,
                    access: vec![read_write(0)],
                    flags: 0,
                },
            );
            state
                .memblocks
                .get_mut(&reference.id)
                .unwrap()
                .refcounts_mut()
                .mappings += 1;
        }
        let unknown = |allocation, byte| Record::FabricImport {
            allocation,
            handle: handle(byte),
            kind: None,
        };
        // An import of unknown kind reports only that.
        assert_eq!(
            sorted(state.inspect().unwrap()),
            sorted(vec![unknown(memory, 2), unknown(object, 1)])
        );
        let Memblock::Unclassified(import) = &state.memblocks[&object.id] else {
            panic!("classified before any call");
        };
        let binding_lock = Arc::clone(&import.binding_lock);
        state.classify(memory.id, FabricKind::Memory);
        state.classify(object.id, FabricKind::Multicast);
        // A kind, once known, does not change.
        state.classify(memory.id, FabricKind::Multicast);
        let access = vec![MemoryAccess {
            location: MemoryLocation {
                location_type: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE as u32,
                id: 0,
            },
            flags: CUmemAccess_flags::CU_MEM_ACCESS_FLAGS_PROT_READWRITE as u32,
        }];
        let expected = vec![
            // Access to GPU 0 counts as peer access, since an import has no home GPU.
            Record::Allocation {
                allocation: memory,
                shared: true,
                peer_access_granted: true,
                size: 0,
                allocation_type: CUmemAllocationType::CU_MEM_ALLOCATION_TYPE_PINNED as u32,
                handle_types: FABRIC.0,
                location: MemoryLocation {
                    location_type: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE as u32,
                    id: 1,
                },
                virtual_allocation_handle_count: 1,
            },
            Record::Mapping {
                allocation: memory,
                address: 0x10000,
                size: 4096,
                offset: 0,
                access: access.clone(),
            },
            Record::Multicast {
                allocation: object,
                properties: MulticastProperties {
                    devices: 0,
                    size: 0,
                    handle_types: u64::from(FABRIC.0),
                    flags: 0,
                },
                virtual_multicast_handle_count: 1,
            },
            Record::MulticastMapping {
                allocation: object,
                address: 0x20000,
                size: 4096,
                offset: 0,
                flags: 0,
                access,
            },
            Record::FabricImport {
                allocation: memory,
                handle: handle(2),
                kind: Some(FabricKind::Memory),
            },
            Record::FabricImport {
                allocation: object,
                handle: handle(1),
                kind: Some(FabricKind::Multicast),
            },
        ];
        assert_eq!(sorted(state.inspect().unwrap()), sorted(expected));
        // Bind and unbind calls that waited for the import keep their order.
        let object = state.memblocks[&object.id].multicast().unwrap();
        assert!(Arc::ptr_eq(&object.binding_lock, &binding_lock));
    }

    #[test]
    fn begin_checkpoint_takes_the_creators_kinds_and_refuses_unknown_imports() {
        let mut state = ProcessState::new(41);
        let memory = unclassified(&mut state, 2);
        let object = unclassified(&mut state, 1);
        let named = |allocation, kind| [ImportKind { allocation, kind }];
        let error = state
            .begin_checkpoint(&named(memory, FabricKind::Memory))
            .unwrap_err();
        assert!(
            matches!(error, Error::UnknownFabricKind(unknown) if unknown == handle(1)),
            "{error}"
        );
        assert_eq!(state.phase, Phase::Active);
        // The import that the coordinator named keeps its kind.
        assert!(state.memblocks[&memory.id].unicast().is_some());
        let Ok(Reply::Inspection { records }) = state
            .begin_checkpoint(&named(object, FabricKind::Multicast))
            .unwrap()
            .result
        else {
            panic!("no records");
        };
        assert_eq!(state.phase, Phase::Checkpointing);
        for (allocation, byte, kind) in [
            (memory, 2, FabricKind::Memory),
            (object, 1, FabricKind::Multicast),
        ] {
            let record = Record::FabricImport {
                allocation,
                handle: handle(byte),
                kind: Some(kind),
            };
            assert!(records.contains(&record), "{records:?}");
        }
    }

    #[test]
    fn begin_checkpoint_stays_active_while_its_reply_exceeds_the_size_limit() {
        let mut state = ProcessState::new(41);
        let memory = unclassified(&mut state, 2);
        // An import of unknown kind reports only its handle, so its mappings first count
        // when BeginCheckpoint classifies it.
        let virtual_handle =
            VirtualAllocationHandle::from_raw(VirtualAllocationHandle::TAG | 1).unwrap();
        for index in 0..(MAX_MESSAGE_BYTES / 64) as u64 {
            let address = 0x10000 + index * 4096;
            state.mappings.insert(
                address,
                Mapping {
                    id: memory.id,
                    handle: virtual_handle,
                    address,
                    size: 4096,
                    offset: 0,
                    access: Vec::new(),
                    flags: 0,
                },
            );
        }
        assert_eq!(state.inspect().unwrap().len(), 1);
        let imports = [ImportKind {
            allocation: memory,
            kind: FabricKind::Memory,
        }];
        // A failure names only the error, since the response holds every mapping.
        let refused = state.begin_checkpoint(&imports);
        assert!(
            matches!(refused, Err(Error::Reply(_))),
            "{:?}",
            refused.err()
        );
        assert_eq!(state.phase, Phase::Active);
        // The import keeps its kind, and the process enters checkpoint mode once the
        // application unmaps enough that the reply fits.
        assert!(state.memblocks[&memory.id].unicast().is_some());
        state.mappings.retain(|address, _| *address == 0x10000);
        state.begin_checkpoint(&[]).unwrap();
        assert_eq!(state.phase, Phase::Checkpointing);
    }

    /// A driver that knows what each fabric handle names: an import of a handle whose
    /// bytes are odd is a multicast object. Like CUDA, it refuses a multicast call on
    /// memory and a multicast object as a member.
    mod fake {
        use cudarc::driver::sys::CUresult::{
            CUDA_ERROR_INVALID_VALUE, CUDA_ERROR_NOT_SUPPORTED, CUDA_SUCCESS,
        };
        use cudarc::driver::sys::*;
        use std::ffi::{CStr, c_void};
        use std::sync::Barrier;
        use std::sync::atomic::{AtomicBool, Ordering};

        /// Set to make the next map wait at `MAP` once it is in the driver, and again
        /// before it returns.
        pub(super) static BLOCK_MAP: AtomicBool = AtomicBool::new(false);
        pub(super) static MAP: Barrier = Barrier::new(2);

        fn multicast(handle: u64) -> bool {
            handle & 1 == 1
        }

        fn accepted(accepted: bool) -> CUresult {
            if accepted {
                CUDA_SUCCESS
            } else {
                CUDA_ERROR_NOT_SUPPORTED
            }
        }

        unsafe extern "C" fn cuCtxGetCurrent(context: *mut *mut c_void) -> CUresult {
            unsafe { context.write(0x77 as *mut c_void) };
            CUDA_SUCCESS
        }

        unsafe extern "C" fn cuCtxGetDevice(device: *mut CUdevice) -> CUresult {
            unsafe { device.write(0) };
            CUDA_SUCCESS
        }

        unsafe extern "C" fn cuMemImportFromShareableHandle(
            handle: *mut u64,
            shareable: *mut c_void,
            _: CUmemAllocationHandleType,
        ) -> CUresult {
            unsafe { handle.write(0x1000 | u64::from(shareable.cast::<u8>().read())) };
            CUDA_SUCCESS
        }

        /// CUDA reports the same properties for both kinds.
        unsafe extern "C" fn cuMemGetAllocationPropertiesFromHandle(
            properties: *mut CUmemAllocationProp,
            _: u64,
        ) -> CUresult {
            unsafe {
                (*properties).location = CUmemLocation {
                    type_: CUmemLocationType::CU_MEM_LOCATION_TYPE_DEVICE,
                    id: 1,
                }
            };
            CUDA_SUCCESS
        }

        /// Like CUDA, it refuses any flags, which are reserved.
        unsafe extern "C" fn cuMemMap(
            _: CUdeviceptr,
            _: usize,
            _: usize,
            _: u64,
            flags: u64,
        ) -> CUresult {
            if BLOCK_MAP.swap(false, Ordering::SeqCst) {
                MAP.wait();
                MAP.wait();
            }
            if flags == 0 {
                CUDA_SUCCESS
            } else {
                CUDA_ERROR_INVALID_VALUE
            }
        }

        unsafe extern "C" fn cuMemUnmap(_: CUdeviceptr, _: usize) -> CUresult {
            CUDA_SUCCESS
        }

        unsafe extern "C" fn cuMemRelease(_: u64) -> CUresult {
            CUDA_SUCCESS
        }

        unsafe extern "C" fn cuMemSetAccess(
            _: CUdeviceptr,
            _: usize,
            _: *const CUmemAccessDesc,
            _: usize,
        ) -> CUresult {
            CUDA_SUCCESS
        }

        unsafe extern "C" fn cuMulticastAddDevice(group: u64, _: CUdevice) -> CUresult {
            accepted(multicast(group))
        }

        unsafe extern "C" fn cuMulticastBindMem(
            group: u64,
            _: usize,
            member: u64,
            _: usize,
            _: usize,
            _: u64,
        ) -> CUresult {
            accepted(multicast(group) && !multicast(member))
        }

        unsafe extern "C" fn cuMulticastBindMem_v2(
            group: u64,
            _: CUdevice,
            _: usize,
            member: u64,
            _: usize,
            _: usize,
            _: u64,
        ) -> CUresult {
            accepted(multicast(group) && !multicast(member))
        }

        unsafe extern "C" fn cuMulticastBindAddr(
            group: u64,
            _: usize,
            _: CUdeviceptr,
            _: usize,
            _: u64,
        ) -> CUresult {
            accepted(multicast(group))
        }

        unsafe extern "C" fn cuMulticastBindAddr_v2(
            group: u64,
            _: CUdevice,
            _: usize,
            _: CUdeviceptr,
            _: usize,
            _: u64,
        ) -> CUresult {
            accepted(multicast(group))
        }

        unsafe extern "C" fn cuMulticastUnbind(
            group: u64,
            _: CUdevice,
            _: usize,
            _: usize,
        ) -> CUresult {
            accepted(multicast(group))
        }

        pub(super) fn lookup(name: &CStr) -> *mut c_void {
            macro_rules! table {
                ($($function:ident),*) => {
                    match name.to_bytes() {
                        $(name if name == stringify!($function).as_bytes() => {
                            $function as *mut c_void
                        })*
                        _ => std::ptr::null_mut(),
                    }
                };
            }
            table!(
                cuCtxGetCurrent,
                cuCtxGetDevice,
                cuMemImportFromShareableHandle,
                cuMemGetAllocationPropertiesFromHandle,
                cuMemMap,
                cuMemUnmap,
                cuMemRelease,
                cuMemSetAccess,
                cuMulticastAddDevice,
                cuMulticastBindMem,
                cuMulticastBindMem_v2,
                cuMulticastBindAddr,
                cuMulticastBindAddr_v2,
                cuMulticastUnbind
            )
        }
    }

    /// Run the test `name` again in a child process with a runtime and the fake driver,
    /// which are global to a process.
    fn with_driver(name: &str, scenario: impl FnOnce()) {
        const CHILD: &str = "CUINTERPOSE_FABRIC_DRIVER_TEST";
        if std::env::var_os(CHILD).is_none() {
            let output = std::process::Command::new(std::env::current_exe().unwrap())
                .args(["--exact", &format!("memory::fabric::tests::{name}")])
                .env(CHILD, "1")
                .output()
                .unwrap();
            // A filter that matches no test also succeeds.
            let stdout = String::from_utf8_lossy(&output.stdout);
            assert!(
                output.status.success() && stdout.contains("1 passed"),
                "{stdout}{}",
                String::from_utf8_lossy(&output.stderr)
            );
            return;
        }
        runtime::install_for_test(ProcessState::new(std::process::id()));
        driver::install_for_test(fake::lookup);
        scenario();
    }

    const SIZE: usize = 4096;

    /// Import `handle(byte)` as the application does.
    fn import_handle(byte: u8) -> u64 {
        let mut imported = 0;
        let mut bytes = handle(byte).0;
        handlers::cuMemImportFromShareableHandle(&mut imported, bytes.as_mut_ptr().cast(), FABRIC)
            .unwrap();
        imported
    }

    fn records() -> Vec<Record> {
        runtime::get().unwrap().inspect().unwrap()
    }

    /// The kind that the import of `handle(byte)` reports.
    fn kind(byte: u8) -> Option<FabricKind> {
        records()
            .into_iter()
            .find_map(|record| match record {
                Record::FabricImport {
                    handle: h, kind, ..
                } if h == handle(byte) => Some(kind),
                _ => None,
            })
            .expect("an import record")
    }

    #[test]
    fn each_call_that_only_one_kind_accepts_classifies_an_import() {
        with_driver(
            "each_call_that_only_one_kind_accepts_classifies_an_import",
            || {
                let added = import_handle(1);
                assert_eq!(kind(1), None);
                handlers::cuMulticastAddDevice(added, 0).unwrap();
                assert_eq!(kind(1), Some(FabricKind::Multicast));
                let unbound = import_handle(3);
                handlers::cuMulticastUnbind(unbound, 0, 0, SIZE).unwrap();
                // No tracked mapping covers this address.
                let bound = import_handle(5);
                handlers::cuMulticastBindAddr(bound, 0, 0x7000_0000, SIZE, 0).unwrap();
                let bound_v2 = import_handle(7);
                handlers::cuMulticastBindAddr_v2(bound_v2, 0, 0, 0x7000_0000, SIZE, 0).unwrap();
                // A memory binding classifies both the object and its member.
                let (object, member) = (import_handle(9), import_handle(2));
                handlers::cuMulticastBindMem(object, 0, member, 0, SIZE, 0).unwrap();
                let (object_v2, member_v2) = (import_handle(11), import_handle(4));
                handlers::cuMulticastBindMem_v2(object_v2, 0, 0, member_v2, 0, SIZE, 0).unwrap();
                // An address binding classifies the import mapped there as memory.
                let (by_address, mapped) = (import_handle(13), import_handle(6));
                handlers::cuMemMap(0x10_0000, SIZE, 0, mapped, 0).unwrap();
                handlers::cuMulticastBindAddr(by_address, 0, 0x10_0000, SIZE, 0).unwrap();
                for byte in [3, 5, 7, 9, 11, 13] {
                    assert_eq!(kind(byte), Some(FabricKind::Multicast), "{byte}");
                }
                for byte in [2, 4, 6] {
                    assert_eq!(kind(byte), Some(FabricKind::Memory), "{byte}");
                }
                // A V1 binding takes the device that CUDA reported for the import.
                let binding = Record::MulticastBinding {
                    allocation: importer_reference(&handle(9)),
                    source: BindingSource::Memory(MemberRange {
                        allocation: importer_reference(&handle(2)),
                        offset: 0,
                    }),
                    size: SIZE as u64,
                    offset: 0,
                    flags: 0,
                    version: BindingVersion::V1,
                    device: 1,
                };
                let records = records();
                assert!(records.contains(&binding), "{records:?}");
            },
        );
    }

    #[test]
    fn a_call_that_the_driver_refuses_leaves_the_import_unknown() {
        with_driver(
            "a_call_that_the_driver_refuses_leaves_the_import_unknown",
            || {
                let memory = import_handle(2);
                let refused = |result: Result<()>| {
                    assert!(
                        matches!(result, Err(Error::Cuda(CUDA_ERROR_NOT_SUPPORTED))),
                        "{result:?}"
                    );
                };
                refused(handlers::cuMulticastAddDevice(memory, 0));
                refused(handlers::cuMulticastUnbind(memory, 0, 0, SIZE));
                let other = import_handle(4);
                refused(handlers::cuMulticastBindMem(memory, 0, other, 0, SIZE, 0));
                // A multicast object cannot be a member.
                let (object, member) = (import_handle(1), import_handle(3));
                refused(handlers::cuMulticastBindMem(object, 0, member, 0, SIZE, 0));
                let unknown = [2, 4, 1, 3].map(|byte| Record::FabricImport {
                    allocation: importer_reference(&handle(byte)),
                    handle: handle(byte),
                    kind: None,
                });
                assert_eq!(sorted(records()), sorted(unknown.to_vec()));
            },
        );
    }

    #[test]
    fn mappings_from_before_the_kind_was_known_become_records_of_that_kind() {
        with_driver(
            "mappings_from_before_the_kind_was_known_become_records_of_that_kind",
            || {
                let access = [read_write(0)];
                let object = import_handle(1);
                handlers::cuMemMap(0x20_0000, SIZE, 0, object, 0).unwrap();
                handlers::cuMemSetAccess(0x20_0000, SIZE, access.as_ptr(), 1).unwrap();
                let memory = import_handle(2);
                handlers::cuMemMap(0x30_0000, SIZE, 0, memory, 0).unwrap();
                handlers::cuMemSetAccess(0x30_0000, SIZE, access.as_ptr(), 1).unwrap();
                // The mapping keeps the import after the application releases its handle.
                handlers::cuMemRelease(memory).unwrap();
                assert_eq!(records().len(), 2);
                handlers::cuMulticastAddDevice(object, 0).unwrap();
                let group = import_handle(3);
                handlers::cuMulticastBindAddr(group, 0, 0x30_0000, SIZE, 0).unwrap();
                let [object, memory] = [1, 2].map(|byte| importer_reference(&handle(byte)));
                let classified = records();
                let mapped = |record: &Record| match record {
                    Record::Mapping {
                        allocation,
                        address,
                        ..
                    } => Some((*allocation, *address)),
                    Record::MulticastMapping {
                        allocation,
                        address,
                        ..
                    } => Some((*allocation, *address)),
                    _ => None,
                };
                let mut mappings: Vec<_> = classified.iter().filter_map(mapped).collect();
                let mut expected = [(object, 0x20_0000), (memory, 0x30_0000)];
                mappings.sort();
                expected.sort();
                assert_eq!(mappings, expected);
                assert!(
                    classified.iter().any(|record| matches!(
                        record,
                        Record::MulticastMapping { allocation, .. } if *allocation == object
                    )),
                    "{classified:?}"
                );
                assert!(
                    classified.iter().any(|record| matches!(
                        record,
                        Record::Allocation {
                            allocation,
                            peer_access_granted: true,
                            virtual_allocation_handle_count: 0,
                            ..
                        } if *allocation == memory
                    )),
                    "{classified:?}"
                );
                // An unmapped import of unknown kind without handles is gone.
                let dropped = import_handle(5);
                handlers::cuMemMap(0x40_0000, SIZE, 0, dropped, 0).unwrap();
                handlers::cuMemRelease(dropped).unwrap();
                handlers::cuMemUnmap(0x40_0000, SIZE).unwrap();
                let gone = records()
                    .iter()
                    .all(|record| !matches!(record, Record::FabricImport { handle: h, .. } if *h == handle(5)));
                assert!(gone);
            },
        );
    }

    #[test]
    fn a_map_that_waits_in_the_driver_keeps_its_memblock_until_its_own_release() {
        with_driver(
            "a_map_that_waits_in_the_driver_keeps_its_memblock_until_its_own_release",
            || {
                // The application releases the last handle while the map waits in the
                // driver without the registry.
                let map_while_released = |address, imported, flags| {
                    fake::BLOCK_MAP.store(true, std::sync::atomic::Ordering::SeqCst);
                    let map = std::thread::spawn(move || {
                        handlers::cuMemMap(address, SIZE, 0, imported, flags)
                    });
                    fake::MAP.wait();
                    handlers::cuMemRelease(imported).unwrap();
                    fake::MAP.wait();
                    map.join().unwrap()
                };
                let held = |byte| {
                    runtime::get()
                        .unwrap()
                        .memblocks
                        .contains_key(&importer_reference(&handle(byte)).id)
                };
                // An import of unknown kind, and one that a call classified as a
                // multicast object.
                let object = import_handle(1);
                handlers::cuMulticastAddDevice(object, 0).unwrap();
                for (byte, imported) in [(2, import_handle(2)), (1, object)] {
                    let address = 0x10_0000 * u64::from(byte);
                    map_while_released(address, imported, 0).unwrap();
                    assert!(held(byte), "{byte}");
                    handlers::cuMemUnmap(address, SIZE).unwrap();
                    assert!(!held(byte), "{byte}");
                }
                // A map that the driver refuses holds nothing once it returns.
                let refused = map_while_released(0x40_0000, import_handle(4), 1);
                assert!(
                    matches!(refused, Err(Error::Cuda(CUDA_ERROR_INVALID_VALUE))),
                    "{refused:?}"
                );
                assert!(!held(4));
            },
        );
    }
}
