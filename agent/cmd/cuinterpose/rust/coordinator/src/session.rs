// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! A relay reaches its attempt's session through the socket that its agent created and
//! passed on. The agent forwards each request to the coordinator with the relay's
//! identity, and answers every request, in any order, by its ID.

use anyhow::{Context, Result, bail};
use cuinterpose_protocol as protocol;
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;
use std::os::fd::{BorrowedFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::net::UnixStream;
use std::time::Duration;

#[derive(Serialize)]
#[serde(tag = "op", rename_all = "snake_case")]
enum Request<'a> {
    Insert {
        id: u64,
        key: &'a str,
        #[serde(with = "serde_bytes")]
        value: &'a [u8],
    },
    Get {
        id: u64,
        key: &'a str,
        wait_ms: u64,
    },
    Delete {
        id: u64,
        key: &'a str,
    },
    Fail {
        id: u64,
        reason: &'a str,
    },
}

#[derive(Deserialize)]
struct Response {
    id: u64,
    ok: bool,
    #[serde(default, with = "serde_bytes")]
    value: Option<Vec<u8>>,
    #[serde(default)]
    code: Option<String>,
    #[serde(default)]
    reason: Option<String>,
}

/// The session failed or closed, so every later request fails the same way and calling
/// `fail` is pointless.
#[derive(Debug)]
pub struct Ended(pub String);

impl std::fmt::Display for Ended {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(formatter, "the session ended: {}", self.0)
    }
}

impl std::error::Error for Ended {}

pub struct Session {
    socket: UnixStream,
    next_id: u64,
    /// Requests whose answers no caller waits for anymore.
    abandoned: BTreeSet<u64>,
}

impl Session {
    pub fn new(socket: UnixStream) -> Self {
        Self {
            socket,
            next_id: 1,
            abandoned: BTreeSet::new(),
        }
    }

    /// Take ownership of the socket that the agent passed as descriptor `fd`.
    pub fn inherit(fd: RawFd) -> Result<Self> {
        // SAFETY: the descriptor is only borrowed until it is checked to be a socket.
        let borrowed = unsafe { BorrowedFd::borrow_raw(fd) };
        let stat = rustix::fs::fstat(borrowed)
            .with_context(|| format!("--session-fd {fd} is not an open descriptor"))?;
        if rustix::fs::FileType::from_raw_mode(stat.st_mode) != rustix::fs::FileType::Socket {
            bail!("--session-fd {fd} is not a socket");
        }
        // SAFETY: the agent passed this descriptor to this process alone, and nothing
        // else in the coordinator owns it.
        let socket = unsafe { OwnedFd::from_raw_fd(fd) };
        rustix::io::fcntl_setfd(&socket, rustix::io::FdFlags::CLOEXEC)?;
        Ok(Self::new(UnixStream::from(socket)))
    }

    pub fn insert(&mut self, key: &str, value: &[u8]) -> Result<()> {
        let id = self.send(|id| Request::Insert { id, key, value })?;
        self.answer(id, key)?;
        Ok(())
    }

    pub fn delete(&mut self, key: &str) -> Result<()> {
        let id = self.send(|id| Request::Delete { id, key })?;
        self.answer(id, key)?;
        Ok(())
    }

    /// End the attempt for every member. `reason` must be a condition reason.
    pub fn fail(&mut self, reason: &str) -> Result<()> {
        let id = self.send(|id| Request::Fail { id, reason })?;
        self.answer(id, reason)?;
        Ok(())
    }

    /// Read every key, waiting up to `wait` for each, with the requests in flight
    /// together. A key that is still absent reads as `None`.
    pub fn get_all(&mut self, keys: &[String], wait: Duration) -> Result<Vec<Option<Vec<u8>>>> {
        let wait_ms = u64::try_from(wait.as_millis()).unwrap_or(u64::MAX);
        let mut pending = Vec::with_capacity(keys.len());
        for key in keys {
            pending.push(self.send(|id| Request::Get { id, key, wait_ms })?);
        }
        let mut values = vec![None; keys.len()];
        let mut answered = 0;
        let result = (|| {
            while answered < keys.len() {
                let response = self.receive()?;
                let Some(index) = pending.iter().position(|id| *id == response.id) else {
                    bail!("the agent answered unknown request {}", response.id);
                };
                answered += 1;
                if response.ok {
                    values[index] = Some(response.value.unwrap_or_default());
                    continue;
                }
                match response.code.as_deref() {
                    Some("NOT_YET" | "NOT_FOUND") => {}
                    code => return Err(refusal(&keys[index], code, response.reason)),
                }
            }
            Ok(())
        })();
        if result.is_err() {
            // Answers to the remaining reads may still arrive before later answers.
            self.abandoned.extend(pending);
        }
        result.map(|()| values)
    }

    pub fn get(&mut self, key: &str, wait: Duration) -> Result<Option<Vec<u8>>> {
        Ok(self.get_all(&[key.to_owned()], wait)?.pop().flatten())
    }

    fn send<'a>(&mut self, request: impl FnOnce(u64) -> Request<'a>) -> Result<u64> {
        let id = self.next_id;
        self.next_id += 1;
        match protocol::send_session(&self.socket, &request(id)) {
            Err(error) if closed(&error) => {
                Err(Ended("the agent closed the session socket".into()).into())
            }
            result => result
                .map(|()| id)
                .context("send a session request to the agent"),
        }
    }

    fn receive(&mut self) -> Result<Response> {
        loop {
            let response = match protocol::receive_session::<Response>(&self.socket) {
                Err(error) if closed(&error) => None,
                result => result.context("receive a session answer from the agent")?,
            }
            .ok_or_else(|| Ended("the agent closed the session socket".into()))?;
            if !self.abandoned.remove(&response.id) {
                return Ok(response);
            }
        }
    }

    fn answer(&mut self, id: u64, subject: &str) -> Result<Response> {
        let response = self.receive()?;
        if response.id != id {
            bail!("the agent answered request {} instead of {id}", response.id);
        }
        if !response.ok {
            return Err(refusal(subject, response.code.as_deref(), response.reason));
        }
        Ok(response)
    }
}

/// Whether the agent closed its end of the socket, which a clean end of the stream also
/// shows.
fn closed(error: &protocol::Error) -> bool {
    matches!(error, protocol::Error::Io(error) if matches!(
        error.kind(),
        std::io::ErrorKind::BrokenPipe | std::io::ErrorKind::ConnectionReset
    ))
}

fn refusal(subject: &str, code: Option<&str>, reason: Option<String>) -> anyhow::Error {
    let code = code.unwrap_or("no code");
    let reason = reason.filter(|reason| !reason.is_empty());
    match code {
        // The session failed or closed, or the agent stopped serving it.
        "ABORTED" | "CLOSED" | "CANCELLED" => {
            let detail = match reason {
                Some(reason) => format!("{code} ({reason})"),
                None => code.to_owned(),
            };
            anyhow::Error::new(Ended(detail)).context(subject.to_owned())
        }
        _ => anyhow::anyhow!("{subject}: the session refused the request: {code}"),
    }
}

/// Whether `error` comes from a session that already ended.
pub fn ended(error: &anyhow::Error) -> bool {
    error.chain().any(|cause| cause.is::<Ended>())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn answers_of_a_session_that_ended_are_told_from_refusals() {
        for code in ["ABORTED", "CLOSED", "CANCELLED"] {
            assert!(ended(&refusal("key", Some(code), None)), "{code}");
        }
        for code in [Some("TOO_LARGE"), None] {
            assert!(!ended(&refusal("key", code, None)), "{code:?}");
        }
    }

    #[test]
    fn reset_socket_ends_the_session() {
        let (relay, agent) = UnixStream::pair().unwrap();
        let mut session = Session::new(relay);
        let id = session
            .send(|id| Request::Delete { id, key: "key" })
            .unwrap();
        // Closing a socket with unread data resets the connection.
        drop(agent);
        let error = session.answer(id, "key").map(drop).unwrap_err();
        assert!(ended(&error), "{error:#}");
    }
}
