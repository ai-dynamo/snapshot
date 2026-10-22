# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Bounded kubectl calls and ownership checks for the endurance controller."""

from decimal import Decimal
import json
import os
from pathlib import Path
import selectors
import signal
import subprocess
import tempfile
import time

from stress_render import OWNER, CAMPAIGN


def durable_json(path, value):
    path = Path(path)
    data = (json.dumps(value, allow_nan=False, indent=2) + "\n").encode()
    if path.is_symlink():
        raise ValueError("run records cannot be links")
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as output:
        temporary = Path(output.name)
        try:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
            os.replace(temporary, path)
            directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            temporary.unlink(missing_ok=True)


def execute(command, *, input=None, output=None, limit=2 * 1024**2, timeout=40, sensitive=False):
    # A temporary input descriptor avoids blocking on a full stdin pipe before reading output.
    with os.fdopen(os.memfd_create("stress-command-input", os.MFD_CLOEXEC), "w+b") as source:
        if input is not None:
            source.write(input)
        source.seek(0)
        process = subprocess.Popen(command, stdin=source, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   start_new_session=True)
        data, errors, size = bytearray(), bytearray(), 0
        deadline = time.monotonic() + timeout
        try:
            with selectors.DefaultSelector() as selector:
                for stream in (process.stdout, process.stderr):
                    os.set_blocking(stream.fileno(), False)
                    selector.register(stream, selectors.EVENT_READ)
                while selector.get_map():
                    if time.monotonic() >= deadline:
                        raise TimeoutError("local command exceeded its deadline")
                    for key, _ in selector.select(min(0.1, max(0, deadline - time.monotonic()))):
                        block = os.read(key.fd, 65536)
                        if not block:
                            selector.unregister(key.fileobj)
                            continue
                        if key.fileobj is process.stderr:
                            if len(errors) + len(block) > 64 * 1024:
                                raise ValueError("local command stderr exceeded its limit")
                            errors.extend(block)
                        else:
                            size += len(block)
                            if size > limit:
                                raise ValueError("local command output exceeded its limit")
                            if output is None:
                                data.extend(block)
                            else:
                                output.write(block)
            process.wait(timeout=max(0.01, deadline - time.monotonic()))
            if process.returncode:
                detail = "" if sensitive else ": " + errors.decode(errors="replace")[-1000:]
                raise RuntimeError(f"{command[0]} exited {process.returncode}" + detail)
            return bytes(data)
        except BaseException:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            raise
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)
            process.stdout.close()
            process.stderr.close()


class Kubernetes:
    def __init__(self, kubeconfig, context):
        if not context or not Path(kubeconfig).is_file():
            raise ValueError("an explicit readable kubeconfig and context are required")
        self.command = ["kubectl", "--kubeconfig", str(Path(kubeconfig).resolve()), "--context", context,
                        "--request-timeout=20s"]

    def call(self, *arguments, **options):
        return execute([*self.command, *arguments], **options)

    def get(self, kind, name=None, namespace=None, selector=None, field_selector=None):
        arguments = (["-n", namespace] if namespace else []) + ["get", kind]
        arguments += [name, "--ignore-not-found"] if name else ["--all-namespaces"] if namespace is None and kind == "pods" else []
        if selector:
            arguments += ["-l", selector]
        if field_selector:
            arguments += ["--field-selector", field_selector]
        data = self.call(*arguments, "-o", "json", sensitive=kind.lower() == "secret")
        return json.loads(data) if data.strip() else None

    def create(self, resource, sensitive=False):
        return json.loads(self.call("create", "-f", "-", "-o", "json", input=json.dumps(resource).encode(), sensitive=sensitive))

    def replace(self, resource):
        return json.loads(self.call("replace", "-f", "-", "-o", "json", input=json.dumps(resource).encode()))

    def patch(self, kind, name, namespace, value):
        return json.loads(self.call("-n", namespace, "patch", kind, name, "--type=merge", "-p", json.dumps(value), "-o", "json"))

    def delete(self, kind, name, namespace, uid):
        # Raw deletion carries an API UID precondition, including after a lost reply.
        api = "batch/v1" if kind == "Job" else "v1"
        plural = {"Pod": "pods", "Secret": "secrets", "Job": "jobs"}[kind]
        root = "/apis/" + api if "/" in api else "/api/" + api
        path = f"{root}/namespaces/{namespace}/{plural}/{name}"
        options = {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {"uid": uid},
                   "gracePeriodSeconds": 10, "propagationPolicy": "Foreground"}
        self.call("delete", "--raw", path, "-f", "-", input=json.dumps(options).encode(), sensitive=kind == "Secret")


def owned(resource, record, uid=None):
    annotations = resource.get("metadata", {}).get("annotations", {})
    if annotations.get(OWNER) != record["owner"] or annotations.get(CAMPAIGN) != record["campaign"]:
        raise ValueError("resource ownership collision; refusing to adopt or mutate it")
    if uid is not None and resource["metadata"].get("uid") != uid:
        raise ValueError("owned resource UID changed")
    return resource


def contains(actual, desired):
    if isinstance(desired, dict):
        return isinstance(actual, dict) and all(key in actual and contains(actual[key], value) for key, value in desired.items())
    if isinstance(desired, list):
        return isinstance(actual, list) and len(actual) == len(desired) and all(contains(a, b) for a, b in zip(actual, desired))
    return actual == desired


def quantity(value):
    value = str(value)
    for suffix, multiplier in (("Ki", 1024), ("Mi", 1024**2), ("Gi", 1024**3), ("Ti", 1024**4),
                               ("K", 1000), ("M", 1000**2), ("G", 1000**3), ("m", Decimal(".001")),
                               ("u", Decimal(".000001")), ("n", Decimal(".000000001"))):
        if value.endswith(suffix):
            return Decimal(value[:-len(suffix)]) * multiplier
    return Decimal(value)


def pod_request(pod, name):
    containers = pod["spec"]["containers"]
    normal = sum(quantity(c.get("resources", {}).get("requests", {}).get(name, "0")) for c in containers)
    sidecars = initial = 0
    for container in pod["spec"].get("initContainers", []):
        request = quantity(container.get("resources", {}).get("requests", {}).get(name, "0"))
        if container.get("restartPolicy") == "Always":
            sidecars += request
            initial = max(initial, sidecars)
        else:
            initial = max(initial, sidecars + request)
    declared = quantity(pod["spec"].get("resources", {}).get("requests", {}).get(name, "0"))
    return max(normal + sidecars, initial, declared) + quantity(pod["spec"].get("overhead", {}).get(name, "0"))
