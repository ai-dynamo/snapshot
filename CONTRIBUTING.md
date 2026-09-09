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

# Contributing to Snapshot

Thank you for your interest in contributing to Snapshot! Contributions are
welcome under the project's [Apache 2.0 license](LICENSE).

Participation in this project is governed by our
[Code of Conduct](CODE_OF_CONDUCT.md). By taking part, you agree to uphold it —
please report unacceptable behavior as described there.
[MAINTAINERS.md](MAINTAINERS.md) lists the maintainers, and
[GOVERNANCE.md](GOVERNANCE.md) describes how decisions get made.

## Start with an issue

Every change starts as an issue, and every pull request must link to one.

1. Open an issue using one of the [issue templates](.github/ISSUE_TEMPLATE),
   or find an existing one that covers the change.
2. Wait for a maintainer to triage it and apply the `approved` label. The label
   is the signal that the project wants the change, so please wait for it before
   investing significant work.
3. Reference the issue in the pull request description, for example
   `Fixes #123` or
   `Fixes https://github.com/ai-dynamo/snapshot/issues/123`.

The `Validate Issue Reference` check fails while a pull request references no
issue, or references only issues that are closed or not yet labeled `approved`.
It re-runs on its own when the label is added, so there is no need to push an
empty commit. Maintainers can waive the requirement for a specific pull request
by adding the `skip-issue-check` label; pull requests opened by bots, such as
Dependabot, are exempt automatically.

Questions and open-ended design discussions belong in
[Discussions](https://github.com/ai-dynamo/snapshot/discussions) rather than in
an issue. Security vulnerabilities are never reported in a public issue; see
[SECURITY.md](SECURITY.md).

## How pull requests are reviewed

**Who reviews.** Every pull request is reviewed by a maintainer. The maintainers
are the code owners for the whole repository — see
[`.github/CODEOWNERS`](.github/CODEOWNERS) and [MAINTAINERS.md](MAINTAINERS.md) —
so GitHub requests review from that group automatically. At least one maintainer
approval is required before a pull request can merge.

**What has to pass.** Alongside the approval, CI must be green: the `check`,
`build`, and `test` jobs, the DCO check, and `Validate Issue Reference`. A
maintainer will not usually start a detailed review while CI is red, so fix
failing checks first.

**Turnaround.** Maintainers aim to give a first response within a week. Reviews
are best-effort alongside other work, and larger or more invasive changes take
longer than small ones — another reason to agree on the approach in the issue
before writing code.

**Addressing feedback.** Push follow-up commits to the same branch rather than
force-pushing over the history under review, so reviewers can see what changed;
squashing happens at merge. Reply to each review comment, and re-request review
when you have addressed them.

**Following up.** If a pull request has had no response after a week, comment on
it to bump it. If it stays quiet after that, raise it in
[Discussions](https://github.com/ai-dynamo/snapshot/discussions) or mention a
maintainer from [MAINTAINERS.md](MAINTAINERS.md) directly. Pinging is welcome —
a stalled review is a maintainer oversight, not an imposition.

**Merging.** Maintainers merge; contributors do not need to (and cannot) merge
their own pull requests. A pull request that goes 90 days without activity is
labeled `lifecycle/stale` and closed 30 days later, as described below.

## Triage, priority, and inactivity

Maintainers triage new issues weekly and set a priority label. Only maintainers
apply labels and priorities.

| Label | Meaning | Target fix |
| --- | --- | --- |
| `priority/P0` | Major functionality broken, significant user impact, no workaround | 30 days |
| `priority/P1` | Usable but does not work as documented, workaround exists | 6 months |
| `priority/P2` | Minor defect with minor impact | No commitment |
| `needs-triage` | Not yet prioritized | Triaged within 7 days |

Issues and pull requests with no activity for 90 days are labeled
`lifecycle/stale` and closed 30 days later unless the discussion resumes. Add
the `lifecycle/frozen` label to exempt an item from this.

## Developer Certificate of Origin (DCO)

Snapshot requires all contributions to be signed off with the
[Developer Certificate of Origin (DCO)](https://developercertificate.org/).
The sign-off certifies that you wrote the patch, or otherwise have the right to
submit it under the project's Apache 2.0 license. By contributing, you agree
that your contributions will be licensed under the
[Apache 2.0 License](LICENSE).

### Signing off your commits

Add a `Signed-off-by` trailer to every commit by passing `-s` (or `--signoff`)
to `git commit`:

```bash
git commit -s -m "your commit message"
```

This appends a line using the name and email from your Git configuration:

```
Signed-off-by: Jane Developer <jane@example.com>
```

Make sure your `user.name` and `user.email` are set correctly:

```bash
git config user.name "Jane Developer"
git config user.email "jane@example.com"
```

**Unsigned commits fail the DCO check**, and the pull request cannot be merged
until every commit carries a valid `Signed-off-by` trailer.

### Fixing a missing sign-off

If you forgot to sign off, you can amend the most recent commit:

```bash
git commit --amend -s --no-edit
git push --force-with-lease
```

To sign off multiple commits at once, rebase over the range and sign each one:

```bash
git rebase --signoff origin/main
git push --force-with-lease
```

## Developer Certificate of Origin 1.1

The full text of the DCO is reproduced below.

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.
1 Letterman Drive
Suite D4700
San Francisco, CA, 94129

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```
