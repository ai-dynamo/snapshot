#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Render an isolated smoke Job; run with the e2e project's pinned dependencies."""

import argparse
import json
from pathlib import Path
import re
from urllib.parse import urlsplit

import yaml

from monitor import validate_timing


TEMPLATES = Path(__file__).resolve().parent
DNS_LABEL = re.compile(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\Z")


def storage_config(args):
    if not args.bucket or not args.region:
        raise ValueError("S3 mode requires --bucket and --region")
    endpoint = args.endpoint or f"https://s3.{args.region}.amazonaws.com"
    parsed = urlsplit(endpoint)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path:
        raise ValueError("--endpoint must be an HTTPS origin without credentials or a path")
    prefix = f"{args.prefix}/{args.namespace}" if args.prefix else args.namespace
    if len(prefix) > 384 or any(part in ("", ".", "..") for part in prefix.split("/")) or "\\" in prefix:
        raise ValueError("--prefix must be a canonical relative S3 key prefix")
    if any(ord(character) < 33 or ord(character) > 126 for character in prefix):
        raise ValueError("--prefix must use printable ASCII without spaces")
    return {
        "storeId": args.namespace, "endpoint": endpoint, "region": args.region,
        "bucket": args.bucket, "prefix": prefix, "addressing": "virtual",
        "limits": {"transactionSeconds": 600, "stagingBytes": 1024**3, "activeTransactions": 4,
                   "uploadPartBytes": 8 * 1024**2, "uploadBufferBytes": 1024**3,
                   "uploadActiveFiles": 2, "uploadTargetGbps": 1,
                   "requestSeconds": 30, "uploadConnectSeconds": 5, "uploadRequestRetries": 2},
    }


def render(args):
    validate_timing(args.sample_interval, args.settle_seconds)
    if not DNS_LABEL.fullmatch(args.namespace) or not args.namespace.startswith("pagebroker-smoke-"):
        raise ValueError("--namespace must be a DNS label starting with pagebroker-smoke-")
    if args.mode not in ("filesystem", "s3"):
        raise ValueError("--mode must be filesystem or s3")
    if not DNS_LABEL.fullmatch(args.credentials_secret):
        raise ValueError("--credentials-secret must be a DNS label")
    for image in (args.client_image, args.pagebroker_image):
        if not image or any(character.isspace() for character in image):
            raise ValueError("explicit client and PageBroker image references are required")
    job = yaml.safe_load((TEMPLATES / "job.yaml").read_text())
    claim, exporter = yaml.safe_load_all((TEMPLATES / "results.yaml").read_text())
    labels = {"app.kubernetes.io/name": "pagebroker-smoke", "app.kubernetes.io/instance": args.namespace}
    for resource in (job, claim, exporter):
        resource["metadata"].update(namespace=args.namespace, labels=dict(labels))
    job["spec"]["template"]["metadata"] = {"labels": dict(labels)}
    pod = job["spec"]["template"]["spec"]
    broker, client = pod["containers"]
    client["args"] = ["--sample-interval", str(args.sample_interval), "--settle-seconds", str(args.settle_seconds)]
    broker["image"], client["image"] = args.pagebroker_image, args.client_image
    exporter["spec"]["containers"][0]["image"] = args.client_image
    for container in [broker, client, exporter["spec"]["containers"][0]]:
        container["imagePullPolicy"] = args.image_pull_policy
    client["env"][0]["value"] = args.mode
    claim["spec"]["storageClassName"] = args.storage_class
    streamer = {
        "filesystem": {"strategies": ["sync_buffered"]}, "readChunkBytes": 8 * 1024**2,
        "s3Reader": {"concurrency": 2, "maxConnections": 2, "maxInflightMiB": 16},
        "logging": {"level": "ERROR", "toStderr": True},
    }
    config = {"apiVersion": "v1", "kind": "ConfigMap",
              "metadata": {"name": "pagebroker-smoke-config", "namespace": args.namespace, "labels": dict(labels)},
              "data": {"streamer.json": json.dumps(streamer, indent=2)}}
    if args.mode == "s3":
        storage = storage_config(args)
        config["data"]["storage.json"] = json.dumps(storage, indent=2)
        broker["args"].extend(["--storage-config", "/config/storage.json"])
        broker["env"] = [
            {"name": "AWS_SHARED_CREDENTIALS_FILE", "value": "/credentials/credentials"},
            {"name": "AWS_CONFIG_FILE", "value": "/dev/null"},
            {"name": "AWS_PROFILE", "value": "default"},
            {"name": "AWS_EC2_METADATA_DISABLED", "value": "true"},
        ]
        broker["volumeMounts"].append({"name": "credentials", "mountPath": "/credentials", "readOnly": True})
        pod["volumes"].append({"name": "credentials", "secret": {"secretName": args.credentials_secret,
                              "defaultMode": 0o440, "items": [{"key": "credentials", "path": "credentials"}]}})
        client["env"].append({"name": "PAGEBROKER_SMOKE_STORE_ID", "value": storage["storeId"]})
    if args.node:
        for spec in (pod, exporter["spec"]):
            spec["nodeSelector"] = {"kubernetes.io/hostname": args.node}
    namespace = {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": args.namespace, "labels": dict(labels)}}
    return [namespace, claim, config], job, exporter


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("filesystem", "s3"), required=True)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--client-image", required=True)
    parser.add_argument("--pagebroker-image", required=True)
    parser.add_argument("--storage-class", default="local-path")
    parser.add_argument("--image-pull-policy", choices=("Never", "IfNotPresent", "Always"), default="Never")
    parser.add_argument("--node", help="Value of the target node's kubernetes.io/hostname label")
    parser.add_argument("--sample-interval", type=float, default=1)
    parser.add_argument("--settle-seconds", type=float, default=5)
    parser.add_argument("--bucket")
    parser.add_argument("--region")
    parser.add_argument("--endpoint")
    parser.add_argument("--prefix", default="pagebroker-smoke")
    parser.add_argument("--credentials-secret", default="pagebroker-smoke-aws")
    parser.add_argument("--output", type=Path, required=True, help="New directory for rendered manifests")
    args = parser.parse_args()
    try:
        resources, job, exporter = render(args)
        args.output.mkdir(parents=True, exist_ok=False)
        for name, documents in (("resources.yaml", resources), ("job.yaml", [job]), ("export.yaml", [exporter])):
            (args.output / name).write_text(yaml.safe_dump_all(documents, sort_keys=False))
        print(f"Rendered {args.mode} Job in namespace {args.namespace}: {args.output}")
    except (ValueError, OSError) as error:
        parser.error(str(error))


if __name__ == "__main__":
    main()
