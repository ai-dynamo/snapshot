// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Release malloc allocations with their contexts, although VMM backing can outlive a
//! context.

use super::{Memblock, ProcessState, VirtualAllocationHandle};
use crate::driver::{self};
use crate::error::Result;
use crate::runtime;
use cudarc::driver::sys::{CUcontext, CUdevice};
use cuinterpose_protocol::AllocationId;

struct ContextResources {
    context: usize,
    mallocs: Vec<(u64, VirtualAllocationHandle)>,
    memblocks: Vec<AllocationId>,
}

impl ContextResources {
    fn capture(state: &ProcessState, context: usize) -> Self {
        Self {
            context,
            mallocs: state
                .malloc_regions
                .iter()
                .filter(|(_, region)| context != 0 && region.context == context)
                .map(|(&address, region)| (address, region.virtual_allocation_handle))
                .collect(),
            memblocks: state
                .memblocks
                .iter()
                .filter(|(_, memblock)| {
                    context != 0
                        && match memblock {
                            Memblock::Unicast(allocation) => allocation.context == context,
                        }
                })
                .map(|(&id, _)| id)
                .collect(),
        }
    }

    fn release(self, state: &mut ProcessState) -> Result<()> {
        for (address, handle) in self.mallocs {
            if state
                .malloc_regions
                .get(&address)
                .is_some_and(|region| region.virtual_allocation_handle == handle)
            {
                // Use context-independent VMM cleanup because the ordinary free path
                // would synchronize a context that has already been destroyed.
                state.unmap_malloc(address)?;
            }
        }
        for id in self.memblocks {
            let Some(memblock) = state.memblocks.get_mut(&id) else {
                continue;
            };
            let context = match memblock {
                Memblock::Unicast(allocation) => &mut allocation.context,
            };
            if *context == self.context {
                // Direct VMM and multicast allocations survive context destruction, so
                // later checkpoint copies fall back to a primary context when the
                // recorded one is gone.
                *context = 0;
            }
        }
        Ok(())
    }
}

fn destroy(context: CUcontext, operation: impl FnOnce() -> Result<()>) -> Result<()> {
    let state = runtime::active()?;
    let resources = ContextResources::capture(&state, context as usize);
    let (mut state, ()) = runtime::call_unlocked(state, operation)?;
    runtime::must_complete(resources.release(&mut state));
    Ok(())
}

pub fn cuCtxDestroy(context: CUcontext) -> Result<()> {
    destroy(context, || unsafe { driver::cuCtxDestroy(context) })
}

pub fn cuCtxDestroy_v2(context: CUcontext) -> Result<()> {
    destroy(context, || unsafe { driver::cuCtxDestroy_v2(context) })
}

fn primary_active(device: CUdevice) -> Result<bool> {
    let mut flags = 0;
    let mut active = 0;
    unsafe { driver::cuDevicePrimaryCtxGetState(device, &mut flags, &mut active) }?;
    Ok(active != 0)
}

fn primary_context(device: CUdevice) -> Result<usize> {
    if !primary_active(device)? {
        return Ok(0);
    }
    // CUDA Runtime can retain the primary context internally, so driver state must be
    // queried rather than inferred from application calls. An active context already
    // has an owner, which lets us release our extra reference before the requested
    // release or reset without changing CUDA behavior.
    let mut context = std::ptr::null_mut();
    unsafe { driver::cuDevicePrimaryCtxRetain(&mut context, device) }?;
    runtime::must_complete(unsafe { driver::cuDevicePrimaryCtxRelease_v2(device) });
    Ok(context as usize)
}

fn primary_lifetime(
    device: CUdevice,
    reset: bool,
    operation: impl FnOnce() -> Result<()>,
) -> Result<()> {
    let state = runtime::active()?;
    let resources = ContextResources::capture(&state, primary_context(device)?);
    // The application must serialize context destruction, release, and reset with other
    // calls that use the context, including new retains. The unlocked call counter also
    // prevents checkpoint entry during these operations.
    let (mut state, destroyed) = runtime::call_unlocked(state, || {
        operation()?;
        Ok(reset || !runtime::must_complete(primary_active(device)))
    })?;
    if destroyed {
        runtime::must_complete(resources.release(&mut state));
    }
    Ok(())
}

pub fn cuDevicePrimaryCtxReset(device: CUdevice) -> Result<()> {
    primary_lifetime(device, true, || unsafe {
        driver::cuDevicePrimaryCtxReset(device)
    })
}

pub fn cuDevicePrimaryCtxReset_v2(device: CUdevice) -> Result<()> {
    primary_lifetime(device, true, || unsafe {
        driver::cuDevicePrimaryCtxReset_v2(device)
    })
}

pub fn cuDevicePrimaryCtxRelease(device: CUdevice) -> Result<()> {
    primary_lifetime(device, false, || unsafe {
        driver::cuDevicePrimaryCtxRelease(device)
    })
}

pub fn cuDevicePrimaryCtxRelease_v2(device: CUdevice) -> Result<()> {
    primary_lifetime(device, false, || unsafe {
        driver::cuDevicePrimaryCtxRelease_v2(device)
    })
}
