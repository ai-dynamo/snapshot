# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that a readiness timeout says why Snapshot Pods did not start."""

import time
from types import SimpleNamespace

import pytest
from kubernetes import client
from kubernetes.client import ApiException

from snapshot_e2e.infra import setup

NAMESPACE = "snapshot-e2e"
SELECTOR = "app.kubernetes.io/name=snapshot"
UNSCHEDULED = (
    "0/6 nodes are available: 1 Insufficient cpu, "
    "5 node(s) didn't match Pod's node affinity/selector."
)


@pytest.fixture(autouse=True)
def fake_clock(monkeypatch):
    clock = SimpleNamespace(now=0.0)

    def sleep(seconds):
        clock.now += seconds

    monkeypatch.setattr(
        setup,
        "time",
        SimpleNamespace(
            monotonic=lambda: clock.now, sleep=sleep, strftime=time.strftime
        ),
    )


def pod(name: str, *, ready: bool) -> client.V1Pod:
    statuses = None
    if ready:
        statuses = [
            client.V1ContainerStatus(
                name="agent", ready=True, image="agent", image_id="", restart_count=0
            )
        ]
    return client.V1Pod(
        metadata=client.V1ObjectMeta(name=name),
        spec=client.V1PodSpec(containers=[], node_name="node-a" if ready else None),
        status=client.V1PodStatus(
            phase="Running" if ready else "Pending", container_statuses=statuses
        ),
    )


def event(reason: str, message: str, count: int = 1) -> client.CoreV1Event:
    return client.CoreV1Event(
        metadata=client.V1ObjectMeta(name=f"{reason}.1"),
        involved_object=client.V1ObjectReference(kind="Pod"),
        reason=reason,
        message=message,
        count=count,
        type="Warning",
    )


def agent_daemonset() -> client.V1DaemonSet:
    return client.V1DaemonSet(
        metadata=client.V1ObjectMeta(name="snapshot-agent", generation=1),
        status=client.V1DaemonSetStatus(
            observed_generation=1,
            desired_number_scheduled=2,
            current_number_scheduled=2,
            updated_number_scheduled=2,
            number_misscheduled=0,
            number_ready=1,
        ),
    )


def serve_pods(monkeypatch, pods):
    monkeypatch.setattr(
        client.CoreV1Api,
        "list_namespaced_pod",
        lambda self, namespace, label_selector: client.V1PodList(items=pods),
    )


def serve_events(monkeypatch, events_by_pod):
    looked_up = []

    def list_events(namespace, *, field_selector=None):
        assert namespace == NAMESPACE
        assert field_selector["type"] == "Warning"
        name = field_selector["involvedObject.name"]
        looked_up.append(name)
        result = events_by_pod[name]
        if isinstance(result, Exception):
            raise result
        return result

    monkeypatch.setattr(setup.k8s, "list_events", list_events)
    return looked_up


def wait_for_agent():
    setup.wait_for_daemonset_rollout_by_selector(
        namespace=NAMESPACE,
        label_selector=SELECTOR,
        timeout_seconds=10,
        description="Snapshot agent",
    )


def test_agent_timeout_names_the_scheduler_reason(monkeypatch, capsys):
    monkeypatch.setattr(
        client.AppsV1Api,
        "list_namespaced_daemon_set",
        lambda self, namespace, label_selector: client.V1DaemonSetList(
            items=[agent_daemonset()]
        ),
    )
    serve_pods(monkeypatch, [pod("snapshot-agent-a", ready=True), pod("snapshot-agent-b", ready=False)])
    looked_up = serve_events(
        monkeypatch, {"snapshot-agent-b": [event("FailedScheduling", UNSCHEDULED, count=7)]}
    )

    with pytest.raises(setup.SetupError) as raised:
        wait_for_agent()

    assert str(raised.value).startswith("timed out waiting for Snapshot agent daemonset")
    assert str(raised.value).endswith(f"; scheduler: {UNSCHEDULED}")
    assert looked_up == ["snapshot-agent-b"]
    assert f"snapshot-agent-b: FailedScheduling (x7): {UNSCHEDULED}" in capsys.readouterr().out


def test_operator_timeout_names_the_scheduler_reason(monkeypatch):
    serve_pods(monkeypatch, [pod("snapshot-operator-0", ready=False)])
    serve_events(
        monkeypatch,
        {
            "snapshot-operator-0": [
                event("BackOff", "Back-off pulling image"),
                event("FailedScheduling", UNSCHEDULED),
            ]
        },
    )

    with pytest.raises(setup.SetupError, match="scheduler: 0/6 nodes are available"):
        setup.wait_for_pods_ready(
            namespace=NAMESPACE,
            label_selector=SELECTOR,
            timeout_seconds=10,
            description="Snapshot operator",
        )


def test_unscheduled_reason_is_absent_without_a_scheduling_event(monkeypatch):
    monkeypatch.setattr(
        client.AppsV1Api,
        "list_namespaced_daemon_set",
        lambda self, namespace, label_selector: client.V1DaemonSetList(
            items=[agent_daemonset()]
        ),
    )
    serve_pods(monkeypatch, [pod("snapshot-agent-b", ready=False)])
    serve_events(monkeypatch, {"snapshot-agent-b": [event("BackOff", "Back-off pulling image")]})

    with pytest.raises(setup.SetupError) as raised:
        wait_for_agent()

    assert "scheduler:" not in str(raised.value)


def test_an_event_listing_error_does_not_hide_the_timeout(monkeypatch, capsys):
    monkeypatch.setattr(
        client.AppsV1Api,
        "list_namespaced_daemon_set",
        lambda self, namespace, label_selector: client.V1DaemonSetList(
            items=[agent_daemonset()]
        ),
    )
    serve_pods(monkeypatch, [pod("snapshot-agent-b", ready=False)])
    serve_events(
        monkeypatch, {"snapshot-agent-b": ApiException(status=403, reason="Forbidden")}
    )

    with pytest.raises(setup.SetupError, match="timed out waiting for Snapshot agent daemonset"):
        wait_for_agent()

    assert "snapshot-agent-b: could not list events: 403 Forbidden" in capsys.readouterr().out
