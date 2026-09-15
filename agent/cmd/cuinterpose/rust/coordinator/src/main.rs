// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

mod report;
mod state;
mod topology;

use cuinterpose_protocol::{Header, Identity, MAX_RECORDS, Operation, RECORD_SIZE, Record};
use report::{Metrics, Phase, write as report};
use std::io::Read;
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::time::Instant;
use topology::Allocation;

type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;

#[derive(Clone)]
struct Participant {
    endpoint: String,
    id: Identity,
    records: Vec<Record>,
    raw_imports: u32,
    unsupported: u32,
}

impl Default for Participant {
    fn default() -> Self {
        Self {
            endpoint: String::new(),
            id: [0; 33],
            records: Vec::new(),
            raw_imports: 0,
            unsupported: 0,
        }
    }
}

impl Participant {
    fn exchange(&mut self, operation: Operation, bytes: Option<u64>) -> Result<u32> {
        let mut stream = UnixStream::connect(&self.endpoint)
            .map_err(|error| format!("{}: {operation:?} connect failed: {error}", self.endpoint))?;
        let timeout = Some(cuinterpose_protocol::timeout(operation));
        stream.set_read_timeout(timeout)?;
        stream.set_write_timeout(timeout)?;
        let mut request = Header::new(
            operation,
            if operation == Operation::Handshake {
                [0; 33]
            } else {
                self.id
            },
        );
        request.payload_size = bytes.unwrap_or(0);
        cuinterpose_protocol::send_header(&stream, &request, None)
            .map_err(|error| format!("{}: {operation:?} send failed: {error}", self.endpoint))?;
        let (response, descriptor) = cuinterpose_protocol::receive_header(&stream)
            .map_err(|error| format!("{}: {operation:?} receive failed: {error}", self.endpoint))?;
        if response.status != 0 {
            let end = response.message.iter().position(|b| *b == 0).unwrap_or(96);
            return Err(format!(
                "{}: {}",
                self.endpoint,
                String::from_utf8_lossy(&response.message[..end])
            )
            .into());
        }
        if descriptor.is_some()
            || response.operation != operation
            || (operation != Operation::Handshake && response.participant != self.id)
        {
            return Err(format!(
                "{}: invalid response identity, operation, or descriptor",
                self.endpoint
            )
            .into());
        }
        if let Some(expected) = bytes {
            if response.count != 0 || response.payload_size != expected {
                return Err(format!(
                    "{}: allocation transfer moved {} bytes, expected {expected}",
                    self.endpoint, response.payload_size
                )
                .into());
            }
        } else {
            if response.count as usize > MAX_RECORDS
                || response.payload_size != u64::from(response.count) * RECORD_SIZE as u64
            {
                return Err("invalid response record count or payload size".into());
            }
            let mut records = Vec::with_capacity(response.count as usize);
            for _ in 0..response.count {
                let mut bytes = [0; RECORD_SIZE];
                stream.read_exact(&mut bytes).map_err(|error| {
                    format!(
                        "{}: {operation:?} record receive failed: {error}",
                        self.endpoint
                    )
                })?;
                records.push(Record::decode(&bytes)?);
            }
            if operation == Operation::Inspect {
                self.records = records;
            }
        }
        if operation == Operation::Handshake {
            self.id = cuinterpose_protocol::parse_identity(&response.participant[..32])?;
            if response.participant[32] != 0 {
                return Err("invalid participant identity".into());
            }
        }
        if matches!(operation, Operation::Handshake | Operation::Inspect) {
            self.raw_imports = response.live_raw_imports;
            self.unsupported = response.unsupported_creations;
        }
        Ok(response.copy_us)
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
    let argv: Vec<String> = std::env::args().collect();
    if argv.len() < 11
        || (argv.len() - 8) % 3 != 0
        || !matches!(argv[1].as_str(), "--prepare" | "--restore")
        || argv[2] != "--proc-root"
        || argv[4] != "--checkpoint-dir"
        || argv[6] != "--control-dir"
    {
        return Err("usage: cuinterpose-coordinator (--prepare|--restore) --proc-root PATH --checkpoint-dir PATH --control-dir PATH --process OBSERVED_PID NAMESPACE_PID...".into());
    }
    let prepare = argv[1] == "--prepare";
    let control = &argv[7];
    if !control.starts_with('/') {
        return Err("--control-dir must be an absolute path".into());
    }
    let path = PathBuf::from(&argv[5]).join("cuinterpose.state");
    // Validate the artifact before contacting a restored process.
    let mut expected = if prepare {
        Vec::new()
    } else {
        state::read(&path).map_err(|error| format!("cannot parse {}: {error}", path.display()))?
    };
    let mut participants = Vec::new();
    for process in argv[8..].chunks_exact(3) {
        if process[0] != "--process" {
            return Err("expected --process OBSERVED_PID NAMESPACE_PID".into());
        }
        let observed: i32 = process[1].parse()?;
        let namespace: i32 = process[2].parse()?;
        if observed <= 0 || namespace <= 0 {
            return Err("process IDs must be positive".into());
        }
        let endpoint = if argv[3].is_empty() {
            format!("{control}/cuinterpose-{namespace}.sock")
        } else {
            format!(
                "{}/{observed}/root{control}/cuinterpose-{namespace}.sock",
                argv[3]
            )
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
                live_raw_imports: participants.iter().map(|p| u64::from(p.raw_imports)).sum(),
                unsupported_exportable_creations: participants
                    .iter()
                    .map(|p| u64::from(p.unsupported))
                    .sum(),
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
