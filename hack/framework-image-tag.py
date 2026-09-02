# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Resolve the content-addressed e2e image for a framework guide.

The framework guides under docs/guides/<framework>/ are the workloads the
framework e2e tests run. Their images are expensive to build (the runtime base
images are tens of gigabytes), so they are published once per distinct set of
image inputs rather than per commit: the tag is a digest over the Dockerfile
and the files it copies. A guide change produces a new tag; an unrelated commit
reuses the published image.

    python3 hack/framework-image-tag.py vllm            # print the image ref
    python3 hack/framework-image-tag.py vllm --check    # fail if unpublished

With --github-output the script also appends image=, tag=, and exists= lines to
$GITHUB_OUTPUT so a workflow can decide whether to build.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

GITHUB_API_VERSION = "2022-11-28"
MAX_PAGES = 5
ORG = "ai-dynamo"
REGISTRY = f"ghcr.io/{ORG}/snapshot"
TAG_LENGTH = 12

REPO_ROOT = Path(__file__).resolve().parent.parent
GUIDES_DIR = REPO_ROOT / "docs" / "guides"

# Files that end up in the image, per framework guide directory. The Dockerfile
# carries the runtime base image pin, so a base bump changes the tag too.
# Deployment manifests are deliberately excluded: they configure the Pod, not
# the image, and must not force a rebuild.
FRAMEWORKS: dict[str, tuple[str, ...]] = {
    "vllm": ("Dockerfile.vllm", "app.py"),
    "sglang": ("Dockerfile.sglang", "app.py"),
    "tensorrt-llm": ("Dockerfile.tensorrt-llm", "app.py"),
}


def image_inputs(framework: str) -> list[Path]:
    return [GUIDES_DIR / framework / name for name in FRAMEWORKS[framework]]


def dockerfile(framework: str) -> Path:
    return image_inputs(framework)[0]


def inputs_digest(framework: str) -> str:
    digest = hashlib.sha256()
    for path in image_inputs(framework):
        # Include the relative name so renaming a file changes the tag even if
        # its content does not.
        digest.update(path.name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()[:TAG_LENGTH]


def package_name(framework: str) -> str:
    return f"snapshot/e2e-{framework}"


def image_ref(framework: str) -> tuple[str, str]:
    tag = inputs_digest(framework)
    return f"{REGISTRY}/e2e-{framework}:{tag}", tag


def package_has_tag(package: str, tag: str, headers: dict[str, str]) -> bool:
    encoded = urllib.parse.quote(package, safe="")

    for page in range(1, MAX_PAGES + 1):
        url = (
            f"https://api.github.com/orgs/{ORG}/packages/container/"
            f"{encoded}/versions?per_page=100&page={page}"
        )
        request = urllib.request.Request(url, headers=headers)
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                versions = json.load(response)
        except urllib.error.HTTPError as error:
            if error.code == 404:
                # The package itself does not exist yet: nothing published.
                return False
            raise

        if not versions:
            break

        for version in versions:
            version_tags = version.get("metadata", {}).get("container", {}).get("tags", [])
            if tag in version_tags:
                return True

    return False


def github_headers() -> dict[str, str]:
    headers = {
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": GITHUB_API_VERSION,
    }
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if token:
        headers["Authorization"] = f"Bearer {token}"
    return headers


def write_github_output(values: dict[str, str]) -> None:
    github_output = os.environ.get("GITHUB_OUTPUT")
    if not github_output:
        return
    with open(github_output, "a", encoding="utf-8") as output:
        for key, value in values.items():
            output.write(f"{key}={value}\n")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("framework", choices=sorted(FRAMEWORKS))
    parser.add_argument(
        "--check",
        action="store_true",
        help="query GHCR and fail unless the image is published",
    )
    parser.add_argument(
        "--allow-missing",
        action="store_true",
        help="with --check, report instead of failing when the image is missing",
    )
    parser.add_argument(
        "--github-output",
        action="store_true",
        help="append image=, tag=, exists= (and dockerfile=, context=) to $GITHUB_OUTPUT",
    )
    args = parser.parse_args()

    missing_inputs = [str(path) for path in image_inputs(args.framework) if not path.is_file()]
    if missing_inputs:
        print(f"Missing image inputs: {', '.join(missing_inputs)}", file=sys.stderr)
        return 2

    image, tag = image_ref(args.framework)
    exists: bool | None = None
    if args.check:
        exists = package_has_tag(package_name(args.framework), tag, github_headers())

    if args.github_output:
        write_github_output(
            {
                "image": image,
                "tag": tag,
                "exists": "true" if exists else "false",
                "dockerfile": str(dockerfile(args.framework).relative_to(REPO_ROOT)),
                "context": str((GUIDES_DIR / args.framework).relative_to(REPO_ROOT)),
            }
        )

    print(image)
    if exists is False:
        message = (
            f"{image} is not published. Run the 'E2E Framework Images' workflow "
            f"for {args.framework} (it builds docs/guides/{args.framework}/) or "
            "build and push it manually."
        )
        if args.allow_missing:
            print(message, file=sys.stderr)
            return 0
        print(message, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
