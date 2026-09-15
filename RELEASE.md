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

Snapshot releases monthly. The maintainers cut a minor release once a month from
whatever has landed on `main`; a month with nothing user-visible to ship can be
skipped rather than padded. Patch releases are cut from the affected line's
maintenance branch as needed, not on the monthly cadence. Release candidates precede any release that changes
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

Throughout, the *release branch* means `main` for the first release of a minor
line (`vX.Y.0`), and that line's maintenance branch — `release/X.Y` — for every
patch release after it. The `Release` workflow builds from the commit the tag
points at, so tagging a patch release against `main` would publish whatever else
has landed there since.

1. Confirm the release branch is green and contains everything intended for the
   release.
2. Publish a [GitHub Release](https://github.com/ai-dynamo/snapshot/releases/new)
   with a new tag `vX.Y.Z` targeting the release branch, and write user-facing
   release notes (see below).
3. The `Release` workflow triggers on `release: published` and:
   - runs the full validation gate against the release commit,
   - builds and pushes the operator and agent images to
     `ghcr.io/ai-dynamo/snapshot`, each carrying SLSA provenance and an SPDX
     SBOM as OCI attestations,
   - packages and pushes the Helm chart to GHCR as an OCI artifact,
   - signs both images and the chart by digest with cosign keyless signing,
   - attaches an SPDX and a CycloneDX SBOM per image, the chart tarball, and a
     signed `SHA256SUMS` to the GitHub release,
   - creates the three Go module sub-path tags.
4. Verify the workflow succeeded and that the images and chart are pullable at
   the new version.

Artifacts are always built and published by CI from the release commit. Never
build and push release artifacts from a local machine. Signing happens in CI
too: the identity in every signature is this repository's workflow, not a
person, and there is no private key anywhere to leak or rotate.

## Verifying a release

**Signing applies to `v0.2.0` and later.** Earlier releases were published
before the signing pipeline existed, and are not signed retroactively: a
signature made today would be dated today while implying the artifact was
signed when released. Those releases do carry SBOMs, generated after the fact
by scanning the images that were actually published — see
[Releases before v0.2.0](#releases-before-v020) below.

Every published artifact from `v0.2.0` on is signed with
[Sigstore](https://www.sigstore.dev/) keyless signing. There is no public key to
fetch — verification asserts *which workflow, in which repository, at which tag*
produced the artifact, and the signature is recorded in the public Rekor
transparency log.

The `cosign` commands below need
[cosign](https://docs.sigstore.dev/cosign/installation/) **v3.0 or newer**. CI
signs with v3, which stores signatures in Sigstore's bundle format alongside the
artifact. Older cosign releases look for a `sha256-<digest>.sig` tag instead,
do not find one, and report `no signatures found` — indistinguishable from an
unsigned artifact. This was confirmed against cosign v2.4.1; if you see that
error, check `cosign version` before concluding anything.

Set the identity of this repository's release workflow once:

```bash
export COSIGN_IDENTITY='^https://github\.com/ai-dynamo/snapshot/\.github/workflows/push-artifacts\.yaml@refs/tags/v'
export COSIGN_ISSUER='https://token.actions.githubusercontent.com'
```

The regular expression is anchored on `refs/tags/v`, so it accepts only
artifacts built from a release tag. Images built from `main` are signed by the
same workflow at `refs/heads/main` and are deliberately rejected by it.

### Images

Verify by digest where you can; a tag can be repointed, a digest cannot.

```bash
cosign verify \
  --certificate-identity-regexp "${COSIGN_IDENTITY}" \
  --certificate-oidc-issuer "${COSIGN_ISSUER}" \
  ghcr.io/ai-dynamo/snapshot/operator:v0.2.0

cosign verify \
  --certificate-identity-regexp "${COSIGN_IDENTITY}" \
  --certificate-oidc-issuer "${COSIGN_ISSUER}" \
  ghcr.io/ai-dynamo/snapshot/agent:v0.2.0
```

A successful run prints the signature payload and the certificate subject; a
failure exits non-zero with `no matching signatures`.

### Helm chart

The chart is an OCI artifact in the same registry, so it verifies the same way:

```bash
cosign verify \
  --certificate-identity-regexp "${COSIGN_IDENTITY}" \
  --certificate-oidc-issuer "${COSIGN_ISSUER}" \
  ghcr.io/ai-dynamo/snapshot/snapshot:0.2.0
```

Note the chart tag carries no `v` prefix — Helm chart versions are bare semver.

### Provenance and SBOM attestations

Each image carries an in-toto SLSA provenance statement and an SPDX SBOM,
attached as OCI attestations at build time:

```bash
docker buildx imagetools inspect \
  ghcr.io/ai-dynamo/snapshot/operator:v0.2.0 \
  --format '{{ json .Provenance }}'

docker buildx imagetools inspect \
  ghcr.io/ai-dynamo/snapshot/operator:v0.2.0 \
  --format '{{ json .SBOM }}'
```

The provenance names the source repository, the commit, and the workflow that
built the image, so you can confirm an image came from the commit it claims.

### Release assets and checksums

Releases from `v0.2.0` carry two SBOMs per image — SPDX (`.spdx.json`) and
CycloneDX (`.cdx.json`), the same inventory in the two formats consumers ask
for — plus the packaged chart and a `SHA256SUMS` covering all of them.
`SHA256SUMS` is itself signed, so verifying one signature transitively covers
every asset:

```bash
gh release download v0.2.0 --repo ai-dynamo/snapshot

cosign verify-blob \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp "${COSIGN_IDENTITY}" \
  --certificate-oidc-issuer "${COSIGN_ISSUER}" \
  SHA256SUMS

sha256sum --check SHA256SUMS
```

`SHA256SUMS.sigstore.json` is a Sigstore bundle: it carries the signature and
the signing certificate in one file, so there is no separate `.sig`/`.pem` pair
to download.

Verify the signature *before* trusting the checksums. `sha256sum --check` on
its own only proves the files match a list an attacker could have replaced
alongside them.

### Releases before v0.2.0

`v0.1.0` and the pre-releases before it carry SPDX and CycloneDX SBOMs as their
only release assets — no signature and no `SHA256SUMS`. Those SBOMs were
generated after the fact by scanning the images already published to GHCR, so
the `created` timestamp inside each one is the backfill date, not the release
date.

Their images do carry a SLSA provenance attestation, because buildx attaches a
minimal one by default. It is not the `mode=max` provenance later releases
carry, and there is no SBOM attestation on them — `imagetools inspect --format
'{{ json .SBOM }}'` returns `{}` for these tags.

Treat them as an inventory, not as provenance. An SBOM is descriptive and
independently reproducible — anyone can re-derive it from the digest it was
taken from. Resolve that digest rather than rescanning the tag, so a repointed
tag cannot hand you an inventory of different bytes:

```bash
DIGEST="$(docker buildx imagetools inspect \
  ghcr.io/ai-dynamo/snapshot/operator:v0.1.0 \
  --raw | sha256sum | cut -d' ' -f1)"

syft scan --platform linux/amd64 \
  "registry:ghcr.io/ai-dynamo/snapshot/operator@sha256:${DIGEST}"
```

What they do not tell you is who built the image or from which commit. That
question has no answer for these releases, which is the reason signing starts at
`v0.2.0` rather than being applied backwards.

## Release notes

Every release gets user-facing notes describing what changed for someone
operating Snapshot — new capabilities, behavior changes, breaking changes, and
required upgrade actions — not a raw commit log. Call out CRD changes and any
change to checkpoint or restore semantics explicitly, since those affect
existing stored checkpoints.
