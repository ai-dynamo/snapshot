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
- the restore pod is an inert placeholder (an explicit sleep command) that
  restores this run's PodSnapshot;
- the model the guide deploys is the one the tests expect;
- the image tag is content-addressed and deterministic.
"""

from __future__ import annotations

import json

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
    server="nfs.example.internal", path="/exports/models", pvc_name="model-cache"
)


@pytest.fixture(autouse=True)
def _workload_image(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", AGENT_IMAGE)
    monkeypatch.delenv("SNAPSHOT_E2E_RESTORE_NODE", raising=False)
    monkeypatch.delenv("SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE", raising=False)
    monkeypatch.delenv("SNAPSHOT_E2E_RECIPE", raising=False)


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
def test_explicit_restore_node(monkeypatch: pytest.MonkeyPatch, spec: frameworks.FrameworkSpec) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_RESTORE_NODE", "gpu-node-destination")
    _, restored, _ = pods(spec)
    assert restored["spec"]["affinity"] == workloads.same_node_affinity("gpu-node-destination")


@pytest.mark.workload
@pytest.mark.parametrize("size", [1, 2])
def test_tensor_parallel_override_matches_source_and_restore_gpus(
    monkeypatch: pytest.MonkeyPatch, spec: frameworks.FrameworkSpec, size: int,
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE", str(size))
    source, restored, _ = pods(spec)
    for pod in (source, restored):
        main = fw.main_container(pod)
        assert fw.env_value(main, "SNAPSHOT_TENSOR_PARALLEL_SIZE") == str(size)
        assert main["resources"]["limits"]["nvidia.com/gpu"] == str(size)
        assert sum(e["name"] == "SNAPSHOT_TENSOR_PARALLEL_SIZE" for e in main["env"]) == 1


@pytest.mark.workload
@pytest.mark.parametrize("value", ["", "0", "-1", "2.5", "two"])
def test_tensor_parallel_override_rejects_invalid_values(
    monkeypatch: pytest.MonkeyPatch, value: str,
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE", value)
    spec = frameworks.FRAMEWORKS["vllm"]
    run = workloads.TestRun.new("invalid-parallelism")
    with pytest.raises(ValueError, match="SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE must be a positive integer"):
        fw.source_pod(config=CONFIG, run=run, spec=spec, image=IMAGE)
    with pytest.raises(ValueError, match="SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE must be a positive integer"):
        fw.restore_pod(config=CONFIG, run=run, spec=spec, source_node="n0", image=IMAGE)


@pytest.mark.workload
def test_unset_tensor_parallel_override_preserves_recipe() -> None:
    deployment = fw.load_manifest(frameworks.FRAMEWORKS["vllm"].deployment_manifest)
    main = deployment["spec"]["template"]["spec"]["containers"][0]
    fw.set_env(main, "SNAPSHOT_TENSOR_PARALLEL_SIZE", "2")
    main["resources"]["limits"]["nvidia.com/gpu"] = "2"
    pod = fw.pod_from_deployment(deployment, name="tp", namespace="test", labels={}, image=IMAGE)
    rendered = fw.main_container(pod)
    assert fw.env_value(rendered, "SNAPSHOT_TENSOR_PARALLEL_SIZE") == "2"
    assert rendered["resources"]["limits"]["nvidia.com/gpu"] == "2"


@pytest.mark.workload
def test_tensor_parallel_override_updates_explicit_gpu_requests(monkeypatch: pytest.MonkeyPatch) -> None:
    deployment = fw.load_manifest(frameworks.FRAMEWORKS["vllm"].deployment_manifest)
    main = deployment["spec"]["template"]["spec"]["containers"][0]
    main["resources"]["requests"] = {"nvidia.com/gpu": "1", "cpu": "1"}
    monkeypatch.setenv("SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE", "2")
    pod = fw.pod_from_deployment(deployment, name="tp", namespace="test", labels={}, image=IMAGE)
    assert fw.main_container(pod)["resources"]["requests"] == {"nvidia.com/gpu": "2", "cpu": "1"}
    assert main["resources"]["requests"]["nvidia.com/gpu"] == "1"


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
        assert fw.env_value(main, "SNAPSHOT_TENSOR_PARALLEL_SIZE") == "1"
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
            expected = AGENT_IMAGE if container["name"] == "snapshot-cuda-install" else IMAGE
            assert container["image"] == expected


@pytest.mark.workload
def test_source_pod_is_checkpointable_with_shared_memory_enabled(spec: frameworks.FrameworkSpec) -> None:
    source, _, _ = pods(spec)
    main = fw.main_container(source)
    # Ready == checkpointable: the test waits for pod readiness before creating
    # the PodSnapshot, which is only sound if readiness is gated on the file.
    assert main["readinessProbe"]["exec"]["command"] == ["cat", workloads.SOURCE_READY]
    assert fw.env_value(main, "SNAPSHOT_RESTORE_STANDBY") is None
    assert source["metadata"]["annotations"] == {
        "nvidia.com/cuda-shared-memory-support": "enabled",
    }


@pytest.mark.workload
@pytest.mark.parametrize("model_cache", [None, CACHE], ids=["guide-cache", "shared-cache"])
@pytest.mark.parametrize("agent_image", [AGENT_IMAGE, "snapshot-agent@sha256:" + "a" * 64])
def test_source_delivers_matching_shared_memory_bundle(
    spec: frameworks.FrameworkSpec,
    monkeypatch: pytest.MonkeyPatch,
    model_cache: frameworks.SharedModelCache | None,
    agent_image: str,
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", agent_image)
    monkeypatch.setenv("SNAPSHOT_E2E_FRAMEWORK_IMAGE", IMAGE)
    run = workloads.TestRun.new("delivery")
    source = fw.source_pod(config=CONFIG, run=run, spec=spec, model_cache=model_cache)
    main = fw.main_container(source)
    assert main["image"] == IMAGE
    assert main["imagePullPolicy"] == "Always"
    assert main["command"][:4] == [
        "/tmp/snapshot-cuda/cuinterpose-launch", "--library",
        "/tmp/snapshot-cuda/libcuinterpose.so", "--",
    ]
    assert main["command"][4] == "python3"
    assert main["command"][-1] == "/snapshot-app/app.py"
    assert {"name": "snapshot-cuda", "mountPath": "/tmp/snapshot-cuda", "readOnly": True} in main["volumeMounts"]
    volumes = {volume["name"]: volume for volume in source["spec"]["volumes"]}
    assert volumes["snapshot-cuda"] == {"name": "snapshot-cuda", "emptyDir": {}}
    installers = [c for c in source["spec"]["initContainers"] if c["name"] == "snapshot-cuda-install"]
    assert len(installers) == 1
    installer = installers[0]
    assert installer["image"] == agent_image
    assert installer["imagePullPolicy"] == ("IfNotPresent" if "@sha256:" in agent_image else "Always")
    assert installer["command"] == ["/bin/cp"]
    assert installer["args"] == [
        "--preserve=mode", "--",
        "/usr/local/lib/snapshot/libcuinterpose.so",
        "/usr/local/lib/snapshot/libcuinterpose_core.so",
        "/usr/local/bin/cuinterpose-launch", "/tmp/snapshot-cuda/",
    ]
    assert installer["volumeMounts"] == [
        {"name": "snapshot-cuda", "mountPath": "/tmp/snapshot-cuda"},
    ]


@pytest.mark.workload
def test_source_preserves_workload_annotations_but_not_restore_routing(
    spec: frameworks.FrameworkSpec, monkeypatch: pytest.MonkeyPatch,
) -> None:
    deployment = fw.load_manifest(spec.deployment_manifest)
    deployment["spec"]["template"]["metadata"]["annotations"].update({
        "example.com/workload": "keep",
        fw.RESTORE_FROM_ANNOTATION: "old-snapshot",
        "nvidia.com/restore-container-map": "main=main",
    })
    monkeypatch.setattr(fw, "load_manifest", lambda _: deployment)
    source = fw.source_pod(config=CONFIG, run=workloads.TestRun.new("annotations"), spec=spec, image=IMAGE)
    assert source["metadata"]["annotations"] == {
        "nvidia.com/cuda-shared-memory-support": "enabled",
        "example.com/workload": "keep",
    }


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
    # namespace, including this one, even though the placeholder's own
    # command never reads it.
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
def test_restore_pod_is_standby_placeholder_for_this_run(spec: frameworks.FrameworkSpec) -> None:
    _, restore, run = pods(spec)
    main = fw.main_container(restore)
    # The guide manifests keep the placeholder inert with an explicit sleep
    # command; without it the guide program would load a model into the GPU
    # while the agent restores into the same container.
    assert main["command"] == ["/bin/sh", "-c", "exec sleep infinity"]
    assert "args" not in main
    assert fw.env_value(main, "SNAPSHOT_RESTORE_STANDBY") is None
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
        # The guide's own claim (SGLang's sglang-model-cache) must not linger.
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
        assert fw.env_value(main, "SNAPSHOT_CONTROL_DIR") == workloads.CONTROL_DIR
        assert any(m["mountPath"] == "/dev/net/tun" for m in main["volumeMounts"])


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
@pytest.mark.parametrize("engine,recipe,size", [
    ("vllm", "glm-5.3", 8),
    ("sglang", "glm-5.3", 8),
    ("tensorrt-llm", "glm-5.3", 8),
    ("vllm", "deepseek-v4-flash", 4),
    ("sglang", "deepseek-v4-flash", 4),
])
def test_multi_gpu_recipe_matches_restore_and_shared_cache(
    monkeypatch: pytest.MonkeyPatch, engine: str, recipe: str, size: int,
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_RECIPE", recipe)
    spec = frameworks.framework_spec(engine)
    source, restore, run = pods(spec)
    source_main, restore_main = map(fw.main_container, (source, restore))
    argument_variable = {"vllm": "VLLM_ENGINE_ARGS", "sglang": "SGLANG_ENGINE_ARGS",
                         "tensorrt-llm": "TRTLLM_ENGINE_ARGS"}[engine]
    arguments = json.loads(fw.env_value(source_main, argument_variable))
    assert arguments == json.loads(fw.env_value(restore_main, argument_variable))
    assert len(arguments["revision"]) == 40
    assert spec.case_name == f"{engine}-{recipe}"
    assert spec.model == fw.env_value(source_main, "SNAPSHOT_MODEL")
    assert source["metadata"]["annotations"][fw.SHARED_MEMORY_ANNOTATION] == "enabled"
    assert restore["metadata"]["annotations"][fw.RESTORE_FROM_ANNOTATION] == run.snapshot_name
    assert restore_main["command"] == ["/bin/sh", "-c", "exec sleep infinity"]
    for pod in (source, restore):
        main = fw.main_container(pod)
        assert main["resources"]["limits"]["nvidia.com/gpu"] == str(size)
        assert fw.env_value(main, "SNAPSHOT_TENSOR_PARALLEL_SIZE") == str(size)
        assert fw.env_value(main, "SNAPSHOT_MODEL_REVISION") == arguments["revision"]
        volumes = {v["name"]: v for v in pod["spec"]["volumes"]}
        assert volumes["shm"]["emptyDir"]["medium"] == "Memory"
        pvc = fw.model_cache_pvc(config=CONFIG, spec=spec)
        assert volumes["model-cache"]["persistentVolumeClaim"]["claimName"] == pvc["metadata"]["name"]
        assert {"name": "shm", "mountPath": "/dev/shm"} in main["volumeMounts"]
        downloader = next(c for c in pod["spec"]["initContainers"] if c["name"] == "model-cache")
        assert fw.env_value(downloader, "SNAPSHOT_MODEL_REVISION") == arguments["revision"]
        assert fw.env_value(downloader, "SNAPSHOT_MODEL") == spec.model
        fw.use_shared_model_cache(pod["spec"], CACHE)
        assert not any(c["name"] == "model-cache" for c in pod["spec"].get("initContainers", []))
        assert fw.env_value(fw.main_container(pod), "HF_HUB_OFFLINE") == "1"
    monkeypatch.setenv("SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE", "2")
    with pytest.raises(ValueError, match=f"requires TP{size}"):
        frameworks.framework_spec(engine)


@pytest.mark.workload
@pytest.mark.parametrize("engine,recipe,error", [
    ("vllm", "../vllm", "unknown SNAPSHOT_E2E_RECIPE"),
    ("tensorrt-llm", "deepseek-v4-flash", "has no deepseek-v4-flash recipe"),
])
def test_recipe_selection_rejects_unknown_or_unavailable_profiles(
    monkeypatch: pytest.MonkeyPatch, engine: str, recipe: str, error: str,
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_RECIPE", recipe)
    with pytest.raises(ValueError, match=error):
        frameworks.framework_spec(engine)
