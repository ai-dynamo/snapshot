<!--
SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
-->

# Release Process

How Snapshot releases are versioned, who can cut one, and what happens when they
do.

## Versioning

Snapshot follows [Semantic Versioning](https://semver.org/): `vMAJOR.MINOR.PATCH`,
with optional pre-release suffixes such as `v0.1.0-rc2`. The release workflow
validates the tag against `^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?$` and fails
on anything that does not match.

Snapshot is pre-1.0. As noted in the [README](README.md), the APIs may still
change, so minor versions can carry breaking changes until 1.0. Release
candidates (`-rcN`) are published ahead of a stable release for integration
testing.

Snapshot releases monthly. The maintainers cut a release once a month from
whatever has landed on `main`; a month with nothing user-visible to ship can be
skipped rather than padded. Release candidates precede any release that changes
the CRDs or the checkpoint or restore contract, so integrators have a version to
test against before the stable tag.

## Who can cut a release

Maintainers listed in [MAINTAINERS.md](MAINTAINERS.md). Publishing a release
requires write access to the repository, so contributors cannot trigger one.
The decision to cut a release is made by maintainer consensus, per
[GOVERNANCE.md](GOVERNANCE.md).

## Tagging and branches

Releases are cut from `main`. Patch releases for a published line are cut from
that line's maintenance branch, such as `release/0.1`. Only the most recent line
receives security fixes; see
[Supported versions](SECURITY.md#supported-versions).

Every release produces four tags. You create one — the rest are automatic:

| Tag | Created by | Why |
| --- | --- | --- |
| `vX.Y.Z` | the maintainer, when publishing the release | The release itself |
| `api/vX.Y.Z` | the `Release` workflow | Go module proxy resolution |
| `operator/vX.Y.Z` | the `Release` workflow | Go module proxy resolution |
| `agent/vX.Y.Z` | the `Release` workflow | Go module proxy resolution |

The sub-path tags are required because `api`, `operator`, and `agent` are
separate Go modules that do not live at the repository root. Without them,
`go get` of any submodule at a version fails. They are created only after
artifact publication succeeds, and published module versions are immutable — the
workflow validates every existing tag before pushing any new one, and refuses to
move a tag that already points elsewhere.

## Cutting a release

1. Confirm `main` is green and contains everything intended for the release.
2. Publish a [GitHub Release](https://github.com/ai-dynamo/snapshot/releases/new)
   with a new tag `vX.Y.Z` targeting `main`, and write user-facing release notes
   (see below).
3. The `Release` workflow triggers on `release: published` and:
   - runs the full validation gate against the release commit,
   - builds and pushes the operator and agent images to
     `ghcr.io/ai-dynamo/snapshot`,
   - packages and pushes the Helm chart to GHCR as an OCI artifact,
   - creates the three Go module sub-path tags.
4. Verify the workflow succeeded and that the images and chart are pullable at
   the new version.

Artifacts are always built and published by CI from the release commit. Never
build and push release artifacts from a local machine.

## Release notes

Every release gets user-facing notes describing what changed for someone
operating Snapshot — new capabilities, behavior changes, breaking changes, and
required upgrade actions — not a raw commit log. Call out CRD changes and any
change to checkpoint or restore semantics explicitly, since those affect
existing stored checkpoints.
