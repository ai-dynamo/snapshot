# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that an install runs the images a candidate recorded."""

import hashlib
import io
import json
import urllib.error

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


MANIFEST = "application/vnd.oci.image.manifest.v1+json"
INDEX = "application/vnd.oci.image.index.v1+json"


@pytest.fixture
def registry(monkeypatch):
    blobs = {}
    calls = []

    def register(body):
        raw = json.dumps(body).encode()
        digest = "sha256:" + hashlib.sha256(raw).hexdigest()
        blobs[digest] = raw
        return digest

    def fetch(request, timeout):
        url = request if isinstance(request, str) else request.full_url
        calls.append(url)
        if "/token?" in url:
            return io.BytesIO(b'{"token":"test-token"}')
        digest = url.rsplit("/", 1)[-1]
        return io.BytesIO(blobs[digest])

    monkeypatch.setattr(setup.urllib.request, "urlopen", fetch)
    monkeypatch.setattr(setup.k8s, "read_node", lambda name: client.V1Node(
        metadata=client.V1ObjectMeta(labels={"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"})
    ))
    return register, blobs, calls


def descriptor(digest, arch="amd64", **extra):
    return {"mediaType": MANIFEST, "digest": digest,
            "platform": {"os": "linux", "architecture": arch}, **extra}


def verify_pagebroker(monkeypatch, recorded, actual):
    pods = running(pagebroker=actual)
    for item in pods:
        item.spec = client.V1PodSpec(containers=[], node_name="worker")
    monkeypatch.setattr(client.CoreV1Api, "list_namespaced_pod", lambda *args, **kwargs: client.V1PodList(items=pods))
    setup.verify_running_images(namespace="test", release="snapshot", expected={**EXPECTED, "pagebroker": recorded})


def test_different_provenance_indexes_with_the_same_platform_image_pass(monkeypatch, registry):
    register, _, calls = registry
    image = register({"mediaType": MANIFEST, "layers": [{"digest": AGENT}]})
    # Even an attestation claiming the node's platform is not executable content.
    attestation = descriptor(OPERATOR, annotations={"vnd.docker.reference.type": "attestation-manifest"})
    old = register({"mediaType": INDEX, "manifests": [descriptor(image), attestation]})
    new = register({"mediaType": INDEX, "manifests": [descriptor(image), {**attestation, "digest": PAGEBROKER}, descriptor(AGENT, "arm64")]})
    verify_pagebroker(monkeypatch, new, old)
    assert len([url for url in calls if "/token?" in url]) == 1
    assert len([url for url in calls if url.endswith(image)]) == 1
    assert not any(url.endswith(OPERATOR) or url.endswith(AGENT) for url in calls)


def test_different_runnable_content_still_fails(monkeypatch, registry):
    register, _, _ = registry
    first = register({"mediaType": MANIFEST, "layers": [{"digest": OPERATOR}]})
    second = register({"mediaType": MANIFEST, "layers": [{"digest": AGENT}]})
    old = register({"mediaType": INDEX, "manifests": [descriptor(first)]})
    new = register({"mediaType": INDEX, "manifests": [descriptor(second)]})
    with pytest.raises(setup.SetupError, match="runs pagebroker"):
        verify_pagebroker(monkeypatch, new, old)


@pytest.mark.parametrize("entries", [[], [descriptor(AGENT, "arm64")], [descriptor(AGENT), descriptor(PAGEBROKER)], "invalid"])
def test_missing_ambiguous_or_malformed_platforms_fail(monkeypatch, registry, entries):
    register, _, _ = registry
    index = register({"mediaType": INDEX, "manifests": entries})
    with pytest.raises(setup.SetupError, match="could not verify platform image"):
        verify_pagebroker(monkeypatch, OPERATOR, index)


def test_registry_errors_fail_closed(monkeypatch, registry):
    def unavailable(*args, **kwargs):
        raise urllib.error.URLError("registry unavailable")
    monkeypatch.setattr(setup.urllib.request, "urlopen", unavailable)
    with pytest.raises(setup.SetupError, match="registry unavailable"):
        verify_pagebroker(monkeypatch, PAGEBROKER, AGENT)


def test_manifest_bytes_must_match_the_requested_digest(monkeypatch, registry):
    register, blobs, _ = registry
    image = register({"mediaType": MANIFEST})
    blobs[image] = b'{}'
    with pytest.raises(setup.SetupError, match="bytes do not match"):
        verify_pagebroker(monkeypatch, PAGEBROKER, image)


def test_matching_digests_do_not_contact_the_registry(monkeypatch, registry):
    _, _, calls = registry
    verify_pagebroker(monkeypatch, PAGEBROKER, PAGEBROKER)
    assert calls == []


def test_malformed_runtime_digest_fails_before_registry_access(monkeypatch, registry):
    _, _, calls = registry
    with pytest.raises(setup.SetupError, match="invalid manifest digest"):
        verify_pagebroker(monkeypatch, PAGEBROKER, "sha256:invalid")
    assert calls == []


def test_missing_node_platform_fails_before_registry_access(monkeypatch, registry):
    _, _, calls = registry
    monkeypatch.setattr(setup.k8s, "read_node", lambda name: client.V1Node())
    with pytest.raises(setup.SetupError, match="no OS/architecture labels"):
        verify_pagebroker(monkeypatch, PAGEBROKER, AGENT)
    assert calls == []


def test_missing_pod_assignment_is_a_setup_error(monkeypatch, registry):
    _, _, calls = registry
    pods = running(pagebroker=AGENT)
    monkeypatch.setattr(client.CoreV1Api, "list_namespaced_pod", lambda *args, **kwargs: client.V1PodList(items=pods))
    with pytest.raises(setup.SetupError, match="has no assigned node"):
        setup.verify_running_images(namespace="test", release="snapshot", expected=EXPECTED)
    assert calls == []
