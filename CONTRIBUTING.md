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

## Preparing a change

**Run the checks locally.** `make check` is the same gate CI's `check` job runs:
it verifies the CRD copies, regenerates, applies and verifies license headers,
formats, tidies, lints, then runs `govulncheck` and `helm lint` — and fails if
any of that left the working tree dirty. `make test` and `make build` cover the
other two jobs. Running all three before pushing means CI holds no surprises.

`make check` needs a Linux amd64 toolchain: the pinned `protoc` it installs is
only downloaded for that platform. On other hosts, run `make linux-build` and
`make linux-test`, which execute in a container, and let CI run the full gate.

**Write conventional commit messages.** Subjects follow
[Conventional Commits](https://www.conventionalcommits.org/): a type — one of
`feat`, `fix`, `docs`, `ci`, `build`, `refactor`, `perf`, `test`, `chore` — an
optional scope in parentheses, a colon, then a short imperative summary:

```
ci: require an approved issue link on every PR
docs(guides): verify TensorRT-LLM without a GPU driver
```

Pull requests are squash-merged, so the pull request *title* becomes the commit
subject on `main` and follows the same format. Individual commits within a
branch are squashed away, so their subjects matter less than the title.

## Extending Snapshot

Most extension work is adding support for a new inference framework. Stages 2
and 3 of the flow — checkpoint and restore — are framework-agnostic; only the
image and deployment in stage 1 differ, so a new framework is additive and does
not touch the operator or the agent. See the
[usage guides](docs/guides/README.md) for the shape of the flow.

To add one, follow an existing framework end to end —
[vLLM](docs/guides/vllm.md) is the smallest — and provide the same four pieces
under `docs/guides/<framework>/`:

| File | What it does |
| --- | --- |
| `Dockerfile.<framework>` | Starts from the framework's runtime image and adds the entrypoint program |
| `app.py` | Cooperates with the checkpoint/restore lifecycle: loads the model, then signals readiness |
| `deployment.yaml` | Deploys the replica that gets checkpointed |
| `restore-deployment.yaml` | Consumes a checkpoint through the `nvidia.com/restore-from` annotation |

Plus `docs/guides/<framework>.md` walking through it, and an entry in the
[guides index](docs/guides/README.md).

The contract your entrypoint has to honor — the control volume, the startup
gate, the `SNAPSHOT_CONTROL_DIR` environment variable, and the seccomp profile
— is specified in the
[Restore Pod contract](docs/reference/restore-pod-contract.md). Read that
before writing `app.py`; everything a framework must do to be checkpointable is
there.

Extending the API surface instead — a new field, or a new custom resource — is
a larger change that affects stored checkpoints. Open an issue and agree the
approach before writing code, and expect the CRD compatibility question to be
the bulk of the review.

## AI-assisted contributions

AI assistance is welcome. Agent-specific repository guidance lives in
[AGENTS.md](AGENTS.md).

### Repository agent skills

Repository-maintained skills live in [`.agents/skills`](.agents/skills). A
compatible coding agent discovers these skills automatically when it works from
this checkout. Available skills:

- [`snep`](.agents/skills/snep/SKILL.md) — guides an interactive,
  section-by-section draft or revision of a Snapshot Enhancement Proposal.

The repository-local skill is available automatically to compatible agents
working in this checkout. To make a skill available in every local project,
create a symlink from the repository root using the directory for your agent:

```sh
# Codex
mkdir -p "${CODEX_HOME:-$HOME/.codex}/skills"
ln -s "$PWD/.agents/skills/snep" "${CODEX_HOME:-$HOME/.codex}/skills/snep"

# Claude Code
mkdir -p "$HOME/.claude/skills"
ln -s "$PWD/.agents/skills/snep" "$HOME/.claude/skills/snep"

# Cursor CLI
mkdir -p "$HOME/.cursor/skills"
ln -s "$PWD/.agents/skills/snep" "$HOME/.cursor/skills/snep"

# Pi
mkdir -p "$HOME/.pi/agent/skills"
ln -s "$PWD/.agents/skills/snep" "$HOME/.pi/agent/skills/snep"
```

Each command links the same versioned repository skill; remove the matching
symlink to uninstall it. Restart the agent if it does not reload skills
automatically.

The rules are the same as for any other contribution, because the obligations
do not change based on how the code was produced:

- **You are the author.** Signing off with the DCO certifies you have the right
  to submit the work under Apache 2.0. That certification is yours regardless
  of what produced the diff, so do not sign off on code you have not reviewed
  and cannot explain.
- **Review before you open.** Run `make check`, `make test`, and `make build`,
  and read the diff. A pull request you cannot talk through in review is not
  ready, and generated code that merely compiles is not evidence that it is
  correct.
- **Do not paste project code into a service you have not cleared.** This is a
  public repository, so its contents are public — but credentials, internal
  URLs, customer data, and unreleased material are not, and must not be pasted
  into any tool. See the secrets guidance in [AGENTS.md](AGENTS.md).
- **Disclosure is not required, and is welcome.** There is no obligation to
  declare AI assistance. Noting it in the pull request description is useful
  context for reviewers, not a mark against the change.

Pull requests that are bulk-generated, untested, or clearly unreviewed will be
closed. The bar is reviewer time: a change nobody has read wastes it.

## How pull requests are reviewed

**Who reviews.** Every pull request is reviewed by a maintainer. Ownership is
recorded as GitHub teams — `@ai-dynamo/snapshot-codeowners` for the repository
and `@ai-dynamo/snapshot-docs-codeowners` for `docs/` and Markdown files, apart
from proposal documents; see [`.github/CODEOWNERS`](.github/CODEOWNERS) and
[MAINTAINERS.md](MAINTAINERS.md) —
so GitHub requests review from the right team automatically. At least one
maintainer approval is required before a pull request can merge.

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

## Community standards

Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md). This
section covers what happens after a report and what the process does not cover.

**Where to report.** Email <dbar@nvidia.com>. Reports are handled
confidentially. If the report concerns the person at that address, contact any
other maintainer in [MAINTAINERS.md](MAINTAINERS.md) directly.

**Response timeline.**

| Stage | Target |
| --- | --- |
| Acknowledgement that the report was received | 3 business days |
| Decision on outcome, or an update explaining the delay | 14 calendar days |
| Communication of the outcome to the reporter | With the decision |

Investigations involving more people, or spanning several incidents, take
longer. When a report cannot be resolved within 14 days the reporter gets an
update rather than silence.

**Out of scope.** The Code of Conduct process is not the route for:

- **Technical disagreement.** Design and implementation disputes belong in the
  issue, the pull request, or [Discussions](https://github.com/ai-dynamo/snapshot/discussions),
  and are settled as described in [GOVERNANCE.md](GOVERNANCE.md). Disagreeing
  firmly is not a violation; how you do it can be.
- **Security vulnerabilities.** These follow [SECURITY.md](SECURITY.md) and
  NVIDIA PSIRT. Never report one through a Code of Conduct email or a public
  issue.
- **Conduct outside project spaces** that has no bearing on the safety of
  participants here. Behavior elsewhere that does affect that safety is in
  scope.
- **Moderation appeals against NVIDIA products or services**, which are not
  this project's to decide.

Anything touching the safety, dignity, or ability to participate of someone in
this community is in scope. If you are unsure, report it and let the
maintainers decide.

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

## Snapshot Enhancement Proposals (SNEPs)

For a substantial new capability, public API change, or architectural change,
start a [Snapshot Enhancement Proposal (SNEP)](docs/proposals/README.md) before
implementation. A SNEP records the motivation, design, trade-offs, and test
plan so maintainers and contributors can discuss the direction early.
