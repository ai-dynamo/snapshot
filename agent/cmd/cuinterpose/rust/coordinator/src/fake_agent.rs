// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! The agent's side of a relay's session socket, over an in-memory session with
//! blocking reads, as relay tests need it. Each request runs on its own thread, so
//! answers arrive in any order, and a failed session answers every read with ABORTED.

/// A member rank, as in `group::Rank`, which integration tests cannot import.
type Rank = u32;
use cuinterpose_protocol::{self as protocol, MAX_MESSAGE_BYTES};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::os::unix::net::UnixStream;
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

#[derive(Deserialize)]
struct Request {
    op: String,
    id: u64,
    #[serde(default)]
    key: String,
    #[serde(default, with = "serde_bytes")]
    value: Option<Vec<u8>>,
    #[serde(default)]
    wait_ms: u64,
    #[serde(default)]
    reason: String,
}

#[derive(Serialize)]
struct Response {
    id: u64,
    ok: bool,
    #[serde(with = "serde_bytes", skip_serializing_if = "Option::is_none")]
    value: Option<Vec<u8>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    code: Option<&'static str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    reason: Option<String>,
}

#[derive(Default)]
struct Session {
    values: BTreeMap<String, Vec<u8>>,
    /// The reason the session failed, once it has.
    failed: Option<String>,
    /// Every `fail` request, with the rank that sent it.
    fails: Vec<(Rank, String)>,
}

#[derive(Default)]
pub struct FakeAgent {
    session: Arc<(Mutex<Session>, Condvar)>,
}

impl FakeAgent {
    /// Serve the relay of `rank` and return the relay's end of its socket.
    pub fn attach(&self, rank: Rank) -> UnixStream {
        let (relay, agent) = UnixStream::pair().unwrap();
        let session = Arc::clone(&self.session);
        std::thread::spawn(move || {
            let writer = Arc::new(Mutex::new(agent.try_clone().unwrap()));
            while let Ok(Some(request)) = protocol::receive_session::<Request>(&agent) {
                let session = Arc::clone(&session);
                let writer = Arc::clone(&writer);
                std::thread::spawn(move || {
                    let response = serve(&session, rank, request);
                    let _ = protocol::send_session(&writer.lock().unwrap(), &response);
                });
            }
        });
        relay
    }

    /// The `fail` requests so far, by rank.
    pub fn fails(&self) -> Vec<(Rank, String)> {
        self.session.0.lock().unwrap().fails.clone()
    }

    pub fn keys(&self) -> Vec<String> {
        self.session
            .0
            .lock()
            .unwrap()
            .values
            .keys()
            .cloned()
            .collect()
    }
}

fn serve(session: &(Mutex<Session>, Condvar), rank: Rank, request: Request) -> Response {
    let (lock, changed) = session;
    let mut session = lock.lock().unwrap();
    let answer =
        |result: Result<Option<Vec<u8>>, &'static str>, reason: Option<String>| match result {
            Ok(value) => Response {
                id: request.id,
                ok: true,
                value,
                code: None,
                reason: None,
            },
            Err(code) => Response {
                id: request.id,
                ok: false,
                value: None,
                code: Some(code),
                reason,
            },
        };
    if request.op == "fail" {
        session.fails.push((rank, request.reason.clone()));
        if let Some(reason) = &session.failed {
            return answer(Err("ABORTED"), Some(reason.clone()));
        }
        session.failed = Some(request.reason);
        changed.notify_all();
        return answer(Ok(None), None);
    }
    let deadline = Instant::now() + Duration::from_millis(request.wait_ms);
    loop {
        if let Some(reason) = &session.failed {
            return answer(Err("ABORTED"), Some(reason.clone()));
        }
        match request.op.as_str() {
            "insert" => {
                let value = request.value.clone().unwrap_or_default();
                if value.len() > MAX_MESSAGE_BYTES {
                    return answer(Err("TOO_LARGE"), None);
                }
                if session
                    .values
                    .get(&request.key)
                    .is_some_and(|known| *known != value)
                {
                    return answer(Err("EXISTS"), None);
                }
                session.values.insert(request.key.clone(), value);
                changed.notify_all();
                return answer(Ok(None), None);
            }
            "delete" => {
                session.values.remove(&request.key);
                return answer(Ok(None), None);
            }
            "get" => {
                if let Some(value) = session.values.get(&request.key) {
                    return answer(Ok(Some(value.clone())), None);
                }
                if request.wait_ms == 0 {
                    return answer(Err("NOT_FOUND"), None);
                }
                let now = Instant::now();
                if now >= deadline {
                    return answer(Err("NOT_YET"), None);
                }
                session = changed.wait_timeout(session, deadline - now).unwrap().0;
            }
            _ => return answer(Err("INVALID"), None),
        }
    }
}
