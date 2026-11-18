# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that an install runs the images a candidate recorded."""

import json

import pytest
from kubernetes import client

from snapshot_e2e.infra import setup

OPERATOR = "sha256:" + "1" * 64
AGENT = "sha256:" + "2" * 64
PAGEBROKER = "sha256:" + "3" * 64
EXPECTED = {"operator": OPERATOR, "agent": AGENT, "pagebroker": PAGEBROKER}
REGISTRY = "ghcr.io/ai-dynamo/snapshot"
TAG = "v0.0.0-pull-request-1-gdeadbeef"


def status(name: str, image: str, digest: str) -> client.V1ContainerStatus:
    return client.V1ContainerStatus(
        name=name,
        image=f"{REGISTRY}/{image}:{TAG}",
        image_id=f"{REGISTRY}/{image}@{digest}",
        ready=True,
        restart_count=0,
    )


def pod(name: str, *statuses, init=()) -> client.V1Pod:
    return client.V1Pod(
        metadata=client.V1ObjectMeta(name=name),
        status=client.V1PodStatus(
            container_statuses=list(statuses),
            init_container_statuses=list(init),
        ),
    )


def running(operator=OPERATOR, agent=AGENT, pagebroker=PAGEBROKER) -> list[client.V1Pod]:
    return [
        pod(
            "snapshot-operator-0",
            status("manager", "operator", operator),
            init=[status("crd-installer", "operator", operator)],
        ),
        pod(
            "snapshot-agent-a",
            status("agent", "agent", agent),
            status("pagebroker", "pagebroker", pagebroker),
            init=[
                client.V1ContainerStatus(
                    name="deploy-seccomp",
                    image="busybox:1.37.0",
                    image_id="docker.io/library/busybox@sha256:" + "9" * 64,
                    ready=True,
                    restart_count=0,
                )
            ],
        ),
    ]


def test_matching_images_pass():
    assert setup.running_image_mismatches(running(), EXPECTED) == []


def test_a_stale_image_is_reported():
    stale = "sha256:" + "f" * 64
    problems = setup.running_image_mismatches(running(operator=stale), EXPECTED)
    assert problems == [
        f"snapshot-operator-0/crd-installer runs operator {stale}, expected {OPERATOR}",
        f"snapshot-operator-0/manager runs operator {stale}, expected {OPERATOR}",
    ]


def test_an_image_nothing_runs_is_reported():
    pods = [p for p in running() if p.metadata.name != "snapshot-agent-a"]
    assert setup.running_image_mismatches(pods, EXPECTED) == [
        "no running container uses the agent image",
        "no running container uses the pagebroker image",
    ]


def test_docker_pullable_image_ids_are_understood():
    pods = running()
    manager = pods[0].status.container_statuses[0]
    manager.image_id = f"docker-pullable://{REGISTRY}/operator@{OPERATOR}"
    assert setup.running_image_mismatches(pods, EXPECTED) == []


@pytest.mark.parametrize(
    ("image", "name"),
    [
        (f"{REGISTRY}/operator:{TAG}", "operator"),
        (f"{REGISTRY}/agent@{AGENT}", "agent"),
        (f"{REGISTRY}/pagebroker:{TAG}@{PAGEBROKER}", "pagebroker"),
        ("localhost:5000/agent:local", "agent"),
    ],
)
def test_image_name_drops_registry_tag_and_digest(image, name):
    assert setup.image_name(image) == name


def test_unset_digests_mean_no_check(monkeypatch):
    monkeypatch.delenv("SNAPSHOT_E2E_IMAGE_DIGESTS", raising=False)
    assert setup.expected_image_digests() == {}
    assert setup.snapshot_helm_overrides("direct", "", None) == []


def test_digests_force_a_fresh_pull_of_every_image(monkeypatch):
    monkeypatch.setenv("SNAPSHOT_E2E_IMAGE_DIGESTS", json.dumps(EXPECTED))
    monkeypatch.delenv("SNAPSHOT_E2E_HELM_SET", raising=False)
    assert setup.snapshot_helm_overrides("vcluster", "vc-ns", ["a=user"]) == [
        "daemonset.nriHostNamespace=vc-ns",
        "image.operator.pullPolicy=Always",
        "image.agent.pullPolicy=Always",
        "image.pageBroker.pullPolicy=Always",
        "a=user",
    ]


@pytest.mark.parametrize(
    ("raw", "match"),
    [
        ("not json", "not JSON"),
        ("[]", "non-empty JSON object"),
        ("{}", "non-empty JSON object"),
        (json.dumps({"busybox": OPERATOR}), "unknown image"),
        (json.dumps({"operator": "sha256:abc"}), "not a sha256 digest"),
        (json.dumps({"operator": OPERATOR.upper()}), "not a sha256 digest"),
    ],
)
def test_malformed_digests_are_rejected(monkeypatch, raw, match):
    monkeypatch.setenv("SNAPSHOT_E2E_IMAGE_DIGESTS", raw)
    with pytest.raises(setup.SetupError, match=match):
        setup.expected_image_digests()


def test_readiness_fails_when_a_stale_image_runs(monkeypatch):
    monkeypatch.setenv("SNAPSHOT_E2E_IMAGE_DIGESTS", json.dumps(EXPECTED))
    monkeypatch.setattr(setup.preflight, "load_config", lambda *args: None)
    monkeypatch.setattr(setup, "wait_for_snapshot_readiness", lambda **kwargs: None)
    stale = running(agent="sha256:" + "e" * 64)
    selectors = []

    def list_pods(self, namespace, label_selector):
        selectors.append(label_selector)
        return client.V1PodList(items=stale)

    monkeypatch.setattr(client.CoreV1Api, "list_namespaced_pod", list_pods)
    args = setup.parse_args(["--snapshot-tag", TAG, "--mode", "direct"])
    with pytest.raises(setup.SetupError, match="snapshot-agent-a/agent runs agent"):
        setup.setup_snapshot_readiness(args, setup.setup_context(args))
    assert selectors == [f"{setup.SNAPSHOT_LABEL},app.kubernetes.io/instance=snapshot"]
