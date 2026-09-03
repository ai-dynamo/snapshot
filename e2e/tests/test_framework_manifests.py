# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that the framework guides are usable as e2e workloads.

The framework tests lift the guide Deployments into Pods with minimal edits, so
anything the guide gets wrong about the checkpoint/restore contract fails on a
GPU cluster minutes into a run. These checks pin the contract cheaply, on every
pull request, without a cluster:

- the source and restore pods carry the restore-pod contract pieces the agent
  relies on (control volume at /snapshot-control with subPath main,
  SNAPSHOT_CONTROL_DIR, io_uring seccomp profile, /dev/net/tun, nvidia
  RuntimeClass, one GPU);
- the restore pod is a standby placeholder (SNAPSHOT_RESTORE_STANDBY=1) that
  restores this run's PodSnapshot;
- the model the guide deploys is the one the tests expect;
- the image tag is content-addressed and deterministic.
"""

from __future__ import annotations

import pytest

from snapshot_e2e import framework_workloads as fw
from snapshot_e2e import frameworks
from snapshot_e2e import k8s
from snapshot_e2e import workloads

CONFIG = k8s.E2EConfig(
    namespace="snapshot-e2e",
    release="snapshot",
    pvc_name="snapshot-pvc",
    kubeconfig=None,
)
IMAGE = "framework-under-test:local"


@pytest.fixture(autouse=True)
def _workload_image(monkeypatch: pytest.MonkeyPatch) -> None:
    # TestRun.new resolves the generic workload image eagerly; these checks
    # never schedule anything, so any value satisfies it.
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", "snapshot-workload:test")


@pytest.fixture(params=sorted(frameworks.FRAMEWORKS))
def spec(request: pytest.FixtureRequest) -> frameworks.FrameworkSpec:
    return frameworks.FRAMEWORKS[request.param]


def pods(spec: frameworks.FrameworkSpec) -> tuple[dict, dict, workloads.TestRun]:
    run = workloads.TestRun.new("manifest")
    source = fw.source_pod(config=CONFIG, run=run, spec=spec, image=IMAGE)
    restore = fw.restore_pod(
        config=CONFIG, run=run, spec=spec, source_node="gpu-node-0", image=IMAGE
    )
    return source, restore, run


@pytest.mark.workload
def test_guide_pods_satisfy_restore_pod_contract(spec: frameworks.FrameworkSpec) -> None:
    source, restore, _ = pods(spec)
    for pod in (source, restore):
        pod_spec = pod["spec"]
        assert pod_spec["runtimeClassName"] == "nvidia"
        assert pod_spec["securityContext"]["seccompProfile"] == {
            "type": "Localhost",
            "localhostProfile": "profiles/block-iouring.json",
        }
        assert pod_spec["restartPolicy"] == "Never"

        main = fw.main_container(pod)
        assert main["image"] == IMAGE
        assert main["resources"]["limits"]["nvidia.com/gpu"] == "1"
        assert fw.env_value(main, "SNAPSHOT_CONTROL_DIR") == workloads.CONTROL_DIR
        assert {
            "name": "snapshot-control",
            "mountPath": workloads.CONTROL_DIR,
            "subPath": workloads.CONTAINER,
        } in main["volumeMounts"]
        assert any(mount["mountPath"] == "/dev/net/tun" for mount in main["volumeMounts"])
        volumes = {volume["name"]: volume for volume in pod_spec["volumes"]}
        assert volumes["snapshot-control"] == {"name": "snapshot-control", "emptyDir": {}}
        assert volumes["tun"]["hostPath"] == {"path": "/dev/net/tun", "type": "CharDevice"}
        for container in pod_spec.get("initContainers", []):
            assert container["image"] == IMAGE


@pytest.mark.workload
def test_source_pod_is_checkpointable_and_unannotated(spec: frameworks.FrameworkSpec) -> None:
    source, _, _ = pods(spec)
    main = fw.main_container(source)
    # Ready == checkpointable: the test waits for pod readiness before creating
    # the PodSnapshot, which is only sound if readiness is gated on the file.
    assert main["readinessProbe"]["exec"]["command"] == ["cat", workloads.SOURCE_READY]
    assert fw.env_value(main, "SNAPSHOT_RESTORE_STANDBY") is None
    assert source["metadata"]["annotations"] == {}


@pytest.mark.workload
def test_restore_pod_is_standby_placeholder_for_this_run(spec: frameworks.FrameworkSpec) -> None:
    _, restore, run = pods(spec)
    main = fw.main_container(restore)
    # Without standby the guide program would load a model into the GPU while
    # the agent restores into the same container.
    assert fw.env_value(main, "SNAPSHOT_RESTORE_STANDBY") == "1"
    assert restore["metadata"]["annotations"] == {fw.RESTORE_FROM_ANNOTATION: run.snapshot_name}
    node_terms = restore["spec"]["affinity"]["nodeAffinity"][
        "requiredDuringSchedulingIgnoredDuringExecution"
    ]["nodeSelectorTerms"]
    assert node_terms[0]["matchFields"][0]["values"] == ["gpu-node-0"]


@pytest.mark.workload
def test_guide_deploys_the_expected_model(spec: frameworks.FrameworkSpec) -> None:
    source, restore, _ = pods(spec)
    for pod in (source, restore):
        for container in pod["spec"].get("initContainers", []) + pod["spec"]["containers"]:
            model = fw.env_value(container, "SNAPSHOT_MODEL")
            if model is not None:
                assert model == spec.model, f"{container['name']} deploys {model}"
    assert fw.env_value(fw.main_container(source), "SNAPSHOT_MODEL") == spec.model


@pytest.mark.workload
def test_e2e_scheduling_is_merged_into_guide_pods(spec: frameworks.FrameworkSpec) -> None:
    source, restore, _ = pods(spec)
    scheduling = workloads.workload_scheduling()
    for pod in (source, restore):
        for key, value in scheduling["nodeSelector"].items():
            assert pod["spec"]["nodeSelector"][key] == value
        for toleration in scheduling["tolerations"]:
            assert toleration in pod["spec"]["tolerations"]


@pytest.mark.workload
def test_framework_image_is_content_addressed(
    spec: frameworks.FrameworkSpec, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("SNAPSHOT_E2E_FRAMEWORK_IMAGE", raising=False)
    first = frameworks.framework_image(spec)
    second = frameworks.framework_image(spec)
    assert first == second
    assert first.startswith(f"ghcr.io/ai-dynamo/snapshot/e2e-{spec.name}:")
    module = frameworks.image_tag_module()
    assert all(path.is_file() for path in module.image_inputs(spec.name))

    monkeypatch.setenv("SNAPSHOT_E2E_FRAMEWORK_IMAGE", IMAGE)
    assert frameworks.framework_image(spec) == IMAGE


@pytest.mark.workload
def test_model_cache_pvc_uses_configured_storage_class(monkeypatch: pytest.MonkeyPatch) -> None:
    spec = frameworks.FRAMEWORKS["sglang"]
    monkeypatch.setenv("SNAPSHOT_E2E_STORAGE_CLASS", "fast-rwx")
    pvc = fw.model_cache_pvc(config=CONFIG, spec=spec)
    assert pvc is not None
    assert pvc["metadata"]["namespace"] == CONFIG.namespace
    assert pvc["spec"]["storageClassName"] == "fast-rwx"
    assert fw.model_cache_pvc(config=CONFIG, spec=frameworks.FRAMEWORKS["vllm"]) is None


@pytest.mark.workload
def test_override_image_is_always_pulled(monkeypatch: pytest.MonkeyPatch) -> None:
    spec = frameworks.FRAMEWORKS["vllm"]
    monkeypatch.delenv("SNAPSHOT_E2E_FRAMEWORK_IMAGE", raising=False)
    run = workloads.TestRun.new("pull")
    pod = fw.source_pod(config=CONFIG, run=run, spec=spec, image=IMAGE)
    assert fw.main_container(pod)["imagePullPolicy"] == "IfNotPresent"
    # A dev override is a mutable tag: a cached copy would test stale bits.
    monkeypatch.setenv("SNAPSHOT_E2E_FRAMEWORK_IMAGE", "registry/vllm-snapshot:dev")
    pod = fw.source_pod(config=CONFIG, run=run, spec=spec)
    for container in pod["spec"]["containers"]:
        assert container["image"] == "registry/vllm-snapshot:dev"
        assert container["imagePullPolicy"] == "Always"


@pytest.mark.workload
def test_control_file_parsing_distinguishes_absent_from_content() -> None:
    from snapshot_e2e import inference

    # exec merges stderr into the output: a missing file must not read as content.
    assert inference.parse_control_file("cat: /snapshot-control/x: No such file or directory") is None
    assert inference.parse_control_file("") is None
    assert inference.parse_control_file(inference._FILE_MARKER + "hello\n") == "hello\n"
    assert inference.parse_control_file(inference._FILE_MARKER) == ""


@pytest.mark.workload
def test_framework_selection_from_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("SNAPSHOT_E2E_FRAMEWORK", raising=False)
    assert frameworks.selected_frameworks() == sorted(frameworks.FRAMEWORKS)
    monkeypatch.setenv("SNAPSHOT_E2E_FRAMEWORK", "vllm, tensorrt-llm")
    assert frameworks.selected_frameworks() == ["vllm", "tensorrt-llm"]
    monkeypatch.setenv("SNAPSHOT_E2E_FRAMEWORK", "nope")
    with pytest.raises(RuntimeError):
        frameworks.selected_frameworks()


@pytest.mark.workload
def test_control_file_names_match_the_guide_program(spec: frameworks.FrameworkSpec) -> None:
    """The sentinel paths in FrameworkSpec are copies of literals in the guide's
    app.py. A rename on either side would otherwise surface only as a GPU run
    timing out on "neither sentinel" minutes later.
    """
    program = (spec.guide_dir / "app.py").read_text(encoding="utf-8")
    sentinels = [spec.precheck_file, spec.restore_ready_file]
    error_file = getattr(spec, "restore_error_file", None)
    if error_file:
        sentinels.append(error_file)
    for path in sentinels:
        name = path.rsplit("/", 1)[-1]
        assert f'"{name}"' in program, f"{spec.name}/app.py does not write {name!r}"
