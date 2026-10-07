// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Preserve internal error causes until returning through CUDA or control APIs.

use crate::memory::checkpoint::Phase;
use cudarc::driver::sys::CUresult;
use std::io;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("CUDA error {0:?}")]
    Cuda(CUresult),
    #[error("{operation}: {source}")]
    Io {
        operation: &'static str,
        #[source]
        source: io::Error,
    },
    #[error("peer export: {0}")]
    PeerExport(#[source] cuinterpose_protocol::Error),
    #[error("lifecycle out of order: expected {expected:?}, actual {actual:?}")]
    OutOfOrder { expected: Phase, actual: Phase },
    #[error("{handles} foreign allocation handles and {mappings} foreign mappings are still held")]
    ForeignMemoryHeld { handles: usize, mappings: usize },
    #[error("runtime startup: {0}")]
    Startup(&'static str),
    #[error("cuinterpose runtime previously failed")]
    RuntimeFailed,
}

pub type Result<T> = std::result::Result<T, Error>;

impl Error {
    pub fn io(operation: &'static str, source: impl Into<io::Error>) -> Self {
        Self::Io {
            operation,
            source: source.into(),
        }
    }
}

impl From<CUresult> for Error {
    fn from(code: CUresult) -> Self {
        Self::Cuda(code)
    }
}
