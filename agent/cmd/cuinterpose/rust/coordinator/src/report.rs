// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use serde::Serialize;
use std::io::{self, Write};
use std::time::Instant;

#[derive(Serialize)]
pub struct Transfer {
    pub allocation_count: usize,
    pub allocation_bytes: u64,
    pub gb_per_s: f64,
    pub copy_gb_per_s: f64,
}

#[derive(Serialize)]
#[serde(tag = "phase", rename_all = "snake_case")]
pub enum Event {
    Inspect {
        entries: usize,
        live_raw_imports: u64,
        unsupported_exportable_creations: u64,
    },
    Validate,
    PrepareMulticast,
    SaveAllocations(Transfer),
    PrepareUnicast,
    StateWrite,
    LoadAllocations(Transfer),
    RestoreUnicast,
    RestoreMulticast,
}

pub fn write(event: Event, start: Instant, participants: usize) -> io::Result<()> {
    #[derive(Serialize)]
    struct Report {
        #[serde(flatten)]
        event: Event,
        status: &'static str,
        elapsed_ms: f64,
        participants: usize,
    }
    let report = Report {
        event,
        status: "ok",
        elapsed_ms: start.elapsed().as_secs_f64() * 1000.0,
        participants,
    };
    let mut output = io::stdout().lock();
    serde_json::to_writer(&mut output, &report)?;
    output.write_all(b"\n")?;
    // Machine-readable progress must remain visible during a blocked collective.
    output.flush()
}
