# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks for Kubernetes E2E helpers."""

from types import SimpleNamespace
from unittest.mock import Mock

import pytest
import urllib3

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle


def test_pod_logs_decodes_raw_bytes_and_preserves_lines(monkeypatch):
    payload = b'COLD_BENCH {"stage":"process_start"}\nCOLD_BENCH {"stage":"engine_init_end"}\n'
    response = urllib3.HTTPResponse(body=payload, status=200, headers={"Content-Type": "text/plain"})
    release = Mock(wraps=response.release_conn)
    monkeypatch.setattr(response, "release_conn", release)
    api = k8s.client.CoreV1Api()
    request = Mock(return_value=response)
    monkeypatch.setattr(api.api_client.rest_client, "GET", request)
    monkeypatch.setattr(k8s.client, "CoreV1Api", lambda: api)

    text = k8s.pod_logs("workloads", "source", tail_lines=None, container="main")

    assert text == payload.decode("utf-8")
    assert len(text.splitlines()) == 2
    assert request.call_args.kwargs["_preload_content"] is False
    release.assert_called_once()


def test_pod_logs_releases_connection_when_read_fails(monkeypatch):
    class Response:
        release_conn = Mock()

        @property
        def data(self):
            raise OSError("interrupted log read")

    response = Response()
    api = Mock()
    api.read_namespaced_pod_log.return_value = response
    monkeypatch.setattr(k8s.client, "CoreV1Api", lambda: api)
    with pytest.raises(OSError, match="interrupted log read"):
        k8s.pod_logs("workloads", "source")
    response.release_conn.assert_called_once()


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
