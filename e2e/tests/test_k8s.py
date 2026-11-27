# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks for Kubernetes E2E helpers."""

import base64
import hashlib
import http.server
import socket
import socketserver
import threading
from types import SimpleNamespace

import pytest
from kubernetes.client import ApiException
from kubernetes.stream import ws_client
from websocket import WebSocketBadStatusException

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle


def test_restart_operator_waits_for_terminating_previous_pod(monkeypatch) -> None:
    deployment = SimpleNamespace(
        metadata=SimpleNamespace(name="snapshot-operator", generation=2),
        spec=SimpleNamespace(replicas=1),
        status=SimpleNamespace(
            observed_generation=2,
            updated_replicas=1,
            available_replicas=1,
            replicas=1,
        ),
    )
    previous = SimpleNamespace(metadata=SimpleNamespace(deletion_timestamp="terminating"))
    replacement = SimpleNamespace(metadata=SimpleNamespace(deletion_timestamp=None))
    pod_lists = [[previous, replacement], [replacement]]
    pod_list_calls = []

    apps = SimpleNamespace(
        list_namespaced_deployment=lambda *_, **__: SimpleNamespace(items=[deployment]),
        patch_namespaced_deployment=lambda *_, **__: None,
        read_namespaced_deployment=lambda *_, **__: deployment,
    )

    def list_operator_pods(*args, **kwargs):
        pod_list_calls.append((args, kwargs))
        return SimpleNamespace(items=pod_lists.pop(0))

    core = SimpleNamespace(list_namespaced_pod=list_operator_pods)
    monkeypatch.setattr(k8s.client, "AppsV1Api", lambda: apps)
    monkeypatch.setattr(k8s.client, "CoreV1Api", lambda: core)
    monkeypatch.setattr(k8s.time, "monotonic", lambda: 0)
    monkeypatch.setattr(k8s.time, "sleep", lambda _: None)

    callbacks = []
    k8s.restart_snapshot_operator(
        "snapshot-system",
        "snapshot",
        after_rollout=lambda: callbacks.append(len(pod_list_calls)),
    )

    assert len(pod_list_calls) == 2
    assert callbacks == [2]


def test_wait_for_snapshotjob_reconcile_ack_matches_event_token(monkeypatch) -> None:
    events = [
        SimpleNamespace(message="reconcile acknowledgment old-token"),
        SimpleNamespace(message="reconcile acknowledgment expected-token"),
    ]
    monkeypatch.setattr(k8s, "list_events", lambda *_args, **_kwargs: events)
    monkeypatch.setattr(lifecycle, "time", SimpleNamespace(monotonic=lambda: 0, sleep=lambda _: None))

    lifecycle.wait_for_snapshotjob_reconcile_ack(
        "snapshot-e2e",
        "job",
        token="expected-token",
        timeout=1,
    )


def test_acknowledge_snapshotjob_reconcile_patches_and_waits(monkeypatch) -> None:
    patches = []
    waited = []

    class CustomObjects:
        def patch_namespaced_custom_object(self, *args):
            patches.append(args)

    monkeypatch.setattr(lifecycle.client, "CustomObjectsApi", lambda: CustomObjects())
    monkeypatch.setattr(lifecycle.uuid, "uuid4", lambda: SimpleNamespace(hex="test-token"))
    monkeypatch.setattr(
        lifecycle,
        "wait_for_snapshotjob_reconcile_ack",
        lambda namespace, name, *, token, timeout: waited.append(
            (namespace, name, token, timeout)
        ),
    )

    token = lifecycle.acknowledge_snapshotjob_reconcile(
        "snapshot-e2e", "job", timeout=7
    )

    assert token == "test-token"
    assert patches == [
        (
            lifecycle.GROUP,
            lifecycle.VERSION,
            "snapshot-e2e",
            lifecycle.SNAPSHOTJOBS,
            "job",
            {
                "metadata": {
                    "annotations": {
                        "snapshot-e2e.nvidia.com/reconcile-ack": "test-token"
                    }
                }
            },
        )
    ]
    assert waited == [("snapshot-e2e", "job", "test-token", 7)]


# What the API server answers when it cannot open its connection towards the
# node of the Pod.
EXEC_DIAL_FAILURE = (
    b"error dialing backend: read tcp 192.0.2.10:50000->192.0.2.20:50001: "
    b"read: connection reset by peer\n"
)


def refused_exec(body: bytes, status: int = 500) -> WebSocketBadStatusException:
    """The websocket error for an exec handshake that is answered with a refusal."""
    return WebSocketBadStatusException(f"Handshake status {status}", status, resp_body=body)


class StartedExec:
    """The websocket of an exec whose handshake the API server accepted.

    It carries one frame of stdout. Then it closes, or it fails with error.
    """

    connected = True
    subprotocol = "v4.channel.k8s.io"

    def __init__(self, stdout: bytes, error: Exception | None = None) -> None:
        # The client polls this socket before it reads each frame.
        self.sock, self._peer = socket.socketpair()
        self._peer.send(b"x")
        self._stdout = bytes([ws_client.STDOUT_CHANNEL]) + stdout
        self._error = error

    def recv_data_frame(self, _control_frame):
        if self._stdout:
            data, self._stdout = self._stdout, b""
            return ws_client.ABNF.OPCODE_BINARY, SimpleNamespace(data=data)
        self.sock.close()
        self._peer.close()
        if self._error:
            raise self._error
        return ws_client.ABNF.OPCODE_CLOSE, SimpleNamespace(data=b"")


def answer_exec_handshakes(monkeypatch, *answers):
    """Give each exec handshake of the real client the next answer.

    Returns the handshakes made and the waits between them.
    """
    handshakes = []
    waits = []

    def create_websocket(_configuration, url, _headers):
        handshakes.append(url)
        answer = answers[len(handshakes) - 1]
        if isinstance(answer, Exception):
            raise answer
        return answer

    monkeypatch.setattr(ws_client, "create_websocket", create_websocket)
    monkeypatch.setattr(k8s.time, "sleep", waits.append)
    return handshakes, waits


def test_exec_command_repeats_an_exec_that_did_not_reach_the_node(monkeypatch) -> None:
    handshakes, waits = answer_exec_handshakes(
        monkeypatch, refused_exec(EXEC_DIAL_FAILURE), StartedExec(b"ran\n")
    )

    assert k8s.exec_command("namespace", "pod", "true", container="app") == "ran\n"
    assert len(handshakes) == 2
    assert waits == [k8s.EXEC_DIAL_RETRY_SECONDS]


def test_exec_command_gives_up_on_a_node_it_cannot_reach(monkeypatch) -> None:
    handshakes, waits = answer_exec_handshakes(
        monkeypatch, *[refused_exec(EXEC_DIAL_FAILURE)] * (k8s.EXEC_DIAL_ATTEMPTS + 1)
    )

    # The client raises a failed exec as an ApiException, or as the
    # AttributeError that it hits while it handles that exception.
    with pytest.raises((AttributeError, ApiException)) as raised:
        k8s.exec_command("namespace", "pod", "true", container="app")

    assert len(handshakes) == k8s.EXEC_DIAL_ATTEMPTS
    assert waits == [k8s.EXEC_DIAL_RETRY_SECONDS] * (k8s.EXEC_DIAL_ATTEMPTS - 1)
    # The failure of the last attempt is raised as it is.
    assert k8s.exec_dial_failure(raised.value) == EXEC_DIAL_FAILURE.decode().strip()


@pytest.mark.parametrize(
    ("status", "body"),
    [
        # A container that has not started is refused in the handshake too,
        # with the same status. A second exec does not start it.
        pytest.param(500, b'container not found ("app")', id="container-not-started"),
        # The answer when the connection is reset after the request was sent.
        # The command may have started.
        pytest.param(
            500,
            b"read tcp 192.0.2.10:50000->192.0.2.20:50001: read: connection reset by peer\n",
            id="reset-after-the-request",
        ),
        pytest.param(500, b"upgrade failed: " + EXEC_DIAL_FAILURE, id="dial-error-not-first"),
        # The API server answers a dial that failed with 500. Another status
        # shows another sender, which may have passed the request on.
        pytest.param(503, EXEC_DIAL_FAILURE, id="another-status"),
    ],
)
def test_exec_command_does_not_repeat_a_refused_exec(monkeypatch, status, body) -> None:
    handshakes, waits = answer_exec_handshakes(monkeypatch, refused_exec(body, status))

    with pytest.raises((AttributeError, ApiException)):
        k8s.exec_command("namespace", "pod", "true", container="app")

    assert len(handshakes) == 1
    assert waits == []


def test_exec_command_does_not_repeat_an_exec_that_started(monkeypatch) -> None:
    # The handshake was accepted, so the command ran before the connection broke.
    handshakes, waits = answer_exec_handshakes(
        monkeypatch,
        StartedExec(b"started\n", ConnectionResetError(104, "Connection reset by peer")),
    )

    with pytest.raises((AttributeError, ApiException)):
        k8s.exec_command("namespace", "pod", "touch /tmp/gate", container="app")

    assert len(handshakes) == 1
    assert waits == []


def test_exec_command_does_not_repeat_an_exec_for_an_earlier_failure(monkeypatch) -> None:
    execs = []

    def fail_after_the_command(*_args, **_kwargs):
        execs.append(1)
        raise AttributeError("not the failed handshake of this exec")

    monkeypatch.setattr(k8s, "stream", fail_after_the_command)
    monkeypatch.setattr(k8s.time, "sleep", lambda _: None)

    # A debug dump runs its execs while the failure of the test is handled, so
    # that failure is the context of every error they raise. Here it is a dial
    # failure as the client raises one, with the websocket error as its context.
    earlier = ApiException(status=0)
    earlier.__context__ = refused_exec(EXEC_DIAL_FAILURE)
    try:
        raise earlier
    except ApiException:
        with pytest.raises((AttributeError, ApiException)):
            k8s.exec_command("namespace", "pod", "true", container="app")

    assert len(execs) == 1


class ExecAnswers(http.server.BaseHTTPRequestHandler):
    """Answers each exec handshake on a local socket with the next answer.

    An answer is a status and bytes. The bytes are the body of a refusal, sent
    the way Go's http.Error sends one, or with 101 the stdout of an exec that
    runs.
    """

    protocol_version = "HTTP/1.1"

    def do_GET(self) -> None:
        self.server.handshakes.append(self.path)
        status, data = self.server.answers.pop(0)
        self.close_connection = True
        self.send_response(status)
        if status != 101:
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        key = self.headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
        accept = base64.b64encode(hashlib.sha1(key.encode()).digest()).decode()
        self.send_header("Upgrade", "websocket")
        self.send_header("Connection", "Upgrade")
        self.send_header("Sec-WebSocket-Accept", accept)
        self.send_header("Sec-WebSocket-Protocol", StartedExec.subprotocol)
        self.end_headers()
        # One binary frame with the stdout of the command, then a close frame.
        stdout = bytes([ws_client.STDOUT_CHANNEL]) + data
        self.wfile.write(bytes([0x82, len(stdout)]) + stdout + bytes([0x88, 0]))

    def log_message(self, *_args) -> None:
        pass


@pytest.fixture
def exec_server(monkeypatch):
    """A local server that the installed client makes its exec handshakes with.

    The tests above raise the websocket error by hand. Here the installed
    kubernetes and websocket-client read an answer from a socket and raise the
    error themselves, so a release that raises it in another way fails a test.
    """
    server = socketserver.TCPServer(("127.0.0.1", 0), ExecAnswers)
    server.answers, server.handshakes, server.waits = [], [], []
    # The client must not take a proxy from the environment to reach it.
    for name in ("http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY"):
        monkeypatch.delenv(name, raising=False)
    host = f"http://127.0.0.1:{server.server_address[1]}"
    api = k8s.client.CoreV1Api(k8s.client.ApiClient(k8s.client.Configuration(host=host)))
    monkeypatch.setattr(k8s.client, "CoreV1Api", lambda: api)
    monkeypatch.setattr(k8s.time, "sleep", server.waits.append)
    thread = threading.Thread(
        target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True
    )
    thread.start()
    yield server
    server.shutdown()
    thread.join()
    server.server_close()


def test_exec_command_repeats_a_dial_error_that_the_installed_client_reads(exec_server) -> None:
    exec_server.answers = [(500, EXEC_DIAL_FAILURE), (101, b"ran\n")]

    assert k8s.exec_command("namespace", "pod", "true", container="app") == "ran\n"
    assert len(exec_server.handshakes) == 2
    assert exec_server.waits == [k8s.EXEC_DIAL_RETRY_SECONDS]


def test_snapshot_operator_hook_uses_configured_input(monkeypatch) -> None:
    deployment = SimpleNamespace(
        spec=SimpleNamespace(
            template=SimpleNamespace(
                spec=SimpleNamespace(
                    containers=[
                        SimpleNamespace(
                            env=[
                                SimpleNamespace(
                                    name="SNAPSHOT_E2E_TEST_FAIL_CAPTURE_SOURCE_ANNOTATION",
                                    value="snapshot-e2e.nvidia.com/fail-capture",
                                )
                            ]
                        )
                    ]
                )
            )
        )
    )
    apps = SimpleNamespace(
        list_namespaced_deployment=lambda *_, **__: SimpleNamespace(items=[deployment])
    )
    monkeypatch.setattr(k8s.client, "AppsV1Api", lambda: apps)
    assert k8s.snapshot_operator_test_hook_enabled("snapshot-e2e", "snapshot")


def test_file_present_executes_in_requested_container(monkeypatch) -> None:
    calls = []

    def exec_command(namespace, pod, command, *, container=None):
        calls.append((namespace, pod, command, container))
        return "__snapshot_e2e_file_present__"

    monkeypatch.setattr(k8s, "exec_command", exec_command)

    assert lifecycle.file_present(
        "snapshot-e2e",
        "pod",
        "/e2e-helper-sync/helper-finished",
        container="main",
    )
    assert calls == [
        (
            "snapshot-e2e",
            "pod",
            "[[ -f /e2e-helper-sync/helper-finished ]] && printf '%s' "
            "__snapshot_e2e_file_present__",
            "main",
        )
    ]


def test_wait_for_container_running_accepts_running_without_ready(monkeypatch) -> None:
    pod = SimpleNamespace(
        status=SimpleNamespace(
            phase="Running",
            container_statuses=[
                SimpleNamespace(
                    name="helper",
                    ready=False,
                    state=SimpleNamespace(running=SimpleNamespace(), terminated=None),
                )
            ],
        )
    )
    monkeypatch.setattr(k8s, "read_pod", lambda *_args, **_kwargs: pod)
    monkeypatch.setattr(
        lifecycle,
        "wait_for",
        lambda _description, check, _timeout, **_kwargs: check(),
    )

    assert lifecycle.wait_for_container_running(
        "namespace", "pod", container="helper", timeout=1
    ) is pod


@pytest.mark.parametrize(
    ("phase", "state"),
    [
        ("Failed", SimpleNamespace(running=None, terminated=None)),
        ("Running", SimpleNamespace(running=None, terminated=SimpleNamespace(exit_code=1))),
    ],
)
def test_wait_for_container_running_rejects_terminal_or_terminated(
    monkeypatch, phase, state
) -> None:
    pod = SimpleNamespace(
        status=SimpleNamespace(
            phase=phase,
            container_statuses=[
                SimpleNamespace(name="main", ready=False, state=state),
            ],
        )
    )
    monkeypatch.setattr(k8s, "read_pod", lambda *_args, **_kwargs: pod)
    monkeypatch.setattr(
        lifecycle,
        "wait_for",
        lambda _description, check, _timeout, **_kwargs: check(),
    )

    with pytest.raises(AssertionError):
        lifecycle.wait_for_container_running("namespace", "pod", timeout=1)
