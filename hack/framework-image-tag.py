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
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

GITHUB_API_VERSION = "2022-11-28"
PAGE_SIZE = 100
ORG = "ai-dynamo"
REGISTRY = f"ghcr.io/{ORG}/snapshot"
TAG_LENGTH = 12

REPO_ROOT = Path(__file__).resolve().parent.parent
GUIDES_DIR = REPO_ROOT / "docs" / "guides"

# FRAMEWORKS is the contract for tag correctness: the tag is a digest over
# exactly these files, so a file that reaches the image but is missing here
# lets CI reuse a stale image for a changed guide. Every file in a guide
# directory must be listed either here (image input) or in EXCLUDED (never
# part of the image); validate_guide_files() fails on anything unclassified,
# and validate_dockerfile_copies() fails if a Dockerfile COPY/ADD source is not
# an input. The Dockerfile carries the runtime base image pin, so a base bump
# changes the tag too.
FRAMEWORKS: dict[str, tuple[str, ...]] = {
    "vllm": ("Dockerfile.vllm", "app.py"),
    "sglang": ("Dockerfile.sglang", "app.py"),
    "tensorrt-llm": ("Dockerfile.tensorrt-llm", "app.py"),
}

# Files in a guide directory that never enter the image. Deployment manifests
# configure the Pod, not the image, and must not force a rebuild.
EXCLUDED: dict[str, tuple[str, ...]] = {
    "vllm": ("deployment.yaml", "restore-deployment.yaml"),
    "sglang": (
        "deployment.yaml",
        "restore-deployment.yaml",
        "model-cache-pvc.yaml",
    ),
    "tensorrt-llm": ("deployment.yaml", "restore-deployment.yaml"),
}

COPY_INSTRUCTION = re.compile(r"^\s*(?:COPY|ADD)\s+(?P<args>.+?)\s*$", re.IGNORECASE)


def image_inputs(framework: str) -> list[Path]:
    return [GUIDES_DIR / framework / name for name in FRAMEWORKS[framework]]


def validate_guide_files(framework: str) -> list[str]:
    """Names of files in the guide directory that are neither inputs nor excluded."""
    classified = set(FRAMEWORKS[framework]) | set(EXCLUDED[framework])
    guide_dir = GUIDES_DIR / framework
    present = {path.name for path in guide_dir.iterdir() if path.is_file()}
    return sorted(present - classified)


def validate_dockerfile_copies(framework: str) -> list[str]:
    """COPY/ADD sources in the Dockerfile that are not hash inputs.

    Sources from another build stage (--from=) do not come from the guide
    directory and are skipped. Only single-file sources are supported; a
    directory or glob would need its members enumerated in FRAMEWORKS.
    """
    inputs = set(FRAMEWORKS[framework])
    unlisted: list[str] = []
    for line in dockerfile(framework).read_text(encoding="utf-8").splitlines():
        match = COPY_INSTRUCTION.match(line)
        if not match:
            continue
        tokens = match.group("args").split()
        flags = [token for token in tokens if token.startswith("--")]
        if any(flag.startswith("--from=") for flag in flags):
            continue
        operands = [token for token in tokens if not token.startswith("--")]
        for source in operands[:-1]:  # the last operand is the destination
            if source.lstrip("./") not in inputs:
                unlisted.append(source)
    return unlisted


def dockerfile(framework: str) -> Path:
    # Named, not positional: the hash-input tuple has no ordering contract,
    # and building from the wrong file would still publish a validly tagged
    # image.
    path = GUIDES_DIR / framework / f"Dockerfile.{framework}"
    if path.name not in FRAMEWORKS[framework]:
        raise ValueError(f"{path.name} is not a hash input for {framework}")
    return path


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

    # Page until the listing is exhausted: content-addressed tags accumulate
    # (one per guide change, never pruned), so a fixed page budget would
    # eventually report a still-valid tag as unpublished.
    page = 0
    while True:
        page += 1
        url = (
            f"https://api.github.com/orgs/{ORG}/packages/container/"
            f"{encoded}/versions?per_page={PAGE_SIZE}&page={page}"
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
            return False

        for version in versions:
            version_tags = version.get("metadata", {}).get("container", {}).get("tags", [])
            if tag in version_tags:
                return True
        if len(versions) < PAGE_SIZE:
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

    # Both checks run on every invocation, so they fail the PR-time --check
    # step and not only a later build.
    unclassified = validate_guide_files(args.framework)
    if unclassified:
        print(
            f"Unclassified files in docs/guides/{args.framework}/: "
            f"{', '.join(unclassified)}. Add each to FRAMEWORKS (it is copied into "
            "the image) or to EXCLUDED (it is not) in hack/framework-image-tag.py.",
            file=sys.stderr,
        )
        return 2
    unlisted = validate_dockerfile_copies(args.framework)
    if unlisted:
        print(
            f"Dockerfile.{args.framework} copies {', '.join(unlisted)} but "
            "FRAMEWORKS does not list it as an image input; the tag would not "
            "change when it does.",
            file=sys.stderr,
        )
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
        print(message, file=sys.stderr)
        return 0 if args.allow_missing else 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
