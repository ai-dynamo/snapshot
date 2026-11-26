# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that an install runs the images a candidate recorded."""

import hashlib
import http.client
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
        spec=client.V1PodSpec(
            containers=[client.V1Container(name=item.name, image=item.image) for item in statuses],
            init_containers=[client.V1Container(name=item.name, image=item.image) for item in init],
        ),
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


# Every install turns retry on, so it is in each expected override list below.
RETRY_LIMIT = f"config.checkpoint.defaultRetryLimit={setup.E2E_CHECKPOINT_RETRY_LIMIT}"


def test_unset_digests_mean_no_check(monkeypatch):
    monkeypatch.delenv("SNAPSHOT_E2E_IMAGE_DIGESTS", raising=False)
    assert setup.expected_image_digests() == {}
    assert setup.snapshot_helm_overrides("direct", "", None) == [RETRY_LIMIT]


def test_digests_force_a_fresh_pull_of_every_image(monkeypatch):
    monkeypatch.setenv("SNAPSHOT_E2E_IMAGE_DIGESTS", json.dumps(EXPECTED))
    monkeypatch.delenv("SNAPSHOT_E2E_HELM_SET", raising=False)
    assert setup.snapshot_helm_overrides("vcluster", "vc-ns", ["a=user"]) == [
        "daemonset.nriHostNamespace=vc-ns",
        "image.operator.pullPolicy=Always",
        "image.agent.pullPolicy=Always",
        "image.pageBroker.pullPolicy=Always",
        RETRY_LIMIT,
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
    monkeypatch.setattr(setup.time, "sleep", lambda seconds: None)
    monkeypatch.setattr(setup.k8s, "read_node", lambda name: client.V1Node(
        metadata=client.V1ObjectMeta(labels={"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"})
    ))
    return register, blobs, calls


def descriptor(digest, arch="amd64", **extra):
    return {"mediaType": MANIFEST, "digest": digest,
            "platform": {"os": "linux", "architecture": arch}, **extra}


def verify_pagebroker(monkeypatch, recorded, actual, *, status_image=None, runtime_reference=None):
    pods = running(pagebroker=actual)
    for item in pods:
        item.spec.node_name = "worker"
    pagebroker = pods[-1].status.container_statuses[-1]
    if status_image is not None:
        pagebroker.image = status_image
    if runtime_reference is not None:
        pagebroker.image_id = runtime_reference
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


@pytest.mark.parametrize("mirrored", [False, True])
def test_different_runnable_content_still_fails(monkeypatch, registry, mirrored):
    register, _, _ = registry
    first = register({"mediaType": MANIFEST, "layers": [{"digest": OPERATOR}]})
    second = register({"mediaType": MANIFEST, "layers": [{"digest": AGENT}]})
    old = register({"mediaType": INDEX, "manifests": [descriptor(first)]})
    new = register({"mediaType": INDEX, "manifests": [descriptor(second)]})
    with pytest.raises(setup.SetupError, match="runs pagebroker"):
        verify_pagebroker(
            monkeypatch, new, old,
            status_image="dynamoci.azurecr.io/stale/other-image:old" if mirrored else None,
            runtime_reference=f"dynamoci.azurecr.io/mirror/pagebroker@{old}" if mirrored else None,
        )


@pytest.mark.parametrize("runtime_kind", ["mirror", "bare", "docker-pullable"])
def test_mirrored_status_names_resolve_content_from_the_desired_repository(monkeypatch, registry, equivalent_indexes, runtime_kind):
    _, _, calls = registry
    new, old = equivalent_indexes
    aliases = {
        "mirror": f"dynamoci.azurecr.io/ghcr.io/ai-dynamo/snapshot/pagebroker@{old}",
        "bare": old,
        "docker-pullable": f"docker-pullable://dynamoci.azurecr.io/mirror/pagebroker@{old}",
    }
    verify_pagebroker(
        monkeypatch, new, old,
        status_image="dynamoci.azurecr.io/stale/other-image:v0.0.0-g2e263f0e",
        runtime_reference=aliases[runtime_kind],
    )
    requests = [url for url in calls if "/manifests/" in url]
    assert requests
    assert all(url.startswith("https://ghcr.io/v2/ai-dynamo/snapshot/pagebroker/manifests/") for url in requests)


@pytest.mark.parametrize("group", ["containers", "init_containers"])
@pytest.mark.parametrize("ambiguous", [False, True])
def test_missing_or_ambiguous_spec_matches_fail_closed(group, ambiguous):
    pods = running()
    containers = getattr(pods[0].spec, group)
    target = containers[0]
    setattr(pods[0].spec, group, [target, target] if ambiguous else [])
    problems = setup.running_image_mismatches(pods, EXPECTED)
    assert any(f"/{target.name} needs one matching container image in the Pod spec" in problem for problem in problems)


def test_missing_pod_spec_fails_closed():
    pods = running()
    pods[0].spec = None
    assert any("has no Pod spec" in problem for problem in setup.running_image_mismatches(pods, EXPECTED))


@pytest.mark.parametrize("entries", [[], [descriptor(AGENT, "arm64")], [descriptor(AGENT), descriptor(PAGEBROKER)], "invalid"])
def test_missing_ambiguous_or_malformed_platforms_fail(monkeypatch, registry, entries):
    register, _, _ = registry
    index = register({"mediaType": INDEX, "manifests": entries})
    with pytest.raises(setup.SetupError, match="could not verify platform image"):
        verify_pagebroker(monkeypatch, OPERATOR, index)


@pytest.mark.parametrize(
    ("raw", "valid_digest", "message"),
    [(b"{}", False, "bytes do not match"),
     (b"not JSON", True, "image identity lookup failed")],
)
def test_invalid_manifest_content_is_not_retried(monkeypatch, registry, raw, valid_digest, message):
    _, blobs, calls = registry
    sleeps = []
    monkeypatch.setattr(setup.time, "sleep", sleeps.append)
    digest = "sha256:" + hashlib.sha256(raw).hexdigest() if valid_digest else OPERATOR
    blobs[digest] = raw
    with pytest.raises(setup.SetupError, match=message):
        verify_pagebroker(monkeypatch, PAGEBROKER, digest)
    assert len([url for url in calls if "/manifests/" in url]) == 1
    assert sleeps == []


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


@pytest.fixture
def equivalent_indexes(registry):
    register, _, _ = registry
    image = register({"mediaType": MANIFEST, "layers": [{"digest": AGENT}]})
    old = register({"mediaType": INDEX, "manifests": [descriptor(image)], "annotations": {"build": "old"}})
    new = register({"mediaType": INDEX, "manifests": [descriptor(image)], "annotations": {"build": "new"}})
    return new, old


@pytest.mark.parametrize("endpoint", ["token", "manifest"])
@pytest.mark.parametrize(
    "failure",
    [TimeoutError("opening timed out"), ConnectionError("connection reset"),
     urllib.error.URLError("temporary failure"), "read-timeout", "truncated-body", 429, 503, 401, 404],
)
def test_registry_read_retries_only_transient_failures(monkeypatch, equivalent_indexes, endpoint, failure):
    fetch = setup.urllib.request.urlopen
    attempts = []
    sleeps = []

    class FailedReadResponse(io.BytesIO):
        def read(self, *args, **kwargs):
            if failure == "truncated-body":
                raise http.client.IncompleteRead(b"partial", 10)
            raise TimeoutError("response read timed out")

    def flaky(request, timeout):
        url = request if isinstance(request, str) else request.full_url
        selected = ("/token?" in url) if endpoint == "token" else ("/manifests/" in url)
        if selected:
            attempts.append(url)
            assert timeout == 30
            if len(attempts) == 1:
                if failure in ("read-timeout", "truncated-body"):
                    return FailedReadResponse()
                if isinstance(failure, int):
                    raise urllib.error.HTTPError(url, failure, "registry failure", {}, None)
                raise failure
        return fetch(request, timeout)

    monkeypatch.setattr(setup.urllib.request, "urlopen", flaky)
    monkeypatch.setattr(setup.time, "sleep", sleeps.append)
    if failure in (401, 404):
        with pytest.raises(setup.SetupError, match=f"HTTP Error {failure}"):
            verify_pagebroker(monkeypatch, *equivalent_indexes)
        assert len(attempts) == 1
        assert sleeps == []
    else:
        verify_pagebroker(monkeypatch, *equivalent_indexes)
        assert attempts.count(attempts[0]) == 2
        assert sleeps == [1]


@pytest.mark.parametrize("endpoint", ["token", "manifest"])
@pytest.mark.parametrize("truncated", [False, True])
def test_exhausted_registry_retries_are_cached_across_pods(monkeypatch, equivalent_indexes, endpoint, truncated):
    recorded, actual = equivalent_indexes
    fetch = setup.urllib.request.urlopen
    attempts = []
    sleeps = []
    pods = running(pagebroker=actual) + [running(pagebroker=actual)[-1]]
    pods[-1].metadata.name = "snapshot-agent-b"
    for item in pods:
        item.spec.node_name = "worker"
    monkeypatch.setattr(client.CoreV1Api, "list_namespaced_pod", lambda *args, **kwargs: client.V1PodList(items=pods))

    class TruncatedResponse(io.BytesIO):
        def read(self, *args, **kwargs):
            raise http.client.IncompleteRead(b"partial", 10)

    def failing(request, timeout):
        url = request if isinstance(request, str) else request.full_url
        selected = ("/token?" in url) if endpoint == "token" else ("/manifests/" in url)
        if selected:
            attempts.append(url)
            if truncated:
                return TruncatedResponse()
            raise TimeoutError("registry read timed out")
        return fetch(request, timeout)

    monkeypatch.setattr(setup.urllib.request, "urlopen", failing)
    monkeypatch.setattr(setup.time, "sleep", sleeps.append)
    message = "IncompleteRead" if truncated else "registry read timed out"
    with pytest.raises(setup.SetupError, match=message) as error:
        setup.verify_running_images(namespace="test", release="snapshot", expected={**EXPECTED, "pagebroker": recorded})
    assert "snapshot-agent-a/pagebroker" in str(error.value)
    assert "snapshot-agent-b/pagebroker" in str(error.value)
    assert len(attempts) == 3
    assert sleeps == [1, 2]
