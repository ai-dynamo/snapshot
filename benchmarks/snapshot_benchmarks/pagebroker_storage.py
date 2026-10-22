# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Owned storage setup/deletion, run as bounded children of the benchmark runner.

The SDK handles small access probes and cleanup. PageBroker transfers all workload
payloads. Never include an SDK exception body in durable results.
"""

from __future__ import annotations

import argparse
import datetime as dt
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import stat
from urllib.parse import urlsplit

AWS_KEYS = ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN")


def write_json(path: Path, value: dict) -> None:
    data = json.dumps(value, separators=(",", ":"), allow_nan=False).encode()
    if len(data) > 16384:
        raise ValueError("worker output exceeds 16 KiB")
    tmp = path.with_suffix(".tmp")
    tmp.write_bytes(data + b"\n")
    os.chmod(tmp, 0o600)
    tmp.replace(path)


def read_json(path: Path, maximum: int = 65536) -> dict:
    with path.open("rb") as f:
        data = f.read(maximum + 1)
    if len(data) > maximum:
        raise ValueError("JSON input exceeds limit")
    value = json.loads(data)
    if not isinstance(value, dict):
        raise ValueError("expected JSON object")
    return value


def safe_prefix(value: str) -> bool:
    return (isinstance(value, str) and len(value) <= 384
            and all(33 <= ord(c) <= 126 and c != "\\" for c in value)
            and all(p not in ("", ".", "..") for p in value.split("/")))


def render_s3_config(template: Path, run_id: str, output_path: Path) -> dict:
    config = read_json(template)
    allowed = {"storeId", "endpoint", "region", "bucket", "prefix", "addressing", "caFile", "allowHttp", "limits"}
    if set(config) - allowed or not re.fullmatch(r"[a-f0-9]{32}", run_id):
        raise ValueError("invalid S3 configuration fields or run ID")
    if any(not isinstance(config.get(k), str) or not config[k] for k in ("storeId", "endpoint", "region", "bucket")):
        raise ValueError("missing S3 connection fields")
    endpoint = urlsplit(config["endpoint"])
    if endpoint.username or endpoint.password or endpoint.query or endpoint.fragment or not endpoint.hostname:
        raise ValueError("invalid S3 endpoint")
    if endpoint.scheme != "https":
        try:
            local = endpoint.hostname == "localhost" or ipaddress.ip_address(endpoint.hostname).is_loopback
        except ValueError:
            local = False
        if endpoint.scheme != "http" or not config.get("allowHttp") or not local:
            raise ValueError("HTTP is allowed only for an explicitly configured loopback emulator")
    if config.get("addressing", "virtual") not in ("path", "virtual"):
        raise ValueError("invalid S3 addressing mode")
    if config.get("caFile"):
        ca = Path(config["caFile"])
        if not ca.is_absolute() or not ca.is_file():
            raise ValueError("S3 CA must be a readable absolute file")
    parent = config.get("prefix", "")
    if parent and not safe_prefix(parent):
        raise ValueError("invalid S3 parent prefix")
    run_prefix = "/".join(p for p in (parent, f"bench-{run_id}") if p)
    config.update(storeId=f"bench-{run_id}", prefix=f"{run_prefix}/data")
    if not safe_prefix(config["prefix"]):
        raise ValueError("effective S3 prefix exceeds native limits")
    limits = config.get("limits", {})
    if not isinstance(limits, dict):
        raise ValueError("S3 limits must be an object")
    write_json(output_path, config)
    return config


def validate_credentials(source: str, expiry: str | None, required_until: dt.datetime,
                         environ=None) -> dict[str, str]:
    env = os.environ if environ is None else environ
    values = {k: env[k] for k in AWS_KEYS if env.get(k)}
    if not all(values.get(k) for k in AWS_KEYS[:2]):
        raise ValueError("explicit AWS environment key pair required")
    if source == "static-environment":
        if values.get("AWS_SESSION_TOKEN") or expiry:
            raise ValueError("static mode cannot include a session token or expiry")
    elif source == "temporary-environment":
        if not values.get("AWS_SESSION_TOKEN") or not expiry:
            raise ValueError("temporary mode requires session token and explicit expiry")
        try:
            if not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})", expiry):
                raise ValueError("expiry must be RFC3339")
            expires = dt.datetime.fromisoformat(expiry.replace("Z", "+00:00"))
            if expires.utcoffset() is None or expires < required_until + dt.timedelta(minutes=15):
                raise ValueError("insufficient temporary credential lifetime")
        except (ValueError, OverflowError) as exc:
            raise ValueError("invalid or insufficient temporary credential expiry") from exc
    else:
        raise ValueError("explicit credential source required")
    return values


def child_environment(credentials: dict[str, str] | None = None) -> dict[str, str]:
    # Remove provider/endpoint overrides as well as credentials from non-SDK helpers.
    env = {k: v for k, v in os.environ.items() if not k.startswith("AWS_") and k != "BOTO_CONFIG"}
    env.update(AWS_EC2_METADATA_DISABLED="true", AWS_SHARED_CREDENTIALS_FILE="/dev/null",
               AWS_CONFIG_FILE="/dev/null", BOTO_CONFIG="/dev/null")
    env.update(credentials or {})
    return env


def sdk_client(config: dict):
    import boto3
    from botocore.config import Config

    return boto3.client("s3", endpoint_url=config["endpoint"], region_name=config["region"],
                        aws_access_key_id=os.environ["AWS_ACCESS_KEY_ID"],
                        aws_secret_access_key=os.environ["AWS_SECRET_ACCESS_KEY"],
                        aws_session_token=os.environ.get("AWS_SESSION_TOKEN"),
                        verify=config.get("caFile") or True,
                        config=Config(connect_timeout=5, read_timeout=10,
                                      retries={"mode": "standard", "total_max_attempts": 2},
                                      s3={"addressing_style": config.get("addressing", "virtual")}))


def ownership(config: dict, run_id: str) -> tuple[str, bytes]:
    expected = f"bench-{run_id}"
    prefix = config["prefix"]
    if not re.fullmatch(r"[a-f0-9]{32}", run_id) or not safe_prefix(prefix):
        raise ValueError("invalid cleanup ownership")
    parts = prefix.split("/")
    if parts[-2:] != [expected, "data"] or config["storeId"] != expected:
        raise ValueError("cleanup prefix/store does not belong to this run")
    marker = "/".join(parts[:-1]) + "/owner.json"
    body = json.dumps({"run_id": run_id, "bucket": config["bucket"], "prefix": prefix}, sort_keys=True).encode()
    return marker, body


def require_owner(client, config: dict, run_id: str) -> None:
    key, body = ownership(config, run_id)
    response = client.get_object(Bucket=config["bucket"], Key=key)
    stream = response["Body"]
    try:
        actual = stream.read(len(body) + 1)
    finally:
        stream.close()
    if actual != body:
        raise ValueError("foreign S3 ownership marker")


def list_entries(client, config: dict, *, versions: bool = False) -> list[dict]:
    """Collect a bounded list before deletion, so deleting a page cannot skip keys."""
    prefix = config["prefix"] + "/"
    operation = "list_object_versions" if versions else "list_objects_v2"
    groups = ("Versions", "DeleteMarkers") if versions else ("Contents",)
    fields = ("Key", "VersionId") if versions else ("Key",)
    markers = ("NextKeyMarker", "NextVersionIdMarker") if versions else ("NextContinuationToken",)
    pages = client.get_paginator(operation).paginate(Bucket=config["bucket"], Prefix=prefix,
                                                    PaginationConfig={"PageSize": 1000})
    entries, seen = [], set()
    for number, page in enumerate(pages, 1):
        if number > 1024:
            raise ValueError("S3 pagination exceeded limit")
        for group in groups:
            for item in page.get(group, []):
                if len(entries) >= 50000 or not item["Key"].startswith(prefix):
                    raise ValueError("S3 listing exceeds owned bounds")
                entries.append({field: item[field] for field in fields})
        if page.get("IsTruncated"):
            # The SDK can stop silently on a truncated page with absent markers.
            token = tuple(page.get(marker) for marker in markers)
            if not token[0] or token in seen:
                raise ValueError("S3 pagination did not advance")
            seen.add(token)
    return entries


def require_no_uploads(client, config: dict) -> None:
    result = client.list_multipart_uploads(Bucket=config["bucket"], Prefix=config["prefix"] + "/", MaxUploads=1)
    if result.get("Uploads") or result.get("IsTruncated"):
        raise ValueError("incomplete multipart upload remains; retain prefix for investigation")


def delete_entries(client, bucket: str, entries: list[dict]) -> None:
    for i in range(0, len(entries), 1000):
        response = client.delete_objects(Bucket=bucket, Delete={"Objects": entries[i:i + 1000], "Quiet": True})
        if response.get("Errors"):
            raise ValueError("S3 returned per-object deletion errors")


def s3_cleanup(config: dict, run_id: str, client=None) -> dict:
    client = client or sdk_client(config)
    require_owner(client, config, run_id)
    require_no_uploads(client, config)
    versioned = client.get_bucket_versioning(Bucket=config["bucket"]).get("Status") in ("Enabled", "Suspended")
    entries = list_entries(client, config, versions=versioned)
    delete_entries(client, config["bucket"], entries)
    if list_entries(client, config) or (versioned and list_entries(client, config, versions=True)):
        raise ValueError("S3 publication data remains after deletion")
    require_no_uploads(client, config)
    return {"deleted_entries": len(entries), "empty_verified": True}


def s3_preflight(config: dict, run_id: str, client=None) -> dict:
    client = client or sdk_client(config)
    key, body = ownership(config, run_id)
    parent = config.copy()
    parent["prefix"] = key.rsplit("/", 1)[0]
    if list_entries(client, parent) or list_entries(client, parent, versions=True):
        raise ValueError("run prefix is already owned or contains data")
    require_no_uploads(client, parent)
    try:
        client.put_object(Bucket=config["bucket"], Key=key, Body=body, IfNoneMatch="*")
    except Exception:
        # A lost successful reply is admissible only with the exact marker we wrote.
        require_owner(client, config, run_id)
    require_owner(client, config, run_id)
    client.put_object(Bucket=config["bucket"], Key=config["prefix"] + "/access-probe", Body=b"benchmark access probe")
    result = s3_cleanup(config, run_id, client)
    result["versioning"] = client.get_bucket_versioning(Bucket=config["bucket"]).get("Status", "Disabled")
    return result


def filesystem_cleanup(publication_path: Path, owned_root: Path) -> dict:
    root = owned_root.resolve(strict=True)
    # Canonical equality rejects symlink roots and publications before mutation.
    if owned_root != root or publication_path.parent != root or publication_path.is_symlink():
        raise ValueError("filesystem cleanup escapes owned root")
    if not publication_path.is_dir() or publication_path.resolve() != publication_path:
        raise ValueError("publication is missing or not an owned directory")
    for current, dirs, _ in os.walk(publication_path, followlinks=False):
        p = Path(current)
        if stat.S_ISLNK(p.lstat().st_mode):
            raise ValueError("unexpected publication symlink")
        p.chmod(0o700)
        for name in dirs:
            child = p / name
            if not child.is_symlink():
                child.chmod(0o700)
    shutil.rmtree(publication_path)
    if os.path.lexists(publication_path):
        raise ValueError("filesystem publication remains")
    return {"empty_verified": True}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--request", required=True, type=Path)
    parser.add_argument("--report", required=True, type=Path)
    args = parser.parse_args()
    request = read_json(args.request)
    result = {k: request[k] for k in ("run_id", "cycle_id", "backend", "action")}
    try:
        if request["backend"] == "s3":
            config = read_json(Path(request["config"]))
            operation = {"preflight": s3_preflight, "cleanup": s3_cleanup}[request["action"]]
            result.update(operation(config, request["run_id"]))
        elif request["backend"] == "filesystem" and request["action"] == "cleanup":
            result.update(filesystem_cleanup(Path(request["publication"]), Path(request["owned_root"])))
        else:
            raise ValueError("unsupported storage action")
        result["status"] = "success"
    except Exception as exc:
        # ValueErrors here contain only our own fixed diagnostics. SDK errors may
        # include URLs, signed headers or credential identifiers: retain type only.
        result.update(status="failed", error=str(exc) if isinstance(exc, ValueError) else type(exc).__name__)
    write_json(args.report, result)
    return 0 if result["status"] == "success" else 1


if __name__ == "__main__":
    raise SystemExit(main())
