// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Short-lived participant validation, global CUDA lifecycle barriers, and state publication.

mod report;
mod state;
mod topology;

use cuinterpose_protocol::{
    self as protocol, Operation, ParticipantId, Record, Reply, Request, Response,
};
use report::{Metrics, Phase, write as report};
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::time::Instant;
use topology::Allocation;

type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;

#[derive(Clone, Default)]
struct Participant {
    endpoint: String,
    id: ParticipantId,
    records: Vec<Record>,
    raw_imports: u64,
    unsupported: u64,
}

impl Participant {
    fn exchange(&mut self, operation: Operation, bytes: Option<u64>) -> Result<u32> {
        let stream = UnixStream::connect(&self.endpoint)
            .map_err(|error| format!("{}: {operation:?} connect failed: {error}", self.endpoint))?;
        let timeout = Some(cuinterpose_protocol::timeout(operation));
        stream.set_read_timeout(timeout)?;
        stream.set_write_timeout(timeout)?;
        let request = match operation {
            Operation::Handshake => Request::Handshake,
            Operation::Inspect => Request::Inspect {
                participant: self.id,
            },
            _ => Request::Execute {
                participant: self.id,
                operation,
            },
        };
        protocol::send(&stream, &request, None)
            .map_err(|error| format!("{}: {operation:?} send failed: {error}", self.endpoint))?;
        let (response, descriptor): (Response, _) = protocol::receive(&stream)
            .map_err(|error| format!("{}: {operation:?} receive failed: {error}", self.endpoint))?;
        if descriptor.is_some()
            || (operation != Operation::Handshake && response.participant != self.id)
        {
            return Err(format!(
                "{}: invalid response identity, operation, or descriptor",
                self.endpoint
            )
            .into());
        }
        match response
            .result
            .map_err(|error| format!("{}: {error}", self.endpoint))?
        {
            Reply::Handshake if operation == Operation::Handshake => {
                self.id = response.participant;
            }
            Reply::Inspection {
                records,
                live_raw_imports,
                unsupported_creations,
            } if operation == Operation::Inspect => {
                self.records = records;
                self.raw_imports = live_raw_imports;
                self.unsupported = unsupported_creations;
            }
            Reply::Completed {
                operation: actual,
                bytes: moved,
                copy_us,
            } if operation == actual && bytes.unwrap_or(0) == moved => return Ok(copy_us),
            _ => {
                return Err(format!(
                    "{}: unexpected {operation:?} response or transfer size",
                    self.endpoint
                )
                .into());
            }
        }
        Ok(0)
    }
}

/// Scoped threads are the global barrier. Even if one exchange fails, all
/// started exchanges are joined before returning the first failure.
fn command_all(
    participants: &mut [Participant],
    operation: Operation,
    allocations: Option<&[Allocation]>,
) -> Result<u32> {
    std::thread::scope(|scope| {
        let mut jobs = Vec::with_capacity(participants.len());
        for participant in participants {
            let bytes = allocations
                .map(|entries| {
                    entries
                        .iter()
                        .filter(|a| a.preserve_content && a.creator == participant.id)
                        .try_fold(0u64, |sum, a| {
                            sum.checked_add(a.size).ok_or("allocation size overflow")
                        })
                })
                .transpose()?;
            jobs.push(
                std::thread::Builder::new()
                    .spawn_scoped(scope, move || participant.exchange(operation, bytes))?,
            );
        }
        let mut longest = 0;
        let mut failure = None;
        for job in jobs {
            match job.join() {
                Ok(Ok(copy_us)) => longest = longest.max(copy_us),
                Ok(Err(error)) => {
                    failure.get_or_insert(error);
                }
                Err(_) => {
                    failure.get_or_insert("participant exchange panicked".into());
                }
            }
        }
        match failure {
            Some(error) => Err(error),
            None => Ok(longest),
        }
    })
}

fn inspect(participants: &mut [Participant]) -> Result<()> {
    for participant in &mut *participants {
        participant.exchange(Operation::Handshake, None)?;
    }
    for participant in participants {
        participant.exchange(Operation::Inspect, None)?;
    }
    Ok(())
}

fn transfer(
    participants: &mut [Participant],
    operation: Operation,
    allocations: &[Allocation],
    phase: Phase,
) -> Result<()> {
    let start = Instant::now();
    let copy_us = command_all(participants, operation, Some(allocations)).map_err(|error| {
        format!(
            "allocation {}: {error}",
            if operation == Operation::SaveAllocations {
                "save"
            } else {
                "load"
            }
        )
    })?;
    let (count, bytes) = allocations.iter().filter(|a| a.preserve_content).try_fold(
        (0usize, 0u64),
        |(n, sum), a| {
            sum.checked_add(a.size)
                .map(|b| (n + 1, b))
                .ok_or("allocation size overflow")
        },
    )?;
    report(
        phase,
        start,
        participants.len(),
        Metrics::Transfer {
            allocation_count: count,
            allocation_bytes: bytes,
            gb_per_s: bytes as f64 / 1e9 / start.elapsed().as_secs_f64(),
            copy_gb_per_s: if copy_us == 0 {
                0.0
            } else {
                bytes as f64 / 1000.0 / f64::from(copy_us)
            },
        },
    )?;
    Ok(())
}

fn run() -> Result<()> {
    let argv: Vec<String> = std::env::args().skip(1).collect();
    let args: Vec<_> = argv.iter().map(String::as_str).collect();
    let [
        mode @ ("--prepare" | "--restore"),
        "--proc-root",
        proc_root,
        "--checkpoint-dir",
        checkpoint,
        "--control-dir",
        control,
        processes @ ..,
    ] = args.as_slice()
    else {
        return Err("usage: cuinterpose-coordinator (--prepare|--restore) --proc-root PATH --checkpoint-dir PATH --control-dir PATH --process OBSERVED_PID NAMESPACE_PID...".into());
    };
    if processes.is_empty() || !processes.len().is_multiple_of(3) {
        return Err("expected --process OBSERVED_PID NAMESPACE_PID".into());
    }
    let prepare = *mode == "--prepare";
    if !control.starts_with('/') {
        return Err("--control-dir must be an absolute path".into());
    }
    let path = PathBuf::from(checkpoint).join("cuinterpose.state");
    // Validate the artifact before contacting a restored process.
    let mut expected = if prepare {
        Vec::new()
    } else {
        state::read(&path).map_err(|error| format!("cannot parse {}: {error}", path.display()))?
    };
    let mut participants = Vec::with_capacity(processes.len() / 3);
    for process in processes.chunks_exact(3) {
        let ["--process", observed, namespace] = process else {
            return Err("expected --process OBSERVED_PID NAMESPACE_PID".into());
        };
        let observed: i32 = observed.parse()?;
        let namespace: i32 = namespace.parse()?;
        if observed <= 0 || namespace <= 0 {
            return Err("process IDs must be positive".into());
        }
        let endpoint = if proc_root.is_empty() {
            format!("{control}/cuinterpose-{namespace}.sock")
        } else {
            format!("{proc_root}/{observed}/root{control}/cuinterpose-{namespace}.sock")
        };
        if endpoint.len() >= 108 {
            return Err("control socket path does not fit in sun_path".into());
        }
        participants.push(Participant {
            endpoint,
            ..Participant::default()
        });
    }
    if prepare {
        let start = Instant::now();
        inspect(&mut participants)?;
        report(
            Phase::Inspect,
            start,
            participants.len(),
            Metrics::Inspection {
                records: participants.iter().map(|p| p.records.len()).sum(),
                live_raw_imports: participants.iter().map(|p| p.raw_imports).sum(),
                unsupported_exportable_creations: participants.iter().map(|p| p.unsupported).sum(),
            },
        )?;
        for participant in &participants {
            if participant.raw_imports != 0 {
                return Err(format!(
                    "prepare refused: {} holds {} live raw imports",
                    participant.endpoint, participant.raw_imports
                )
                .into());
            }
            if participant.unsupported != 0 {
                return Err(format!("prepare refused: {} created {} CUDA resources with unsupported exportable handle types", participant.endpoint, participant.unsupported).into());
            }
        }
        let start = Instant::now();
        let allocations = topology::validate(&participants)?;
        report(Phase::Validate, start, participants.len(), Metrics::None {})?;
        let start = Instant::now();
        command_all(&mut participants, Operation::PrepareMulticast, None)
            .map_err(|error| format!("multicast teardown: {error}"))?;
        report(
            Phase::PrepareMulticast,
            start,
            participants.len(),
            Metrics::None {},
        )?;
        transfer(
            &mut participants,
            Operation::SaveAllocations,
            &allocations,
            Phase::SaveAllocations,
        )?;
        let start = Instant::now();
        command_all(&mut participants, Operation::PrepareUnicast, None)?;
        report(
            Phase::PrepareUnicast,
            start,
            participants.len(),
            Metrics::None {},
        )?;
        let start = Instant::now();
        state::write_atomic(&path, &mut participants)?;
        report(
            Phase::StateWrite,
            start,
            participants.len(),
            Metrics::None {},
        )?;
    } else {
        let start = Instant::now();
        for participant in &mut participants {
            participant.exchange(Operation::Handshake, None)?;
        }
        participants.sort_by_key(|p| p.id);
        expected.sort_by_key(|p| p.id);
        if participants
            .iter()
            .map(|p| p.id)
            .ne(expected.iter().map(|p| p.id))
        {
            return Err("restored processes do not match the checkpointed participants".into());
        }
        let allocations = topology::validate(&expected)?;
        report(
            Phase::Handshake,
            start,
            participants.len(),
            Metrics::None {},
        )?;
        transfer(
            &mut participants,
            Operation::LoadAllocations,
            &allocations,
            Phase::LoadAllocations,
        )?;
        let start = Instant::now();
        command_all(&mut participants, Operation::RestoreUnicast, None)?;
        report(
            Phase::RestoreUnicast,
            start,
            participants.len(),
            Metrics::None {},
        )?;
        let start = Instant::now();
        for operation in [
            Operation::RestoreMulticastCreators,
            Operation::RestoreMulticastImporters,
            Operation::RestoreMulticastDevices,
            Operation::RestoreMulticastBindings,
        ] {
            command_all(&mut participants, operation, None)?;
        }
        report(
            Phase::RestoreMulticast,
            start,
            participants.len(),
            Metrics::None {},
        )?;
        let start = Instant::now();
        inspect(&mut participants)?;
        topology::validate(&participants)?;
        participants.sort_by_key(|p| p.id);
        for (actual, expected) in participants.iter_mut().zip(&mut expected) {
            actual.records.sort();
            expected.records.sort();
            if actual.id != expected.id || actual.records != expected.records {
                return Err("restored topology does not match the checkpoint".into());
            }
        }
        report(Phase::Validate, start, participants.len(), Metrics::None {})?;
    }
    Ok(())
}

fn main() -> std::process::ExitCode {
    match run() {
        Ok(()) => std::process::ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("cuinterpose-coordinator: {error}");
            std::process::ExitCode::FAILURE
        }
    }
}
