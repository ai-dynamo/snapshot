# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Collect measured clocks and inference checks through the existing e2e helpers."""

import json
import shlex
import subprocess
import sys
from pathlib import Path

from kubernetes import client
from snapshot_e2e import k8s, lifecycle

from snapshot_benchmarks import logs
from snapshot_benchmarks.timing import restore_metrics, seconds


def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(client.ApiClient().sanitize_for_serialization(value), indent=2)
        + "\n"
    )


def remote_json(namespace, pod, script, *, container="main"):
    # Kubernetes' preloaded exec stream does not expose the remote exit status.
    # Require a complete result marker, printed only after the script succeeds.
    command = "python3 -c " + shlex.quote(script)
    text = k8s.exec_command(namespace, pod, command, container=container)
    records = [
        line.removeprefix("BENCHMARK_JSON ")
        for line in text.splitlines()
        if line.startswith("BENCHMARK_JSON ")
    ]
    if len(records) != 1:
        raise RuntimeError("Remote benchmark collection failed: " + text[-1500:])
    return json.loads(records[0])


def source_measurement(cfg, pod, folder):
    packet = remote_json(
        cfg.workload_namespace,
        pod.metadata.name,
        """
import json
from pathlib import Path
root = Path('/compile-cache/startup-spans')
files = sorted(root.glob('*.jsonl'))
if not files:raise RuntimeError('No timing span files')
text = '\\n'.join(p.read_text() for p in files)
print('BENCHMARK_JSON ' + json.dumps(dict(log=text)))
""",
    )
    text = (
        k8s.pod_logs(
            cfg.workload_namespace, pod.metadata.name, tail_lines=None, container="main"
        )
        + "\n"
        + packet["log"]
    )
    (folder / "startup.log").write_text(text)
    marks = [
        json.loads(line.split("COLD_BENCH ", 1)[1])
        for line in text.splitlines()
        if "COLD_BENCH " in line
    ]
    for stage in (
        "process_start",
        "engine_init_begin",
        "engine_init_end",
        "first_inference_complete",
    ):
        if sum(m["stage"] == stage for m in marks) != 1:
            raise ValueError("Missing or ambiguous startup marker: " + stage)
    checks = [
        json.loads(line.split("CR_SEMANTIC_PRECHECK ", 1)[1])
        for line in text.splitlines()
        if "CR_SEMANTIC_PRECHECK " in line
    ]
    if sorted(c["expected"] for c in checks) != ["berlin", "paris", "tokyo"] or not all(
        c["expected"] in c["answer"].lower() for c in checks
    ):
        raise ValueError("Source inference checks failed")
    events = [
        json.loads(line.split("CR_PHASE ", 1)[1])
        for line in text.splitlines()
        if "CR_PHASE " in line
    ]
    tokens = {event["checkpoint_token"] for event in events}
    if len(tokens) != 1:
        raise ValueError("Missing or inconsistent source process token")
    save(folder / "source-inference.json", checks)
    save(folder / "measurement.json", dict(success=True, markers=marks))
    save(
        folder / "cold-runs.json",
        dict(cases=[dict(case="source", directory=str(folder))]),
    )
    analysis = Path(__file__).parent / "analysis"
    with (folder / "analysis.log").open("w") as stream:
        for script in (
            "summarize_spans.py",
            "phase_inventory.py",
            "partition_phases.py",
        ):
            subprocess.run(
                [sys.executable, str(analysis / script), str(folder)],
                stdout=stream,
                stderr=subprocess.STDOUT,
                check=True,
            )
    measured = json.loads((folder / "phase-accounting.json").read_text())[0]
    return measured, tokens.pop()


def agent_log(cfg, node, path):
    name = lifecycle.checkpoint_agent_pod(cfg.snapshot_e2e_config(), node)
    text = k8s.pod_logs(
        cfg.snapshot_namespace, name, tail_lines=None, container="agent"
    )
    path.write_text(text)
    return text


def checkpoint_measurement(cfg, node, uid, content_name, folder):
    agent = lifecycle.checkpoint_agent_pod(cfg.snapshot_e2e_config(), node)
    root = lifecycle.checkpoint_artifact_path(uid)
    scan = f"""
import json,stat
from pathlib import Path
root=Path({shlex.split(root)[0]!r})
files=[]
for path in root.rglob('*'):
    s=path.lstat()
    if stat.S_ISREG(s.st_mode):files.append(dict(path=str(path.relative_to(root)),bytes=s.st_size))
if not files:raise RuntimeError('Empty checkpoint artifact')
print('BENCHMARK_JSON '+json.dumps(dict(files=files,total_bytes=sum(f['bytes'] for f in files))))
"""
    inventory = remote_json(cfg.snapshot_namespace, agent, scan, container="agent")
    save(folder / "artifacts.json", inventory)
    if not any(
        len(parts := Path(f["path"]).parts) == 3
        and parts[0] == "gpu"
        and parts[1].isdigit()
        and parts[2] == "manifest.txt"
        for f in inventory["files"]
    ):
        raise ValueError("Checkpoint lacks PageBroker CustomStorage GPU manifests")
    text = agent_log(cfg, node, folder / "checkpoint-agent.log")
    payloads = []
    for line in text.splitlines():
        if "Checkpoint timing summary" not in line or "{" not in line:
            continue
        payload = json.loads(line[line.index("{") :])
        if payload.get("content") == content_name:
            payloads.append(payload["checkpoint"])
    if len(payloads) != 1:
        raise ValueError("Missing or ambiguous checkpoint timing summary")
    save(folder / "checkpoint-timing.json", payloads[0])
    return inventory["total_bytes"], seconds(payloads[0]["duration"])


def restored_measurement(cfg, pod, snapshot_name, token, folder):
    packet = remote_json(
        cfg.workload_namespace,
        pod.metadata.name,
        """
import json,time,urllib.request
from pathlib import Path
checks=[]
for prompt,expected in [('The capital city of France is','paris'),('The capital city of Germany is','berlin'),('The capital city of Japan is','tokyo')]:
    request=urllib.request.Request('http://127.0.0.1:8000/generate',data=json.dumps(dict(prompt=prompt)).encode(),headers={'Content-Type':'application/json'})
    with urllib.request.urlopen(request,timeout=120) as response:result=json.load(response)
    checks.append(dict(expected=expected,response=result))
phases=Path('/snapshot-control/phase-events.jsonl').read_text()
print('BENCHMARK_JSON '+json.dumps(dict(checks=checks,phases=phases)))
""",
    )
    save(folder / "inference.json", packet["checks"])
    (folder / "phases.jsonl").write_text(packet["phases"])
    if not all(
        c["expected"] in c["response"]["text"].lower()
        and c["response"].get("checkpoint_token") == token
        for c in packet["checks"]
    ):
        raise ValueError("Restore inference or process-token check failed")
    save(folder / "pod-final.json", pod)
    text = agent_log(cfg, pod.spec.node_name, folder / "agent.log")
    payload = logs.find_restore_summary_json(
        text,
        pod=cfg.workload_namespace + "/" + pod.metadata.name,
        snapshot=snapshot_name,
    )
    if payload is None:
        raise ValueError("Missing restore timing summary")
    save(folder / "timing.json", payload["restore"])
    metrics = restore_metrics(folder)
    save(folder / "metrics.json", metrics)
    return metrics
