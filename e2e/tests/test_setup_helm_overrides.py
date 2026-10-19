# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks for extra Helm --set assignments during chart install."""

import pytest

from snapshot_e2e.infra import setup


def test_cli_assignments_are_kept_in_order():
    assert setup.parse_helm_overrides(
        ["image.agent.pullPolicy=IfNotPresent", "runtime.socketPath=/run/k3s.sock"]
    ) == ["image.agent.pullPolicy=IfNotPresent", "runtime.socketPath=/run/k3s.sock"]


def test_environment_fallback_is_newline_separated():
    # Newlines rather than commas: a comma is meaningful to helm --set itself.
    assert setup.parse_helm_overrides(
        None, "image.agent.tag=local\n\n  runtime.storageDir=/var/lib/k3s  \n"
    ) == ["image.agent.tag=local", "runtime.storageDir=/var/lib/k3s"]


def test_assignments_reach_helm_verbatim():
    # helm --set owns comma handling, so callers escape literal commas as `\,`
    # and we must not rewrite either form on the way through.
    assert setup.parse_helm_overrides(
        [r"a.b=one\,two", "c.d={one,two}"]
    ) == [r"a.b=one\,two", "c.d={one,two}"]


def test_cli_wins_over_the_environment():
    assert setup.parse_helm_overrides(["a=cli"], "b=env") == ["a=cli"]


@pytest.mark.parametrize("entry", ["image.agent.tag", "=orphan"])
def test_assignments_without_a_key_and_value_are_rejected(entry):
    with pytest.raises(setup.SetupError, match="KEY=VALUE"):
        setup.parse_helm_overrides([entry])


def test_overrides_are_appended_after_chart_defaults(monkeypatch):
    commands = []
    monkeypatch.setattr(setup, "run", lambda command, **_: commands.append(command))

    setup.install_snapshot_chart(
        kubeconfig=None,
        namespace="snapshot-e2e",
        release="snapshot",
        image_tag="local",
        pvc_name="snapshot-pvc",
        timeout="6m",
        helm_overrides=["image.agent.tag=override"],
    )

    (command,) = commands
    # Helm resolves repeated keys last-wins, so the override has to come after
    # the chart defaults to take effect.
    assert command[-2:] == ["--set", "image.agent.tag=override"]
    assert command.index("image.agent.tag=local") < command.index(
        "image.agent.tag=override"
    )


def test_install_enables_bound_publications_for_the_lifecycle_suite(monkeypatch):
    commands = []
    monkeypatch.setattr(setup, "run", lambda command, **_: commands.append(command))

    setup.install_snapshot_chart(
        kubeconfig=None,
        namespace="snapshot-e2e",
        release="snapshot",
        image_tag="local",
        pvc_name="snapshot-pvc",
        timeout="6m",
    )

    (command,) = commands
    assert command[-2:] == ["--set-json", "daemonset.imagePullSecrets=[]"]


@pytest.mark.parametrize(
    ("mode", "expected"),
    [
        ("vcluster", ["daemonset.nriHostNamespace=snapshot-e2e-vc", "a=user"]),
        ("direct", ["a=user"]),
    ],
)
def test_vcluster_restores_only_its_own_pods(mode, expected):
    assert setup.snapshot_helm_overrides(mode, "snapshot-e2e-vc", ["a=user"]) == expected


@pytest.mark.parametrize(
    "cpu_only,mode", [(False, "ReadWriteMany"), (True, "ReadWriteOnce")]
)
def test_installer_requests_configured_checkpoint_mode(monkeypatch, cpu_only, mode):
    from kubernetes import client
    from snapshot_e2e import k8s

    monkeypatch.setenv("SNAPSHOT_E2E_CPU_ONLY", "true" if cpu_only else "false")
    args = setup.parse_args(["--snapshot-tag", "test", "--mode", "direct"])
    context = setup.setup_context(args)
    seen = []
    monkeypatch.setattr(setup.preflight, "load_config", lambda *args: None)
    monkeypatch.setattr(k8s, "require_single_cpu_node", lambda: seen.append("single-node"))
    monkeypatch.setattr(setup, "ensure_target_namespace", lambda *args: None)
    monkeypatch.setattr(
        setup, "ensure_snapshot_release_can_own_cluster_resources", lambda *args: None
    )
    monkeypatch.setattr(setup, "install_snapshot_chart", lambda **kwargs: None)

    def create(self, namespace, body):
        seen.append(body.spec.access_modes)
        return client.V1PersistentVolumeClaim(
            spec=body.spec,
            status=client.V1PersistentVolumeClaimStatus(phase="Pending"),
        )

    monkeypatch.setattr(client.CoreV1Api, "create_namespaced_persistent_volume_claim", create)
    setup.setup_snapshot_install(args, context)
    assert seen == (["single-node", [mode]] if cpu_only else [[mode]])


@pytest.mark.parametrize(
    "expected,existing_modes",
    [
        ("ReadWriteOnce", ["ReadWriteMany"]),
        ("ReadWriteMany", ["ReadWriteOnce"]),
        ("ReadWriteOnce", ["ReadWriteOnce", "ReadWriteMany"]),
    ],
)
def test_existing_pvc_with_wrong_mode_is_rejected(monkeypatch, expected, existing_modes):
    from kubernetes import client
    from kubernetes.client import ApiException

    def conflict(*args, **kwargs):
        raise ApiException(status=409)

    monkeypatch.setattr(client.CoreV1Api, "create_namespaced_persistent_volume_claim", conflict)
    pvc = client.V1PersistentVolumeClaim(
        spec=client.V1PersistentVolumeClaimSpec(
            access_modes=existing_modes,
            resources=client.V1VolumeResourceRequirements(requests={"storage": "2Gi"}),
        ),
        status=client.V1PersistentVolumeClaimStatus(phase="Bound"),
    )
    monkeypatch.setattr(client.CoreV1Api, "read_namespaced_persistent_volume_claim", lambda *args: pvc)
    with pytest.raises(setup.SetupError, match="accessModes"):
        setup.ensure_checkpoint_pvc("test", "pvc", "2Gi", "", access_mode=expected)


def test_existing_gpu_pvc_with_rwx_and_rwo_remains_usable(monkeypatch):
    from kubernetes import client
    from kubernetes.client import ApiException

    def conflict(*args, **kwargs):
        raise ApiException(status=409)

    monkeypatch.setattr(client.CoreV1Api, "create_namespaced_persistent_volume_claim", conflict)
    pvc = client.V1PersistentVolumeClaim(
        spec=client.V1PersistentVolumeClaimSpec(
            access_modes=["ReadWriteOnce", "ReadWriteMany"],
            resources=client.V1VolumeResourceRequirements(requests={"storage": "2Gi"}),
        ),
        status=client.V1PersistentVolumeClaimStatus(phase="Bound"),
    )
    monkeypatch.setattr(
        client.CoreV1Api, "read_namespaced_persistent_volume_claim", lambda *args: pvc
    )
    setup.ensure_checkpoint_pvc("test", "pvc", "2Gi", "", access_mode="ReadWriteMany")


@pytest.mark.parametrize(
    "cpu_only,modes,accepted",
    [
        (False, ["ReadWriteOnce", "ReadWriteMany"], True),
        (True, ["ReadWriteOnce"], True),
        (True, ["ReadWriteOnce", "ReadWriteMany"], False),
    ],
)
def test_environment_uses_same_checkpoint_mode_contract(monkeypatch, cpu_only, modes, accepted):
    from types import SimpleNamespace

    import test_environment as environment
    from snapshot_e2e import k8s

    config = k8s.E2EConfig("test", "snapshot", "pvc", None, cpu_only=cpu_only)
    monkeypatch.setattr(k8s.E2EConfig, "from_env", classmethod(lambda cls: config))
    monkeypatch.setattr(k8s, "configure", lambda *_: None)
    monkeypatch.setattr(
        k8s, "read_namespace", lambda *_: SimpleNamespace(status=SimpleNamespace(phase="Active"))
    )
    version = SimpleNamespace(name="v1alpha1", served=True)
    monkeypatch.setattr(
        k8s, "read_crd", lambda *_: SimpleNamespace(spec=SimpleNamespace(versions=[version]))
    )
    monkeypatch.setattr(k8s, "snapshot_custom_resource_api_is_accessible", lambda *_: None)
    monkeypatch.setattr(
        k8s, "read_pvc", lambda *_: SimpleNamespace(
            status=SimpleNamespace(phase="Bound"),
            spec=SimpleNamespace(access_modes=modes),
        )
    )
    monkeypatch.setattr(k8s, "list_snapshot_pods", lambda *_: [object()])
    monkeypatch.setattr(k8s, "pod_containers_ready", lambda *_: True)
    monkeypatch.setattr(k8s, "list_snapshot_daemonsets", lambda *_: [object()])
    monkeypatch.setattr(k8s, "daemonset_scheduled", lambda *_: True)
    if accepted:
        environment.test_snapshot_e2e_environment_is_ready()
    else:
        with pytest.raises(AssertionError):
            environment.test_snapshot_e2e_environment_is_ready()


def test_cpu_installer_rejects_vcluster_before_mutation(monkeypatch):
    monkeypatch.setenv("SNAPSHOT_E2E_CPU_ONLY", "true")
    args = setup.parse_args(["--mode", "vcluster"])
    with pytest.raises(setup.SetupError, match="requires direct mode"):
        setup.setup_context(args)


def test_cpu_installer_rejects_multiple_nodes_before_mutation(monkeypatch):
    from kubernetes import client

    monkeypatch.setenv("SNAPSHOT_E2E_CPU_ONLY", "true")
    args = setup.parse_args(["--snapshot-tag", "test", "--mode", "direct"])
    context = setup.setup_context(args)
    monkeypatch.setattr(setup.preflight, "load_config", lambda *args: None)
    monkeypatch.setattr(
        client.CoreV1Api, "list_node",
        lambda self: client.V1NodeList(items=[client.V1Node(), client.V1Node()]),
    )
    mutations = []
    monkeypatch.setattr(setup, "ensure_target_namespace", lambda *args: mutations.append("namespace"))
    monkeypatch.setattr(setup, "ensure_checkpoint_pvc", lambda **kwargs: mutations.append("pvc"))
    monkeypatch.setattr(setup, "install_snapshot_chart", lambda **kwargs: mutations.append("chart"))
    with pytest.raises(ValueError, match="exactly one"):
        setup.setup_snapshot_install(args, context)
    assert mutations == []
