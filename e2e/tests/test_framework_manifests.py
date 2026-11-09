# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that the framework guides are usable as e2e workloads.

The framework tests lift the guide Deployments into Pods with minimal edits, so
anything the guide gets wrong about the checkpoint/restore contract fails on a
GPU cluster minutes into a run. These checks pin the contract cheaply, on every
pull request, without a cluster:

- the source and restore pods carry the restore-pod contract pieces the agent
  relies on (control volume at /snapshot-control with subPath main,
  the apps' default control directory, io_uring seccomp profile, nvidia
  RuntimeClass, one GPU);
- the restore pod keeps the source's command, which the restore proxy
  replaces, or idles on nodes without the proxy, and restores this run's
  PodSnapshot;
- the model the guide deploys is the one the tests expect;
- the image tag is content-addressed and deterministic.
"""

from __future__ import annotations

from dataclasses import replace

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
AGENT_IMAGE = "snapshot-agent:test"
CACHE = frameworks.SharedModelCache(
    server="nfs.example.internal", path="/exports/models", pvc_name="shared-model-cache"
)


@pytest.fixture(autouse=True)
def _workload_image(monkeypatch: pytest.MonkeyPatch) -> None:
    # TestRun.new resolves the generic workload image eagerly; these checks
    # never schedule anything, so any value satisfies it.
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", AGENT_IMAGE)


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
def test_framework_pods_use_explicit_cpu_only_scheduling(
    spec: frameworks.FrameworkSpec, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_CPU_ONLY", "false")
    config = replace(CONFIG, cpu_only=True)
    run = workloads.TestRun.new("cpu-manifest")
    source = fw.source_pod(config=config, run=run, spec=spec, image=IMAGE)
    restore = fw.restore_pod(
        config=config, run=run, spec=spec, source_node="only-node", image=IMAGE
    )
    for pod in (source, restore):
        # Preserve GPU requirements from the guide itself; CPU-only mode only
        # suppresses extra scheduling constraints from the e2e helper.
        assert pod["spec"]["nodeSelector"] == {"nvidia.com/gpu.present": "true"}
        assert pod["spec"]["tolerations"] == []


@pytest.mark.workload
def test_guide_pods_satisfy_restore_pod_contract(spec: frameworks.FrameworkSpec) -> None:
    source, restore, _ = pods(spec)
    guide_pvc = fw.model_cache_pvc(config=CONFIG, spec=spec)
    cache_claims = {guide_pvc["metadata"]["name"]} if guide_pvc else set()
    assert fw.env_value(fw.main_container(restore), "SNAPSHOT_CONTROL_DIR") == workloads.CONTROL_DIR
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
        assert fw.env_value(main, "SNAPSHOT_CONTROL_DIR") in (None, workloads.CONTROL_DIR)
        assert {
            "name": "snapshot-control",
            "mountPath": workloads.CONTROL_DIR,
            "subPath": workloads.CONTAINER,
        } in main["volumeMounts"]
        assert all(mount["mountPath"] != "/dev/net/tun" for mount in main["volumeMounts"])
        volumes = {volume["name"]: volume for volume in pod_spec["volumes"]}
        assert {
            volume["persistentVolumeClaim"]["claimName"] for volume in volumes.values()
            if "persistentVolumeClaim" in volume
        } <= cache_claims
        assert volumes["snapshot-control"] == {"name": "snapshot-control", "emptyDir": {}}
        assert not any(v.get("hostPath", {}).get("path") == "/dev/net/tun" for v in volumes.values())
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
def test_source_pod_mounts_app_py_from_the_named_configmap(
    spec: frameworks.FrameworkSpec,
) -> None:
    source, _, _ = pods(spec)
    main = fw.main_container(source)
    volumes = {volume["name"]: volume for volume in source["spec"]["volumes"]}
    assert volumes["app"]["configMap"] == {"name": spec.app_configmap_name}
    assert {"name": "app", "mountPath": "/snapshot-app", "readOnly": True} in main[
        "volumeMounts"
    ]
    # Runs the mounted copy, not anything baked into the image.
    assert "/snapshot-app/app.py" in main["command"]


@pytest.mark.workload
def test_restore_pod_also_mounts_the_app_configmap(
    spec: frameworks.FrameworkSpec,
) -> None:
    # CRIU recreates every mount from the source container's mount
    # namespace, including this one, even though the restore container's own
    # command never runs.
    _, restore, _ = pods(spec)
    main = fw.main_container(restore)
    volumes = {volume["name"]: volume for volume in restore["spec"]["volumes"]}
    assert volumes["app"]["configMap"] == {"name": spec.app_configmap_name}
    assert {"name": "app", "mountPath": "/snapshot-app", "readOnly": True} in main[
        "volumeMounts"
    ]


@pytest.mark.workload
def test_app_configmap_matches_the_guide_program(
    spec: frameworks.FrameworkSpec,
) -> None:
    configmap = fw.app_configmap(config=CONFIG, spec=spec)
    assert configmap["metadata"]["name"] == spec.app_configmap_name
    assert configmap["metadata"]["namespace"] == CONFIG.namespace
    assert configmap["data"]["app.py"] == spec.app_py.read_text(encoding="utf-8")


@pytest.mark.workload
def test_restore_pod_restores_this_run(
    spec: frameworks.FrameworkSpec, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("SNAPSHOT_E2E_RESTORE_PROXY", raising=False)
    source, restore, run = pods(spec)
    main = fw.main_container(restore)
    source_main = fw.main_container(source)
    # The restore container keeps the engine's command, which Snapshot
    # replaces with the restore proxy. Model arguments come from the checkpoint.
    assert main["command"] == source_main["command"]
    assert fw.env_value(main, "SNAPSHOT_RESTORE_STANDBY") is None
    assert restore["metadata"]["annotations"] == {fw.RESTORE_FROM_ANNOTATION: run.snapshot_name}
    node_terms = restore["spec"]["affinity"]["nodeAffinity"][
        "requiredDuringSchedulingIgnoredDuringExecution"
    ]["nodeSelectorTerms"]
    assert node_terms[0]["matchFields"][0]["values"] == ["gpu-node-0"]


@pytest.mark.workload
def test_restore_pod_idles_without_the_proxy(
    spec: frameworks.FrameworkSpec, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_RESTORE_PROXY", "false")
    _, restore, _ = pods(spec)
    main = fw.main_container(restore)
    # The source's command would load a second model beside the restored one.
    assert main["command"] == workloads.INERT_COMMAND
    assert "args" not in main


@pytest.mark.workload
@pytest.mark.parametrize(
    ("agent_log", "proxy"),
    [
        ("... Registered with the container runtime ...", True),
        ("... Restoring without the restore proxy ...", False),
    ],
)
def test_restore_pod_follows_the_agent_in_auto_mode(
    monkeypatch: pytest.MonkeyPatch, agent_log: str, proxy: bool
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_RESTORE_PROXY", "auto")
    monkeypatch.setattr(fw.lifecycle, "checkpoint_agent_pod", lambda config, node: "agent-0")
    monkeypatch.setattr(fw.lifecycle.k8s, "pod_logs", lambda *args, **kwargs: agent_log)
    _, restore, _ = pods(frameworks.FRAMEWORKS["vllm"])
    idles = fw.main_container(restore)["command"] == workloads.INERT_COMMAND
    assert idles is not proxy


@pytest.mark.workload
def test_auto_mode_fails_when_the_agent_says_neither(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_RESTORE_PROXY", "auto")
    monkeypatch.setattr(fw.lifecycle, "checkpoint_agent_pod", lambda config, node: "agent-0")
    monkeypatch.setattr(fw.lifecycle.k8s, "pod_logs", lambda *args, **kwargs: "starting")
    with pytest.raises(AssertionError, match="logged neither"):
        pods(frameworks.FRAMEWORKS["vllm"])


@pytest.mark.workload
def test_guide_deploys_the_expected_model(spec: frameworks.FrameworkSpec) -> None:
    source, restore, _ = pods(spec)
    assert frameworks.guide_model(fw.main_container(source)["args"])[0] == spec.model
    for container in source["spec"].get("initContainers", []):
        if container["name"] == fw.GUIDE_CACHE_INIT_CONTAINER:
            assert container["args"][1] == spec.model
    for pod in (source, restore):
        for container in pod["spec"].get("initContainers", []) + pod["spec"]["containers"]:
            assert fw.env_value(container, "SNAPSHOT_MODEL") is None


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
def test_framework_image_comes_from_the_guide_manifest(
    spec: frameworks.FrameworkSpec, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("SNAPSHOT_E2E_FRAMEWORK_IMAGE", raising=False)
    first = frameworks.framework_image(spec)
    second = frameworks.framework_image(spec)
    assert first == second
    deployment = fw.load_manifest(spec.deployment_manifest)
    assert first == deployment["spec"]["template"]["spec"]["containers"][0]["image"]
    # The upstream framework image, not a Snapshot-built one: no guide-specific
    # image to build, push, or keep available.
    assert not first.startswith("ghcr.io/ai-dynamo/snapshot/")

    monkeypatch.setenv("SNAPSHOT_E2E_FRAMEWORK_IMAGE", IMAGE)
    assert frameworks.framework_image(spec) == IMAGE


@pytest.mark.workload
def test_shared_model_cache_replaces_guide_download(spec: frameworks.FrameworkSpec) -> None:
    run = workloads.TestRun.new("cache")
    source = fw.source_pod(config=CONFIG, run=run, spec=spec, image=IMAGE, model_cache=CACHE)
    restore = fw.restore_pod(
        config=CONFIG, run=run, spec=spec, source_node="n0", image=IMAGE, model_cache=CACHE
    )
    assert fw.env_value(fw.main_container(restore), "SNAPSHOT_CONTROL_DIR") == workloads.CONTROL_DIR
    for pod in (source, restore):
        pod_spec = pod["spec"]
        # No download init container: the export is mounted read-mostly and the
        # pod runs offline, so a downloader would fail or race the readers.
        assert not any(
            c["name"] == fw.GUIDE_CACHE_INIT_CONTAINER for c in pod_spec.get("initContainers", [])
        )
        volumes = {v["name"]: v for v in pod_spec["volumes"]}
        assert volumes[frameworks.MODEL_CACHE_VOLUME]["persistentVolumeClaim"] == {
            "claimName": CACHE.pvc_name
        }
        # The guide's own model-cache claim must not linger.
        assert sum(1 for v in pod_spec["volumes"] if v["name"] == frameworks.MODEL_CACHE_VOLUME) == 1
        main = fw.main_container(pod)
        assert {
            "name": frameworks.MODEL_CACHE_VOLUME,
            "mountPath": frameworks.MODEL_CACHE_MOUNT,
        } in main["volumeMounts"]
        assert fw.env_value(main, "HF_HOME") == frameworks.MODEL_CACHE_MOUNT
        assert fw.env_value(main, "HF_HUB_OFFLINE") == "1"
        # Exactly one HF_HOME even when the guide already set one (SGLang).
        assert sum(1 for e in main["env"] if e["name"] == "HF_HOME") == 1
        # Contract pieces are untouched by the cache rewrite.
        assert fw.env_value(main, "SNAPSHOT_CONTROL_DIR") in (None, workloads.CONTROL_DIR)
        assert all(m["mountPath"] != "/dev/net/tun" for m in main["volumeMounts"])


@pytest.mark.workload
def test_control_file_names_match_the_guide_program(spec: frameworks.FrameworkSpec) -> None:
    """The sentinel paths in FrameworkSpec are copies of literals in the guide's
    app.py. A rename on either side would otherwise surface only as a GPU run
    timing out on "neither sentinel" minutes later.
    """
    program = (spec.manifest_dir / "app.py").read_text(encoding="utf-8")
    sentinels = [spec.restore_ready_file]
    error_file = getattr(spec, "restore_error_file", None)
    if error_file:
        sentinels.append(error_file)
    for path in sentinels:
        name = path.rsplit("/", 1)[-1]
        assert f'"{name}"' in program, f"{spec.name}/app.py does not write {name!r}"


@pytest.mark.workload
@pytest.mark.parametrize("engine,recipe", [
    ("vllm", "glm-5.3"),
    ("sglang", "glm-5.3"),
    ("tensorrt-llm", "glm-5.3"),
    ("vllm", "deepseek-v4.1-flash"),
    ("sglang", "deepseek-v4.1-flash"),
])
def test_multi_gpu_recipe_preserves_capture_restore_contract(
    monkeypatch: pytest.MonkeyPatch, engine: str, recipe: str,
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_RECIPE", recipe)
    spec = frameworks.framework_spec(engine)
    run = workloads.TestRun.new("recipe")
    source = fw.source_pod(config=CONFIG, run=run, spec=spec, image=IMAGE, model_cache=CACHE)
    restore = fw.restore_pod(
        config=CONFIG, run=run, spec=spec, source_node="n0", image=IMAGE, model_cache=CACHE,
    )
    assert source["metadata"]["annotations"][fw.MULTI_GPU_ANNOTATION] == "enabled"
    installer = next(c for c in source["spec"]["initContainers"] if c["name"] == "snapshot-cuda-install")
    assert installer["image"] == AGENT_IMAGE
    model, revision, parallelism = frameworks.guide_model(fw.main_container(source)["args"])
    assert (model, parallelism) == (spec.model, 8)
    assert revision
    for pod in (source, restore):
        main = fw.main_container(pod)
        assert main["image"] == IMAGE
        assert main["resources"]["limits"]["nvidia.com/gpu"] == "8"
    restore_main = fw.main_container(restore)
    assert fw.env_value(restore_main, "SNAPSHOT_CONTROL_DIR") == workloads.CONTROL_DIR
    # The restore profile keeps the workload command the source runs under the
    # launcher. Snapshot replaces it with the restore proxy.
    restore_command = restore_main["command"]
    assert fw.main_container(source)["command"][-len(restore_command):] == restore_command
    assert restore["metadata"]["annotations"][fw.RESTORE_FROM_ANNOTATION] == run.snapshot_name
