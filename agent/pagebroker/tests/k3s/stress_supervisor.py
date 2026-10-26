#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Watch the endurance runner independently and own normal broker shutdown."""

import argparse
from pathlib import Path
import signal

from run import exited, write_json
from stress_config import Clock, RunSpec
from stress_control import Channel, Peer, own_record


class Supervisor:
    def __init__(self, spec, clock=None):
        self.spec, self.clock = spec, clock or Clock()
        self.channel = Channel(spec.control, spec.run_id)
        self.broker = self.runner = self.client = None
        self.client_token = None
        self.helper = self.cleanup_worker = None
        self.cleanup_worker_token = None
        self.interrupted = False
        self.next_beat = 0
        self.errors = []

    def error(self, message):
        if message not in self.errors and len(self.errors) < 8:
            self.errors.append(message)

    def pulse(self, watch=True):
        now = self.clock.monotonic()
        if now >= self.next_beat:
            self.channel.beat("supervisor", self.clock)
            self.next_beat = now + self.spec.timing.heartbeat
        if watch and self.runner is not None:
            self.runner.check()
            self.channel.check_heartbeat("runner", self.clock, self.spec.timing.stale)
        if watch and self.interrupted:
            raise InterruptedError("supervisor received a termination signal")

    def register_client(self):
        registration = self.channel.read("client.json")
        if registration is None or registration["token"] == self.client_token:
            return
        if self.client is not None:
            if not exited(self.client.fd):
                raise RuntimeError("client registration changed while the previous client was alive")
            self.client.close()
            self.client = None
        self.client_token = registration["token"]
        if registration["active"]:
            self.client = Peer.open(registration["peer"])
        self.channel.write("client-ack.json", token=self.client_token)

    def register_helper(self):
        if self.spec.mode != "s3":
            return
        registration = self.channel.read("helper-registration.json")
        if self.helper is None and self.channel.read("helper-heartbeat.json") is None:
            return
        if registration is not None:
            if self.helper is None:
                self.helper = Peer.open(registration["peer"])
            elif registration["peer"] != self.helper.record():
                raise RuntimeError("cleanup helper identity changed")
            self.helper.check()
            self.channel.check_heartbeat("helper", self.clock, self.spec.timing.stale)
            self.channel.write("helper-ack.json", peer=self.helper.record())
        registration = self.channel.read("cleanup-worker.json")
        if registration is None or registration["token"] == self.cleanup_worker_token:
            return
        if self.cleanup_worker is not None:
            if not exited(self.cleanup_worker.fd):
                raise RuntimeError("cleanup worker replaced while still alive")
            self.cleanup_worker.close()
            self.cleanup_worker = None
        self.cleanup_worker_token = registration["token"]
        if registration["active"]:
            if self.helper is None:
                raise RuntimeError("SDK worker registered without a helper")
            self.cleanup_worker = Peer.open(registration["peer"])
        self.channel.write("cleanup-worker-ack.json", token=self.cleanup_worker_token)

    def stop_helper(self, deadline, emergency):
        if self.spec.mode != "s3":
            return None
        if emergency:
            # No cleanup requests are issued on observer loss or uncertain broker activity.
            registration = self.channel.read("cleanup-worker.json")
            if registration and registration.get("active") and self.cleanup_worker is None:
                try:
                    self.cleanup_worker = Peer.open(registration["peer"])
                except (OSError, RuntimeError):
                    pass
            for peer in (self.cleanup_worker, self.helper):
                if peer is not None:
                    peer.send(signal.SIGKILL)
                    if not self.wait_exit(peer, min(deadline, self.clock.monotonic() + self.spec.timing.kill_wait)):
                        self.error("cleanup peer exit was not confirmed")
            return {"status": "ERROR", "remoteCleanup": "UNCERTAIN"}
        if self.helper is None:
            raise RuntimeError("required cleanup helper never registered")
        if self.cleanup_worker is not None and not exited(self.cleanup_worker.fd):
            raise RuntimeError("SDK worker remains active at shutdown")
        shutdown = self.channel.read("shutdown.json")
        self.channel.write("cleanup-stop.json", executionStatus=shutdown["status"] if shutdown else "ERROR")
        while True:
            self.pulse()
            reply = self.channel.read("cleanup-stopped.json")
            if reply is not None:
                if reply.get("status") != "STOPPED" or reply.get("helper") != self.helper.record():
                    raise RuntimeError("invalid cleanup final acknowledgment")
                break
            self.helper.check()
            if self.clock.monotonic() >= deadline:
                raise TimeoutError("cleanup helper final acknowledgment timed out")
            self.clock.sleep(self.spec.timing.poll)
        if not self.wait_exit(self.helper, deadline):
            raise TimeoutError("cleanup helper did not exit")
        return reply

    def wait_exit(self, peer, deadline):
        while not exited(peer.fd) and self.clock.monotonic() < deadline:
            self.pulse(watch=False)
            if self.runner is not None:
                try:
                    self.runner.check()
                    self.channel.check_heartbeat("runner", self.clock, self.spec.timing.stale)
                except RuntimeError as error:
                    self.error(str(error))
            self.clock.sleep(min(self.spec.timing.poll, max(0, deadline - self.clock.monotonic())))
        return exited(peer.fd)

    def terminate(self, peer, label, term_seconds, deadline, expected=False):
        if peer is None:
            return
        if exited(peer.fd):
            if not expected:
                self.error(f"{label} exited before shutdown")
            return
        peer.send(signal.SIGTERM)
        if not self.wait_exit(peer, min(deadline, self.clock.monotonic() + term_seconds)):
            peer.send(signal.SIGKILL)
            self.error(f"{label} required SIGKILL")
            if not self.wait_exit(peer, min(deadline, self.clock.monotonic() + self.spec.timing.kill_wait)):
                self.error(f"{label} exit was not confirmed")

    def shutdown(self, emergency=False):
        timing = self.spec.timing
        deadline = min(self.expiry, self.clock.monotonic() + timing.shutdown)
        if emergency:
            self.channel.write("failure.json", errors=self.errors, remoteCleanup="NOT_STARTED")
            # Open the last pre-exec gate even if failure preceded its acknowledgment.
            try:
                self.register_client()
            except (OSError, RuntimeError, ValueError) as error:
                self.error(f"register last client: {error}")
        self.terminate(self.client, "client", timing.client_term, deadline, expected=True)
        if emergency:
            self.terminate(self.runner, "runner", timing.client_term, deadline, expected=True)
        helper_report = None
        try:
            helper_deadline = min(deadline, self.clock.monotonic() + timing.client_term + timing.kill_wait)
            helper_report = self.stop_helper(helper_deadline, emergency)
        except (OSError, RuntimeError, ValueError, TimeoutError) as error:
            self.error(str(error))
            helper_report = self.stop_helper(deadline, True)
        self.terminate(self.broker, "PageBroker", timing.broker_term, deadline)
        report = {"runId": self.spec.run_id, "status": "ERROR" if self.errors else "SUCCESS",
                  "errors": self.errors, "broker": self.broker.record() if self.broker else None}
        if self.spec.mode == "s3":
            report["helper"] = helper_report
        write_json(Path(self.spec.results) / "supervisor.json", report)
        self.channel.write("shutdown-complete.json", **{key: value for key, value in report.items() if key != "runId"})
        if not emergency:
            try:
                while self.channel.read("completion-ack.json") is None:
                    self.pulse()
                    if self.clock.monotonic() >= deadline:
                        raise TimeoutError("runner did not acknowledge supervisor completion")
                    self.clock.sleep(timing.poll)
            except (OSError, RuntimeError, TimeoutError, InterruptedError) as error:
                self.error(str(error))
                self.channel.write("failure.json", errors=self.errors, remoteCleanup="NOT_STARTED")
                self.terminate(self.runner, "runner", timing.client_term, deadline, expected=True)
                report.update(status="ERROR", errors=self.errors)
                write_json(Path(self.spec.results) / "supervisor.json", report)
        return 1 if self.errors else 0

    def run(self):
        initialized = False
        try:
            self.startup, self.expiry = self.spec.deadlines(self.clock)
            Path(self.spec.control).mkdir(parents=True, exist_ok=True)
            Path(self.spec.results).mkdir(parents=True, exist_ok=True)
            with (Path(self.spec.control) / "supervisor-owner").open("x"):
                pass
            initialized = True
            self.channel.write("supervisor-registration.json", peer=own_record())
            while self.broker is None or self.runner is None:
                self.pulse()
                if self.clock.monotonic() >= self.startup:
                    raise TimeoutError("supervisor startup admission expired")
                registration = self.channel.read("runner-registration.json")
                if registration is not None and self.runner is None:
                    self.runner = Peer.open(registration["peer"])
                if self.broker is None:
                    try:
                        self.broker = Peer.discover(self.spec.socket, min(0.1, self.spec.timing.poll))
                    except TimeoutError:
                        pass
                self.clock.sleep(self.spec.timing.poll)
            registration = self.channel.read("runner-registration.json")
            if registration["broker"] != self.broker.record():
                raise RuntimeError("runner and supervisor registered different brokers")
            self.channel.write("registered.json", peer=own_record(), broker=self.broker.record())
            while True:
                self.pulse()
                self.register_client()
                self.register_helper()
                request = self.channel.read("shutdown.json")
                if request is not None:
                    if request["status"] not in ("SUCCESS", "ERROR", "INTERRUPTED"):
                        raise ValueError("invalid shutdown outcome")
                    return self.shutdown()
                self.broker.check()
                if self.clock.monotonic() >= self.expiry - self.spec.timing.shutdown:
                    raise TimeoutError("supervisor run deadline reached")
                self.clock.sleep(self.spec.timing.poll)
        except (Exception, KeyboardInterrupt) as error:
            self.error(str(error) or type(error).__name__)
            if initialized:
                return self.shutdown(emergency=True)
            return 1
        finally:
            for peer in (self.cleanup_worker, self.helper, self.client, self.runner, self.broker):
                if peer is not None:
                    peer.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--spec", type=Path, required=True)
    args = parser.parse_args()
    supervisor = Supervisor(RunSpec.load(args.spec))
    def interrupted(_signum, _frame):
        supervisor.interrupted = True
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    return supervisor.run()


if __name__ == "__main__":
    raise SystemExit(main())
