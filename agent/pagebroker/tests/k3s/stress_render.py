#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Render endurance resources without changing the smoke templates or Helm chart."""

import argparse
from copy import deepcopy
import json
from pathlib import Path
import re
from types import SimpleNamespace
import uuid

import yaml

import render
from stress_config import RunSpec


OWNER = "snapshot.nvidia.com/stress-owner"
CAMPAIGN = "snapshot.nvidia.com/stress-run"
JOB = "pagebroker-stress"
CONFIG = "pagebroker-stress-config"
RECORD = "pagebroker-stress-run"
RESULTS = "pagebroker-stress-results"
SECRET = "pagebroker-stress-aws"


def render_stress(spec, namespace, campaign, owner, client_image, broker_image, node,
                  storage_class="local-path", ca_pem=None):
    spec.validate()
    for value in (owner, campaign):
        if str(uuid.UUID(value)) != value:
            raise ValueError("ownership and campaign IDs must be canonical UUIDs")
    if not render.DNS_LABEL.fullmatch(namespace) or not namespace.startswith("pagebroker-stress-"):
        raise ValueError("endurance namespaces must start with pagebroker-stress-")
    if not node or not storage_class:
        raise ValueError("an explicit node and storage class are required")
    for image in (client_image, broker_image):
        if "://" in image or ("@" in image and not re.fullmatch(r".+@sha256:[a-f0-9]{64}", image)):
            raise ValueError("image references cannot contain URLs or credentials")
    if (spec.socket, spec.staging, spec.storage, spec.results, spec.control) != (
            "/shared/pb.sock", "/staging", "/storage", "/results", "/control"):
        raise ValueError("container run specifications must use the dedicated volume paths")
    options = SimpleNamespace(mode="filesystem", namespace="pagebroker-smoke-template", client_image=client_image,
                              pagebroker_image=broker_image, node=node, storage_class=storage_class,
                              image_pull_policy="Never", sample_interval=1, settle_seconds=5,
                              credentials_secret=SECRET)
    resources, job, exporter = render.render(options)
    names = {"pagebroker-smoke-template": namespace, "pagebroker-smoke": JOB,
             "pagebroker-smoke-config": CONFIG, "pagebroker-smoke-results": RESULTS}
    labels = {"app.kubernetes.io/name": JOB, "app.kubernetes.io/instance": namespace, CAMPAIGN: campaign}
    annotations = {OWNER: owner, CAMPAIGN: campaign}
    for resource in [*resources, job, exporter]:
        metadata = resource["metadata"]
        metadata.update(name=names[metadata["name"]], labels=dict(labels), annotations=dict(annotations))
        if resource["kind"] != "Namespace":
            metadata["namespace"] = namespace
    claim, config = resources[1:]
    claim["spec"]["resources"]["requests"]["storage"] = "2Gi"
    config["data"]["run-spec.json"] = json.dumps(spec.as_dict(), allow_nan=False)
    job["spec"].update(activeDeadlineSeconds=int(spec.timing.job), backoffLimit=0)
    if job["spec"]["activeDeadlineSeconds"] != spec.timing.job:
        raise ValueError("the Kubernetes Job deadline must be an integer number of seconds")
    job["spec"]["template"]["metadata"] = {"labels": dict(labels), "annotations": dict(annotations)}
    pod = job["spec"]["template"]["spec"]
    pod["terminationGracePeriodSeconds"] = 90
    broker, client = pod["containers"]
    for volume in pod["volumes"]:
        if "configMap" in volume:
            volume["configMap"]["name"] = CONFIG
        if "persistentVolumeClaim" in volume:
            volume["persistentVolumeClaim"]["claimName"] = RESULTS
    pod["volumes"].append({"name": "control", "emptyDir": {"sizeLimit": "16Mi"}})
    client.update(command=["python3", "/opt/stress/stress.py"], args=["--spec", "/config/run-spec.json"])
    client.pop("env", None)
    client["volumeMounts"].extend([{"name": "control", "mountPath": "/control"},
                                   {"name": "config", "mountPath": "/config", "readOnly": True}])
    supervisor = deepcopy(client)
    supervisor.update(name="supervisor", command=["python3", "/opt/stress/stress_supervisor.py"])
    supervisor["volumeMounts"] = [mount for mount in supervisor["volumeMounts"] if mount["name"] not in ("staging", "storage")]
    supervisor["resources"] = {"requests": {"cpu": "100m", "memory": "64Mi"},
                               "limits": {"cpu": "250m", "memory": "256Mi"}}
    pod["containers"].append(supervisor)
    if spec.mode == "s3":
        helper = deepcopy(supervisor)
        helper.update(name="s3-helper", command=["python3", "/opt/stress/s3_cleanup.py"])
        helper["volumeMounts"] = [dict(mount, readOnly=True) if mount["name"] == "results" else mount
                                  for mount in helper["volumeMounts"] if mount["name"] != "socket"]
        helper["resources"] = {"requests": {"cpu": "100m", "memory": "128Mi"},
                               "limits": {"cpu": "500m", "memory": "512Mi"}}
        names = ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
        temporary = ("AWS_SESSION_TOKEN", "AWS_CREDENTIAL_EXPIRATION")
        if spec.s3.credential_provider == "temporary-environment":
            names += temporary
        environment = [{"name": name, "valueFrom": {"secretKeyRef": {"name": SECRET, "key": name}}} for name in names]
        # Name-only entries override image defaults with empty strings. Kubernetes
        # omits empty value fields, so use its canonical form for ownership checks.
        environment.append({"name": "AWS_SECURITY_TOKEN"})
        if spec.s3.credential_provider == "static-environment":
            environment.extend({"name": name} for name in temporary)
        environment.extend({"name": name, "value": value} for name, value in (
            ("AWS_EC2_METADATA_DISABLED", "true"), ("AWS_CONFIG_FILE", "/dev/null"),
            ("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")))
        if ca_pem is not None:
            if (len(ca_pem.encode()) > 64 * 1024 or not re.fullmatch(
                    r"(?:-----BEGIN CERTIFICATE-----\s+[A-Za-z0-9+/=\r\n]+-----END CERTIFICATE-----\s*)+", ca_pem)):
                raise ValueError("CA input must be a bounded public PEM certificate bundle")
            config["data"]["ca.pem"] = ca_pem
            environment.append({"name": "AWS_CA_BUNDLE", "value": "/config/ca.pem"})
        broker["env"], helper["env"] = deepcopy(environment), deepcopy(environment)
        storage = {"storeId": spec.store_id, "bucket": spec.s3.bucket, "prefix": spec.s3.prefix,
                   "region": spec.s3.region, "endpoint": spec.s3.endpoint, "addressing": spec.s3.addressing,
                   "limits": {"transactionSeconds": 600, "stagingBytes": 1024**3, "activeTransactions": 4,
                              "uploadPartBytes": 8 * 1024**2, "uploadBufferBytes": 1024**3,
                              "uploadActiveFiles": 2, "uploadTargetGbps": 1, "requestSeconds": 30,
                              "uploadConnectSeconds": 5, "uploadRequestRetries": 2}}
        if ca_pem is not None:
            storage["caFile"] = "/config/ca.pem"
        config["data"]["storage.json"] = json.dumps(storage)
        broker["args"].extend(["--storage-config", "/config/storage.json"])
        pod["containers"].append(helper)
    exporter["spec"].update(activeDeadlineSeconds=600, terminationGracePeriodSeconds=10)
    exporter["spec"]["volumes"][0]["persistentVolumeClaim"]["claimName"] = RESULTS
    exporter["spec"]["volumes"].append({"name": "config", "configMap": {"name": CONFIG}})
    export_container = exporter["spec"]["containers"][0]
    export_container["volumeMounts"].append({"name": "config", "mountPath": "/config", "readOnly": True})
    export_container["command"] = ["python3", "/opt/stress/stress_export.py", "--spec", "/config/run-spec.json", "serve"]
    return resources, job, exporter


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("spec", "namespace", "campaign", "owner", "client-image", "broker-image", "node", "output"):
        parser.add_argument("--" + option, required=True)
    parser.add_argument("--storage-class", default="local-path")
    parser.add_argument("--ca-bundle", type=Path)
    args = parser.parse_args()
    if args.ca_bundle and args.ca_bundle.stat().st_size > 64 * 1024:
        parser.error("CA bundle exceeds 64 KiB")
    resources, job, exporter = render_stress(RunSpec.load(args.spec), args.namespace, args.campaign, args.owner,
                                            args.client_image, args.broker_image, args.node, args.storage_class,
                                            args.ca_bundle.read_text() if args.ca_bundle else None)
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=False)
    for name, documents in (("resources.yaml", resources), ("job.yaml", [job]), ("export.yaml", [exporter])):
        (output / name).write_text(yaml.safe_dump_all(documents, sort_keys=False))


if __name__ == "__main__":
    main()
