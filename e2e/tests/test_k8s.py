# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks for Kubernetes E2E helpers."""

import socket
from types import SimpleNamespace

import pytest
from kubernetes.client import ApiException
from kubernetes.stream import ws_client

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


def refused_exec(body: bytes) -> RuntimeError:
    """The websocket error for an exec handshake that is answered with a 500."""
    headers = {"content-type": "text/plain; charset=utf-8", "content-length": str(len(body))}
    return RuntimeError(
        f"Handshake status 500 Internal Server Error -+-+- {headers} -+-+- {body}"
    )


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
    assert k8s.exec_dial_failure(raised.value) == repr(EXEC_DIAL_FAILURE)


def test_exec_command_does_not_repeat_a_refused_exec(monkeypatch) -> None:
    # A container that has not started is refused in the handshake too, with
    # the same status. A second exec does not start it.
    handshakes, waits = answer_exec_handshakes(
        monkeypatch, refused_exec(b'container not found ("app")')
    )

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
    # that failure is the context of every error they raise.
    try:
        raise ApiException(status=0, reason=str(refused_exec(EXEC_DIAL_FAILURE)))
    except ApiException:
        with pytest.raises((AttributeError, ApiException)):
            k8s.exec_command("namespace", "pod", "true", container="app")

    assert len(execs) == 1


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
