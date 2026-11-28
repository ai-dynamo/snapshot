# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Real checkpoint/restore with one canary and concurrent consumers."""

from dataclasses import replace
import time

from kubernetes import client
import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap


def read_snapshot(namespace, name):
    return client.CustomObjectsApi().get_namespaced_custom_object(
        snap.GROUP, snap.VERSION, namespace, snap.PODSNAPSHOTS, name
    )


def wait_phase(namespace, name, phase, timeout=600):
    def check():
        obj = read_snapshot(namespace, name)
        current = obj.get("status", {}).get("phase")
        if current == phase:
            return obj
        if current in {"Failed", "VerificationFailed"}:
            raise AssertionError(obj["status"])
        return None

    return snap.wait_for(f"{name} phase {phase}", check, timeout)


@pytest.mark.cpu
@pytest.mark.snapshot_success
@pytest.mark.parametrize("outcome", ["success", "damaged", "timeout"])
def test_restore_verification_controls_scale_out(config, run, outcome):
    canary_name = run.restore_pod + "-canary"
    consumer_names = [run.restore_pod, run.restore_pod + "-second"]
    try:
        k8s.create_pod(snap.source_pod(config=config, run=run, gpu=False, hooks_server=True))
        source = snap.wait_for_pod_ready(config.namespace, run.source_pod)
        snap.wait_for_file(config.namespace, run.source_pod, snap.SOURCE_READY)
        observations = snap.wait_for_state_observations(
            config.namespace, run.source_pod, run.source_token, gpu=False, minimum=2
        )
        canary = snap.restore_pod(
            config=config, run=replace(run, restore_pod=canary_name),
            gpu=False, source_node=source.spec.node_name,
        )
        main = canary["spec"]["containers"][0]
        # Check a live endpoint of the restored application, not default readiness.
        main["readinessProbe"] = {
            "httpGet": {"path": snap.HOOKS_STATUS_PATH, "port": snap.HOOKS_PORT},
            "periodSeconds": 1, "failureThreshold": 1,
        }
        # Hold the canary before its destination exists. This lets the damaged
        # case corrupt only this test's artifact before any restore can begin.
        canary["spec"]["initContainers"] = [{
            "name": "canary-start", "image": main["image"],
            "command": ["sh", "-c", "until test -f /snapshot-control/allow-canary; do sleep 1; done"],
            "volumeMounts": [{"name": "snapshot-control", "mountPath": snap.CONTROL_DIR, "subPath": "main"}],
        }]
        created = k8s.create_pod(canary)
        for name in consumer_names:
            k8s.create_pod(snap.restore_pod(
                config=config, run=replace(run, restore_pod=name),
                gpu=False, source_node=source.spec.node_name,
            ))
        snap.create_podsnapshot(
            config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid,
            restore_verification={
                "podRef": {"name": canary_name, "uid": created.metadata.uid},
                "observationWindowSeconds": 20, "timeoutSeconds": 60 if outcome == "timeout" else 600,
            },
        )
        _, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)
        wait_phase(config.namespace, run.snapshot_name, "RestoreVerifying")
        for name in consumer_names:
            snap.wait_for_restored_condition(config.namespace, name, "False", "VerificationPending")
        if outcome == "damaged":
            snap.corrupt_checkpoint_image(config, source.spec.node_name, content["metadata"]["uid"])
        if outcome != "timeout":
            snap.wait_for(
                "canary init container running",
                lambda: any(s.name == "canary-start" and s.state.running
                            for s in k8s.read_pod(config.namespace, canary_name).status.init_container_statuses or []) or None,
                120,
            )
            k8s.exec_command(config.namespace, canary_name, "touch /snapshot-control/allow-canary", container="canary-start")

        if outcome != "success":
            failed = wait_phase(config.namespace, run.snapshot_name, "VerificationFailed", timeout=180)
            assert snap.condition(failed, "Ready")["status"] == "True"
            assert failed["status"]["restoreVerifyInfo"]["outcome"] == "Failed"
            for name in consumer_names:
                snap.wait_for_restored_condition(config.namespace, name, "False", "RestoreFailed")
                events = [e for e in k8s.list_events(config.namespace)
                          if e.involved_object and e.involved_object.name == name]
                assert not any(e.reason == "RestoreStarted" for e in events)
            return

        snap.wait_for_restored_condition(config.namespace, canary_name, "True", "RestoreSucceeded")
        # Keep checking consumers through the observation window. A post-read
        # snapshot check avoids a race with the successful approval write.
        deadline = time.monotonic() + 90
        observed_window = False
        while time.monotonic() < deadline:
            consumers = [k8s.read_pod(config.namespace, name) for name in consumer_names]
            obj = read_snapshot(config.namespace, run.snapshot_name)
            phase = obj["status"]["phase"]
            if phase == "Completed":
                break
            assert phase == "RestoreVerifying", obj["status"]
            observed_window = True
            for pod in consumers:
                cond = snap.pod_condition(pod, snap.RESTORED_CONDITION)
                assert cond is None or cond.reason == "VerificationPending", cond
            time.sleep(0.5)
        else:
            raise AssertionError("verification did not complete")
        assert observed_window, "test must observe the healthy window before approval"
        info = obj["status"]["restoreVerifyInfo"]
        assert info["contentUID"] == content["metadata"]["uid"]
        assert info["podRef"]["uid"] == created.metadata.uid
        assert info["outcome"] == "Succeeded"
        for name in consumer_names:
            snap.wait_for_restored_condition(config.namespace, name, "True", "RestoreSucceeded")
            snap.wait_for_state_observations(
                config.namespace, name, run.source_token, gpu=False, minimum=observations + 2,
            )
        k8s.delete_pod(config.namespace, canary_name)
        snap.wait_for_pod_deleted(config.namespace, canary_name)
        assert read_snapshot(config.namespace, run.snapshot_name)["status"]["restoreVerifyInfo"] == info
    except Exception:
        snap.debug_dump(config, run)
        raise
    finally:
        for name in [canary_name, consumer_names[1]]:
            k8s.delete_pod(config.namespace, name)
