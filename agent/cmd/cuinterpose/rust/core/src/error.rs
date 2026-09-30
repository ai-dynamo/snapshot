// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Internal failures retain their cause until the CUDA or control boundary.

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
