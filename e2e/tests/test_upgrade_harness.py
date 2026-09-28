# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from pathlib import Path

import pytest
import yaml
from kubernetes import client

from snapshot_e2e.upgrade import checks
from snapshot_e2e.upgrade import configs
from snapshot_e2e.upgrade import scenarios
from snapshot_e2e import k8s
from snapshot_e2e.upgrade.context import Timings, UpgradeContext, UpgradeSettings


ENV = (
    "SNAPSHOT_E2E_UPGRADE_FROM_TAG",
    "SNAPSHOT_E2E_SNAPSHOT_TAG",
    "SNAPSHOT_E2E_UPGRADE_CONFIG",
    "SNAPSHOT_E2E_UPGRADE_PROFILE",
    "SNAPSHOT_E2E_UPGRADE_SCENARIOS",
)


@pytest.fixture
def env(monkeypatch: pytest.MonkeyPatch) -> pytest.MonkeyPatch:
    for name in ENV:
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("SNAPSHOT_E2E_UPGRADE_FROM_TAG", "v0.1.0")
    monkeypatch.setenv("SNAPSHOT_E2E_SNAPSHOT_TAG", "v0.0.0-g1a2b3c4d")
    return monkeypatch


def test_settings_default_to_the_basic_full_upgrade(env: pytest.MonkeyPatch) -> None:
    settings = UpgradeSettings.from_env()

    assert (settings.from_tag, settings.to_tag) == ("v0.1.0", "v0.0.0-g1a2b3c4d")
    assert (settings.config, settings.profile, settings.scenarios) == ("full", "basic", ())


@pytest.mark.parametrize(
    ("name", "value", "message"),
    [
        ("SNAPSHOT_E2E_UPGRADE_FROM_TAG", "", "UPGRADE_FROM_TAG is required"),
        ("SNAPSHOT_E2E_SNAPSHOT_TAG", "", "SNAPSHOT_TAG is required"),
        ("SNAPSHOT_E2E_SNAPSHOT_TAG", "v0.1.0", "same version"),
        ("SNAPSHOT_E2E_UPGRADE_PROFILE", "nightly", "UPGRADE_PROFILE"),
    ],
)
def test_settings_reject_invalid_environment(
    env: pytest.MonkeyPatch, name: str, value: str, message: str
) -> None:
    env.setenv(name, value)

    with pytest.raises(ValueError, match=message):
        UpgradeSettings.from_env()


def test_basic_profile_selects_basic_scenarios(env: pytest.MonkeyPatch) -> None:
    selected = scenarios.selected(UpgradeSettings.from_env())

    assert [scenario.name for scenario in selected] == ["restore-pre-upgrade-snapshot-gpu", "delete-pre-upgrade-snapshot"]


def test_all_profile_includes_every_scenario(env: pytest.MonkeyPatch) -> None:
    env.setenv("SNAPSHOT_E2E_UPGRADE_PROFILE", "all")

    assert scenarios.selected(UpgradeSettings.from_env()) == list(scenarios.SCENARIOS)


def test_explicit_scenarios_override_the_profile(env: pytest.MonkeyPatch) -> None:
    env.setenv("SNAPSHOT_E2E_UPGRADE_SCENARIOS", "delete-pre-upgrade-snapshot")

    assert [s.name for s in scenarios.selected(UpgradeSettings.from_env())] == ["delete-pre-upgrade-snapshot"]


def test_unknown_scenario_is_rejected(env: pytest.MonkeyPatch) -> None:
    env.setenv("SNAPSHOT_E2E_UPGRADE_SCENARIOS", "restore-everything")

    with pytest.raises(ValueError, match="unknown upgrade scenario"):
        scenarios.selected(UpgradeSettings.from_env())


def test_scenario_names_and_run_prefixes_are_unique() -> None:
    names = [scenario.name for scenario in scenarios.SCENARIOS]
    prefixes = [scenario.run_prefix for scenario in scenarios.SCENARIOS]

    assert len(set(names)) == len(names)
    assert len(set(prefixes)) == len(prefixes)
    assert all(len(prefix) <= 24 for prefix in prefixes)


def test_full_config_moves_both_components_to_the_new_version(env: pytest.MonkeyPatch) -> None:
    settings = UpgradeSettings.from_env()
    full = configs.get("full")

    assert full.operator_tag(settings) == full.agent_tag(settings) == "v0.0.0-g1a2b3c4d"
    assert not full.reuse_values


def test_upgrade_installs_the_checked_out_chart() -> None:
    assert configs.CHART_DIR.is_absolute()
    assert (configs.CHART_DIR / "Chart.yaml").is_file()


def test_unknown_config_is_rejected() -> None:
    with pytest.raises(ValueError, match="unknown upgrade config"):
        configs.get("sideways")


@pytest.mark.parametrize(
    ("image", "tag"),
    [
        ("ghcr.io/ai-dynamo/snapshot/operator:v0.1.0", "v0.1.0"),
        ("localhost:5000/snapshot/agent:v0.0.0-g1a2b3c4d", "v0.0.0-g1a2b3c4d"),
        ("ghcr.io/ai-dynamo/snapshot/agent:v0.1.0@sha256:abc", "v0.1.0"),
    ],
)
def test_image_tag(image: str, tag: str) -> None:
    assert checks.image_tag(image) == tag


def crd(properties: dict) -> dict:
    return {
        "spec": {
            "versions": [
                {"name": "v1alpha1", "schema": {"openAPIV3Schema": {"properties": properties}}}
            ]
        }
    }


def test_missing_crd_fields_reports_fields_the_served_crd_lacks() -> None:
    expected = crd(
        {
            "status": {
                "properties": {
                    "source": {"properties": {"node": {"type": "object"}}},
                    "conditions": {"items": {"properties": {"type": {"type": "string"}}}},
                }
            }
        }
    )
    served = crd({"status": {"properties": {"conditions": {"items": {"properties": {"type": {}}}}}}})

    assert checks.missing_crd_fields(expected, served) == [
        "v1alpha1.status.source",
        "v1alpha1.status.source.node",
    ]
    assert checks.missing_crd_fields(served, expected) == []


def test_every_generated_crd_has_a_schema_to_compare() -> None:
    files = sorted(checks.CRD_DIR.glob("*.yaml"))

    assert [path.name for path in files] == [
        "nvidia.com_podsnapshotcontents.yaml",
        "nvidia.com_podsnapshots.yaml",
        "nvidia.com_snapshotjobs.yaml",
    ]
    for path in files:
        schemas = checks.crd_version_schemas(yaml.safe_load(Path(path).read_text(encoding="utf-8")))
        assert all(checks.schema_paths(schema) for schema in schemas.values()), path.name


def test_timings_summary_lists_each_phase(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    summary = tmp_path / "summary.md"
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
    timings = Timings()
    with timings.phase("PreUpgrade restore-pre-upgrade-snapshot-gpu"):
        pass
    with pytest.raises(RuntimeError), timings.phase("Upgrade (full)"):
        raise RuntimeError("helm failed")

    timings.publish("Upgrade v0.1.0 → main")

    text = summary.read_text(encoding="utf-8")
    assert "### Upgrade v0.1.0 → main" in text
    assert "| PreUpgrade restore-pre-upgrade-snapshot-gpu | 0s | ok |" in text
    assert "| Upgrade (full) | 0s | failed |" in text
    assert "| **Total** |" in text


def test_upgrade_reapplies_the_install_helm_overrides(env: pytest.MonkeyPatch) -> None:
    env.setenv("SNAPSHOT_E2E_HELM_SET", "image.agent.pullPolicy=IfNotPresent\nruntime.storageDir=/var/lib/k3s")
    calls = []
    env.setattr(configs.setup, "install_snapshot_chart", lambda **kwargs: calls.append(kwargs))
    config = k8s.E2EConfig(namespace="snapshot-e2e", release="snapshot", pvc_name="snapshot-pvc", kubeconfig=None)

    configs.get("full").apply(UpgradeContext(config=config, settings=UpgradeSettings.from_env()))

    assert calls[0]["helm_overrides"] == ["image.agent.pullPolicy=IfNotPresent", "runtime.storageDir=/var/lib/k3s"]


TO = "v0.0.0-g1a2b3c4d"


def agent_pod(agent: str, pagebroker: str | None) -> client.V1Pod:
    containers = [client.V1Container(name="agent", image=f"ghcr.io/ai-dynamo/snapshot/agent:{agent}")]
    if pagebroker:
        containers.append(
            client.V1Container(name="pagebroker", image=f"ghcr.io/ai-dynamo/snapshot/pagebroker:{pagebroker}")
        )
    return client.V1Pod(metadata=client.V1ObjectMeta(name="agent-x"), spec=client.V1PodSpec(containers=containers))


@pytest.mark.parametrize(
    ("deploys", "agent", "pagebroker", "problem"),
    [
        (True, TO, TO, None),
        (True, TO, None, "without pagebroker"),
        (True, TO, "v0.1.0", "runs pagebroker v0.1.0"),
        (False, TO, None, None),
        (False, TO, TO, "does not deploy"),
        (True, "v0.1.0", None, None),
        (False, "v0.1.0", "v0.1.0", None),
    ],
)
def test_pagebroker_matches_what_the_upgraded_chart_deploys(
    deploys: bool, agent: str, pagebroker: str | None, problem: str | None
) -> None:
    found = checks.pagebroker_problem(agent_pod(agent, pagebroker), TO, deploys)

    assert (found is None) if problem is None else (problem in found)
