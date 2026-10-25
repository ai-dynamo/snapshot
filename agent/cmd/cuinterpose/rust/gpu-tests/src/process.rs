// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Fresh test processes.
//!
//! The frontend must be preloaded before a process resolves any CUDA symbol, so the test
//! process never runs a scenario itself. [`run`] and [`spawn`] start the current test
//! again, as `--exact <test>`, with a role name in the environment. The re-run test
//! function reaches the same call: [`run`] then runs its closure, and a test that spawns
//! roles must dispatch [`role`] before it starts anything. Code before those calls runs in
//! every process of the test, so it must not have side effects that only the test process
//! should perform.
//!
//! Every process of one test shares its work directory, which holds the control socket
//! directory, checkpoints, and compiled fixtures.

use crate::shim;
use cuinterpose_protocol::{receive, send, socket_path};
use serde::{Serialize, de::DeserializeOwned};
use std::cell::Cell;
use std::env;
use std::fs::{self, File};
use std::io::{Read, Seek};
use std::os::fd::{AsRawFd, BorrowedFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::net::UnixStream;
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::{Path, PathBuf};
use std::process::{self, Command, ExitStatus, Stdio};
use std::sync::OnceLock;
use std::thread;
use std::time::{Duration, Instant};
use tempfile::TempDir;

const ROLE: &str = "CUINTERPOSE_TEST_ROLE";
const CHANNEL: &str = "CUINTERPOSE_TEST_CHANNEL";
const WORK_DIR: &str = "CUINTERPOSE_TEST_DIR";
/// The core creates control sockets in this directory.
const SOCKET_DIR: &str = "CUINTERPOSE_SOCKET_DIR";
const RUN: &str = "run";
/// Bounds every wait that correct code finishes quickly, so only a hang reaches it.
pub const TIMEOUT: Duration = Duration::from_secs(180);

thread_local! {
    // libtest runs each test on its own thread of the test process.
    static TEST_WORK_DIR: TempDir = {
        let directory = tempfile::Builder::new().prefix("cuinterpose-test-").tempdir().unwrap();
        fs::create_dir(directory.path().join("sockets")).unwrap();
        directory
    };
    static RAN: Cell<bool> = const { Cell::new(false) };
}

/// The role this process runs, or `None` in the test process.
pub fn role() -> Option<String> {
    env::var(ROLE).ok()
}

/// The directory shared by every process of the current test.
pub fn work_dir() -> PathBuf {
    match env::var_os(WORK_DIR) {
        Some(directory) => directory.into(),
        None => TEST_WORK_DIR.with(|directory| directory.path().to_owned()),
    }
}

pub fn socket_dir() -> PathBuf {
    work_dir().join("sockets")
}

/// This process's PID, which names its control socket.
pub fn pid() -> u32 {
    process::id()
}

/// This process's control socket.
pub fn endpoint() -> PathBuf {
    socket_path(&socket_dir(), pid())
}

/// How to start a test process. The default preloads the frontend.
#[derive(Default)]
pub struct Options {
    unshimmed: bool,
    env: Vec<(String, String)>,
    cwd: Option<PathBuf>,
}

impl Options {
    pub fn unshimmed() -> Self {
        Self {
            unshimmed: true,
            ..Self::default()
        }
    }

    /// Overrides one variable, including `LD_PRELOAD`.
    pub fn env(mut self, name: &str, value: impl Into<String>) -> Self {
        self.env.push((name.into(), value.into()));
        self
    }

    pub fn cwd(mut self, directory: impl Into<PathBuf>) -> Self {
        self.cwd = Some(directory.into());
        self
    }

    fn apply(&self, command: &mut Command) {
        if self.unshimmed {
            command.env_remove("LD_PRELOAD");
        } else {
            command.env("LD_PRELOAD", &shim::artifacts().frontend);
        }
        command.envs(self.env.iter().map(|(name, value)| (name, value)));
        if let Some(directory) = &self.cwd {
            command.current_dir(directory);
        }
    }
}

/// Runs `body` in a fresh process with the frontend preloaded and returns its stderr.
/// A test calls this at most once, because every call starts the same role.
pub fn run(body: impl FnOnce()) -> String {
    run_with(Options::default(), body)
}

pub fn run_with(options: Options, body: impl FnOnce()) -> String {
    match role().as_deref() {
        Some(RUN) => {
            body();
            process::exit(0)
        }
        Some(other) => panic!("role {other} reached run(); dispatch roles before running"),
        None => {
            assert!(!RAN.replace(true), "a test can run only one scenario");
            spawn_with(RUN, options).wait()
        }
    }
}

/// Runs an executable with the frontend preloaded and returns its stderr.
pub fn run_executable(path: &Path, arguments: &[&str], options: Options) -> String {
    if role().is_none() {
        shim::require_test_environment();
    }
    let mut command = Command::new(path);
    command.args(arguments).env(SOCKET_DIR, socket_dir());
    options.apply(&mut command);
    Child::start(command, format!("{} {arguments:?}", path.display()), None).wait()
}

/// Starts `role` as a fresh process of the current test with the frontend preloaded.
pub fn spawn(role: &str) -> Child {
    spawn_with(role, Options::default())
}

pub fn spawn_with(role: &str, options: Options) -> Child {
    let current = self::role();
    assert_ne!(
        current.as_deref(),
        Some(role),
        "the test did not dispatch role {role}"
    );
    if current.is_none() {
        shim::require_test_environment();
    }
    let (parent, child) = UnixStream::pair().unwrap();
    let mut command = test_command(role);
    options.apply(&mut command);
    let inherited = child.as_raw_fd();
    let build = shim::artifacts().frontend.parent().unwrap();
    command
        .env(CHANNEL, inherited.to_string())
        .env(WORK_DIR, work_dir())
        .env(SOCKET_DIR, socket_dir())
        // A relative build directory would not resolve from a child's working directory.
        .env(shim::BUILD_DIR, build);
    // SAFETY: fcntl is async-signal-safe, and the descriptor stays open until spawn
    // returns.
    unsafe {
        command.pre_exec(move || {
            let fd = BorrowedFd::borrow_raw(inherited);
            rustix::io::fcntl_setfd(fd, rustix::io::FdFlags::empty())?;
            Ok(())
        });
    }
    let name = format!("{} [{role}]", current_test());
    let started = Child::start(command, name, Some(Channel::new(parent)));
    drop(child);
    started
}

/// Replaces this process with the current test in `role`, keeping its PID, environment,
/// and output.
pub fn exec(role: &str) -> ! {
    let error = test_command(role).exec();
    panic!("exec {role}: {error}")
}

/// The channel to the process that started this role.
pub fn channel() -> &'static Channel {
    static INHERITED: OnceLock<Channel> = OnceLock::new();
    INHERITED.get_or_init(|| {
        let fd: RawFd = env::var(CHANNEL)
            .expect("only started roles have a channel")
            .parse()
            .unwrap();
        // SAFETY: the starting process passed this descriptor only to this process.
        Channel::new(UnixStream::from(unsafe { OwnedFd::from_raw_fd(fd) }))
    })
}

fn current_test() -> String {
    thread::current()
        .name()
        .filter(|name| *name != "main")
        .expect("start test processes from the test's own thread")
        .to_owned()
}

fn test_command(role: &str) -> Command {
    let mut command = Command::new(env::current_exe().unwrap());
    command
        .args(["--exact", &current_test(), "--nocapture", "--quiet"])
        .arg("--test-threads=1")
        .env(ROLE, role);
    command
}

/// A socket that carries MessagePack values and descriptors between test processes.
pub struct Channel {
    stream: UnixStream,
}

impl Channel {
    fn new(stream: UnixStream) -> Self {
        stream.set_read_timeout(Some(TIMEOUT)).unwrap();
        Self { stream }
    }

    pub fn send<T: Serialize>(&self, message: &T) {
        send(&self.stream, message, None).expect("send to the other test process");
    }

    pub fn send_fd<T: Serialize>(&self, message: &T, fd: &OwnedFd) {
        send(&self.stream, message, Some(fd)).expect("send to the other test process");
    }

    pub fn recv<T: DeserializeOwned>(&self) -> T {
        let (message, fd) = self.receive();
        assert!(fd.is_none(), "unexpected descriptor");
        message
    }

    pub fn recv_fd<T: DeserializeOwned>(&self) -> (T, OwnedFd) {
        let (message, fd) = self.receive();
        (message, fd.expect("expected a descriptor"))
    }

    fn receive<T: DeserializeOwned>(&self) -> (T, Option<OwnedFd>) {
        receive(&self.stream)
            .unwrap_or_else(|error| panic!("receive from the other test process: {error}"))
    }
}

/// A started test process. Dropping it kills the process if it is still running and,
/// during a panic, prints its output.
pub struct Child {
    name: String,
    process: process::Child,
    channel: Option<Channel>,
    stdout: File,
    stderr: File,
    reported: bool,
}

impl Child {
    /// Starts `command` with its output captured. Only re-runs of the test binary have a
    /// channel.
    pub(crate) fn start(mut command: Command, name: String, channel: Option<Channel>) -> Self {
        let stdout = tempfile::tempfile().unwrap();
        let stderr = tempfile::tempfile().unwrap();
        let process = command
            .stdin(Stdio::null())
            .stdout(stdout.try_clone().unwrap())
            .stderr(stderr.try_clone().unwrap())
            .spawn()
            .unwrap_or_else(|error| panic!("start {name}: {error}"));
        Self {
            name,
            process,
            channel,
            stdout,
            stderr,
            reported: false,
        }
    }

    pub fn pid(&self) -> u32 {
        self.process.id()
    }

    pub fn send<T: Serialize>(&self, message: &T) {
        self.channel().send(message);
    }

    pub fn send_fd<T: Serialize>(&self, message: &T, fd: &OwnedFd) {
        self.channel().send_fd(message, fd);
    }

    pub fn recv<T: DeserializeOwned>(&self) -> T {
        self.channel().recv()
    }

    pub fn recv_fd<T: DeserializeOwned>(&self) -> (T, OwnedFd) {
        self.channel().recv_fd()
    }

    /// Sends `message` and waits for the process to reply "ok".
    pub fn command<T: Serialize>(&self, message: &T) {
        self.send(message);
        assert_eq!(self.recv::<String>(), "ok");
    }

    /// Waits for a successful exit and returns stderr.
    pub fn wait(self) -> String {
        self.wait_for(|status| status.success(), "exit status 0")
    }

    /// Waits for the process to die from `signal` and returns stderr.
    pub fn wait_for_signal(self, signal: i32) -> String {
        self.wait_for(
            |status| status.signal() == Some(signal),
            &format!("signal {signal}"),
        )
    }

    fn wait_for(mut self, expected: impl Fn(ExitStatus) -> bool, description: &str) -> String {
        let (status, stdout, stderr) = self.finish();
        assert!(
            expected(status),
            "{} {status}, expected {description}\nstdout:\n{stdout}\nstderr:\n{stderr}",
            self.name
        );
        stderr
    }

    /// Waits for exit and returns the status and output.
    pub(crate) fn finish(&mut self) -> (ExitStatus, String, String) {
        let deadline = Instant::now() + TIMEOUT;
        let status = loop {
            if let Some(status) = self.process.try_wait().unwrap() {
                break status;
            }
            assert!(
                Instant::now() < deadline,
                "{} did not exit within {TIMEOUT:?}",
                self.name
            );
            thread::sleep(Duration::from_millis(10));
        };
        let (stdout, stderr) = self.output();
        self.reported = true;
        // A re-run whose --exact filter matched no test would exit 0 without running.
        assert!(
            self.channel.is_none() || stdout.contains("running 1 test"),
            "{} ran no test\nstdout:\n{stdout}\nstderr:\n{stderr}",
            self.name
        );
        (status, stdout, stderr)
    }

    fn channel(&self) -> &Channel {
        self.channel.as_ref().expect("this process has no channel")
    }

    fn output(&self) -> (String, String) {
        let read = |mut file: &File| {
            let mut contents = Vec::new();
            file.rewind().unwrap();
            file.read_to_end(&mut contents).unwrap();
            String::from_utf8_lossy(&contents).into_owned()
        };
        (read(&self.stdout), read(&self.stderr))
    }
}

impl Drop for Child {
    fn drop(&mut self) {
        if matches!(self.process.try_wait(), Ok(None)) {
            let _ = self.process.kill();
            let _ = self.process.wait();
        }
        if thread::panicking() && !self.reported {
            let (stdout, stderr) = self.output();
            eprintln!(
                "{} PID {}\nstdout:\n{stdout}\nstderr:\n{stderr}",
                self.name,
                self.pid()
            );
        }
    }
}
