// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use serde::Serialize;
use std::io::{self, Write};
use std::time::Instant;

#[derive(Clone, Copy, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Phase {
    Inspect,
    Validate,
    PrepareMulticast,
    SaveAllocations,
    PrepareUnicast,
    StateWrite,
    Handshake,
    LoadAllocations,
    RestoreUnicast,
    RestoreMulticast,
}

#[derive(Serialize)]
#[serde(untagged)]
pub enum Metrics {
    None {},
    Inspection {
        records: usize,
        live_raw_imports: u64,
        unsupported_exportable_creations: u64,
    },
    Transfer {
        allocation_count: usize,
        allocation_bytes: u64,
        gb_per_s: f64,
        copy_gb_per_s: f64,
    },
}

#[derive(Serialize)]
struct Report {
    phase: Phase,
    status: &'static str,
    elapsed_ms: f64,
    participants: usize,
    #[serde(flatten)]
    metrics: Metrics,
}

pub fn write(
    phase: Phase,
    start: Instant,
    participants: usize,
    metrics: Metrics,
) -> io::Result<()> {
    let report = Report {
        phase,
        status: "ok",
        elapsed_ms: start.elapsed().as_secs_f64() * 1000.0,
        participants,
        metrics,
    };
    let mut output = io::stdout().lock();
    serde_json::to_writer(&mut output, &report)?;
    output.write_all(b"\n")?;
    // Progress must be visible while a subsequent collective is blocked.
    output.flush()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn transfer_report_has_numeric_metrics() {
        let report = Report {
            phase: Phase::LoadAllocations,
            status: "ok",
            elapsed_ms: 12.5,
            participants: 8,
            metrics: Metrics::Transfer {
                allocation_count: 3,
                allocation_bytes: 1_610_612_736,
                gb_per_s: 41.2,
                copy_gb_per_s: 50.0,
            },
        };
        let value = serde_json::to_value(report).unwrap();
        assert_eq!(value["phase"], "load_allocations");
        assert_eq!(value["status"], "ok");
        assert_eq!(value["allocation_bytes"], 1_610_612_736_u64);
        assert_eq!(value["gb_per_s"], 41.2);
        assert!(value.get("records").is_none());
    }
}
