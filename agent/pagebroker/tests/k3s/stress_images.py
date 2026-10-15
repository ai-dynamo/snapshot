#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Package reviewed Docker images as OCI images with explicit digest identities."""

import argparse
from copy import deepcopy
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile


DIGEST = re.compile(r"sha256:[a-f0-9]{64}\Z")
REVISION = re.compile(r"[a-f0-9]{40}\Z")
OCI = "application/vnd.oci.image."


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()


def canonical_reference(value):
    if not isinstance(value, str) or not re.fullmatch(r"[a-z0-9][a-zA-Z0-9._:/@-]{0,255}", value):
        raise ValueError("invalid image reference")
    first = value.split("/", 1)[0]
    if "/" not in value:
        value = "docker.io/library/" + value
    elif not ("." in first or ":" in first or first == "localhost"):
        value = "docker.io/" + value
    return value


def validate_identities(value, revisions=None, tags=None):
    if value.get("schemaVersion") != 1 or set(value) != {"schemaVersion", "images"} or set(value["images"]) != {"harness", "broker"}:
        raise ValueError("reviewed image identities require the harness and broker")
    for role, item in value["images"].items():
        if set(item) != {"tag", "reference", "manifestDigest", "configDigest", "revision", "platform"}:
            raise ValueError("invalid reviewed image identity fields")
        if not all(isinstance(item[key], str) and DIGEST.fullmatch(item[key]) for key in ("manifestDigest", "configDigest")):
            raise ValueError("manifest and configuration SHA-256 digests are required")
        tag = canonical_reference(item["tag"])
        repository = tag.rsplit(":", 1)[0]
        if "@" in tag or ":" not in tag.rsplit("/", 1)[-1] or item["reference"] != repository + "@" + item["manifestDigest"]:
            raise ValueError("immutable image reference does not match its reviewed manifest")
        if item["platform"] != "linux/amd64" or not REVISION.fullmatch(item["revision"]):
            raise ValueError("reviewed image platform or source revision is invalid")
        if revisions and item["revision"] != revisions[role]:
            raise ValueError("image revision differs from the declared source")
        if tags and tag != canonical_reference(tags[role]):
            raise ValueError("image tag differs from the reviewed image")
    return deepcopy(value["images"])


def load_identities(path, revisions, tags):
    if Path(path).stat().st_size > 16 * 1024:
        raise ValueError("image identity document exceeds 16 KiB")
    return validate_identities(json.loads(Path(path).read_text()), revisions, tags)


def runtime_identity(value, expected):
    # CRI may return a configuration ID or a repository-qualified manifest digest.
    raw = value.removeprefix("containerd://").removeprefix("docker://").removeprefix("docker-pullable://")
    if "@" in raw:
        if canonical_reference(raw) != expected["reference"]:
            raise ValueError("container manifest differs from the reviewed image")
        return "manifest"
    if raw != expected["configDigest"]:
        raise ValueError("container configuration differs from the reviewed image")
    return "configuration"


def package(source, output, tags, revisions):
    output = Path(output)
    output.mkdir(exist_ok=False)
    identities, descriptors, written = {}, [], set()
    with tarfile.open(source, "r:") as original, tarfile.open(output / "images.oci.tar", "w") as archive:
        def small(name):
            member = original.getmember(name)
            if member.size > 1024**2 or not member.isfile():
                raise ValueError("Docker image metadata exceeds its limit")
            with original.extractfile(member) as stream:
                return stream.read()

        def add(name, data=None, member=None):
            if name in written:
                return
            written.add(name)
            header = tarfile.TarInfo(name)
            header.size = len(data) if data is not None else member.size
            with io.BytesIO(data) if data is not None else original.extractfile(member) as stream:
                archive.addfile(header, stream)

        def blob(data, media_type):
            digest = "sha256:" + hashlib.sha256(data).hexdigest()
            add("blobs/sha256/" + digest[7:], data)
            return {"mediaType": media_type, "digest": digest, "size": len(data)}

        manifests = json.loads(small("manifest.json"))
        for role, tag in tags.items():
            normalized = canonical_reference(tag)
            matches = [item for item in manifests if normalized in [canonical_reference(t) for t in item.get("RepoTags", [])]]
            if len(matches) != 1:
                raise ValueError("Docker archive must contain exactly one image for each reviewed tag")
            selected = matches[0]
            config_data = small(selected["Config"])
            config = json.loads(config_data)
            if (config.get("os"), config.get("architecture")) != ("linux", "amd64"):
                raise ValueError("reviewed images must use linux/amd64")
            if config.get("config", {}).get("Labels", {}).get("org.opencontainers.image.revision") != revisions[role]:
                raise ValueError("archived image source label differs from the intended revision")
            if not REVISION.fullmatch(revisions[role]):
                raise ValueError("full source revisions are required")
            layers = []
            if not 0 < len(selected["Layers"]) <= 256:
                raise ValueError("invalid image layer count")
            for name in selected["Layers"]:
                member = original.getmember(name)
                if not member.isfile() or not 0 <= member.size <= 20 * 1024**3:
                    raise ValueError("invalid Docker image layer")
                digest = hashlib.sha256()
                with original.extractfile(member) as stream:
                    while block := stream.read(1024**2):
                        digest.update(block)
                value = "sha256:" + digest.hexdigest()
                add("blobs/sha256/" + value[7:], member=member)
                layers.append({"mediaType": OCI + "layer.v1.tar", "digest": value, "size": member.size})
            if config.get("rootfs", {}).get("diff_ids") != [layer["digest"] for layer in layers]:
                raise ValueError("Docker layer contents disagree with the reviewed configuration")
            configuration = blob(config_data, OCI + "config.v1+json")
            manifest = blob(encoded({"schemaVersion": 2, "mediaType": OCI + "manifest.v1+json",
                                     "config": configuration, "layers": layers}), OCI + "manifest.v1+json")
            reference = normalized.rsplit(":", 1)[0] + "@" + manifest["digest"]
            identities[role] = {"tag": normalized, "reference": reference, "manifestDigest": manifest["digest"],
                                "configDigest": configuration["digest"], "revision": revisions[role], "platform": "linux/amd64"}
            for name in (normalized, reference):
                descriptors.append(manifest | {"platform": {"os": "linux", "architecture": "amd64"},
                    "annotations": {"io.containerd.image.name": name, "org.opencontainers.image.ref.name": name}})
        add("oci-layout", encoded({"imageLayoutVersion": "1.0.0"}))
        add("index.json", encoded({"schemaVersion": 2, "mediaType": OCI + "index.v1+json", "manifests": descriptors}))
    result = {"schemaVersion": 1, "images": identities}
    validate_identities(result, revisions, tags)
    (output / "identities.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("docker-archive", "output", "client-image", "pagebroker-image", "harness-revision", "broker-revision"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    result = package(args.docker_archive, args.output, {"harness": args.client_image, "broker": args.pagebroker_image},
                     {"harness": args.harness_revision, "broker": args.broker_revision})
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
