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

# Governance

Snapshot is an open source project maintained by NVIDIA and open to outside
contributors. This document describes who makes decisions, how those decisions
are made, and how the set of decision-makers changes.

## Roles

| Role | Who | What they do |
| --- | --- | --- |
| **Contributor** | Anyone who opens an issue, a pull request, or a discussion | Proposes and implements changes |
| **Maintainer** | Listed in [MAINTAINERS.md](MAINTAINERS.md) and [`.github/CODEOWNERS`](.github/CODEOWNERS) | Reviews and merges pull requests, triages issues, sets priorities, cuts releases |

There is no separate committer tier. Maintainers hold write access; everyone
else contributes through pull requests from forks.

## How decisions are made

The project runs on **lazy consensus**: a proposal is accepted if no maintainer
objects. In practice this means:

1. **Routine changes** — bug fixes, documentation, tests, dependency updates.
   Decided in code review. One maintainer approval is required to merge, as
   enforced by `CODEOWNERS` and branch protection. No wider discussion needed.

2. **User-visible or architectural changes** — new APIs or CRD fields, changes
   to checkpoint or restore semantics, new dependencies, anything that changes
   behavior for existing users. These start as an issue, which a maintainer
   triages and labels `approved` before significant work begins. The `approved`
   label is the project's signal that it wants the change, and the
   `Validate Issue Reference` check enforces the link from the pull request. See
   [CONTRIBUTING.md](CONTRIBUTING.md#start-with-an-issue).

3. **Disagreement** — if a maintainer objects to a change, the objection is
   resolved in the issue or pull request discussion. Any maintainer may block a
   merge by requesting changes; the block stands until it is withdrawn or the
   maintainers reach agreement. Consensus is preferred over voting.

4. **Deadlock** — if maintainers cannot reach consensus, the decision escalates
   to a simple majority vote of the maintainers, recorded in the relevant issue.
   A tie means the change is not made.

Open-ended design questions belong in
[Discussions](https://github.com/ai-dynamo/snapshot/discussions) rather than in
an issue.

## Issue triage and priority

Maintainers triage new issues weekly and set a priority label. Only maintainers
apply labels and priorities. The priority ladder and the stale-issue policy are
documented in
[CONTRIBUTING.md](CONTRIBUTING.md#triage-priority-and-inactivity).

## Releases

Maintainers decide when to cut a release and what goes into it. Releases are
built and published by the `release` workflow. Because Snapshot's APIs may still
change, releases are currently pre-1.0 and API stability is not yet guaranteed —
see the note at the top of the [README](README.md).

[RELEASE.md](RELEASE.md) documents the versioning scheme, the tagging
convention, and the mechanics of cutting a release.

## Becoming a maintainer

Maintainers are added by consensus of the existing maintainers. The usual path
is a sustained record of merged contributions, useful code review, and issue
triage — roughly a few months of steady participation rather than any fixed
count of pull requests.

To propose a new maintainer, an existing maintainer opens an issue naming the
candidate. If no maintainer objects within two weeks, the candidate is added to
[MAINTAINERS.md](MAINTAINERS.md) and `.github/CODEOWNERS` in a pull request.

## Stepping down and inactivity

Maintainers may step down at any time by opening a pull request removing
themselves. Maintainers who have been inactive for six months may be moved to
emeritus status by consensus of the remaining maintainers; this is not a
judgment on past contributions, and returning maintainers can be reinstated by
the same process that added them.

## Changing this document

Changes to this document follow the same process as any other user-visible
change: an issue, then a pull request, approved by maintainer consensus.

## Code of conduct

Participation in the project is governed by
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), which applies to every project space.
