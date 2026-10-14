---
name: snep
description: Guide users through creating or revising a Snapshot Enhancement Proposal (SNEP), including an interactive section-by-section draft. Use for significant Snapshot design changes, not ordinary implementation or small documentation changes.
license: Apache-2.0
metadata:
  SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
---

# Snapshot Enhancement Proposals

Facilitate a focused dialogue to draft or continue a Snapshot Enhancement
Proposal (SNEP). SNEPs are design documents for significant Snapshot changes;
they establish motivation, scope, design, trade-offs, and validation before
implementation.

Read [the SNEP process](../../../docs/proposals/README.md) and [the SNEP
template](../../../docs/proposals/NNNN-template/README.md) before starting.
They are the source of truth when they differ from this skill.

## Working style

- Lead the conversation one topic at a time; do not present every question at
  once.
- Draft a concise, polished section after the user provides its content. Ask
  for confirmation before proceeding, then write that confirmed section.
- Keep the proposal non-repetitive: Summary explains the change, Motivation
  explains why, Proposal describes behavior, and Design Details explains how.
- Push back gently on vague scope, missing failure behavior, or implementation
  detail placed outside Design Details.
- Preserve the user's decisions and distinguish them from open questions or
  follow-up work. Do not invent product commitments, issue numbers, APIs, or
  maturity dates.

## Identify or continue the SNEP

Ask for a Snapshot tracking issue number or its GitHub issue URL. The issue
number is the SNEP number; do not create a new proposal without one.

Search `docs/proposals/<issue-number>-*/README.md` for an existing proposal.
If it exists, tell the user its path and ask whether to continue editing it or
start a replacement. For a continuation, review the existing headings in order
and ask which sections to keep, revise, or complete.

If it does not exist, ask for a short descriptive title. Confirm the proposed
path, `docs/proposals/<issue-number>-<kebab-case-title>/README.md`, then copy
the template into that path. Replace its title placeholder and run `make
update-toc` so the initial document is navigable. Retain the template's SPDX
header, headings, and `<!-- toc -->` markers.

## Draft the sections

Work through the template in its order. Offer optional sections only when they
clarify the proposal. For each confirmed section, replace its template comment
with the draft and run `make update-toc` after a heading changes.

### Summary and Motivation

- **Summary:** one user-focused paragraph describing the proposed functional
  change and benefit without implementation detail.
- **Motivation:** explain the problem and why it matters.
- **Goals / Non-Goals:** make outcomes observable and scope boundaries
  unambiguous. Ask what a reasonable reader might mistakenly assume is in
  scope.

### Proposal

Describe the expected behavior, users, and boundaries at a level suitable for
design review. Keep APIs, data models, algorithms, and controller/agent flows
in Design Details.

- **User Stories (optional):** use real scenarios when they sharpen a user or
  operator need.
- **Limitations, Risks, and Mitigations:** cover compatibility, operational,
  performance, security, and ecosystem risks. Pair material risks with a
  credible mitigation or explicitly record them as accepted.

### Design Details

Explain lifecycle, data model, algorithms, failure behavior, and implementation
boundaries. Use diagrams, examples, CRD fragments, or CLI snippets only when
they make the design clearer.

- **API:** address public CRDs, CLI commands, configuration contracts, and
  compatibility. Explicitly state when no public API changes.
- **Security:** this is a required, standalone **SNEP-only subsection within
  Design Details**, separate from API. Cover authorization, isolation,
  credentials, data exposure, and abuse cases, or explicitly state why no
  security impact is expected.
- **Configuration** and **Performance and Scalability** (optional): include
  them when defaults, feature gates, migration, resource use, latency,
  throughput, or scale limits change.
- **Monitoring:** specify relevant metrics, events, status conditions, logs, or
  dashboards; state when none are needed.
- **Dependencies (optional):** identify external systems, Snapshot components,
  feature gates, or earlier SNEPs and link setup material where useful.
- **Test Plan:** ground the plan in existing Snapshot coverage where possible.
  Describe unit, integration, and end-to-end scenarios, expected outcomes, and
  links to tracking issues when they exist.
- **Graduation Criteria:** define alpha, beta, and/or GA evidence appropriate
  to the feature's scope, including API stability, test coverage, operational
  evidence, and migration or deprecation requirements. Do not prescribe release
  timing the user has not chosen.

### Remaining sections

Use **Implementation History** for dated milestones only when known. Use
**Alternatives** for materially considered approaches and concise rejection
reasons. Use the **Appendix** for supporting reading or detailed data that
would interrupt the main narrative.

## Finish

Run `make update-toc` after all heading changes, then run `make verify-toc`.
Review and commit the proposal and generated TOC with DCO sign-off before
running `make check`, which requires a clean working tree. Then run `make
check`, `make test`, and `make build`. If `make check` changes files, amend the
commit and rerun it. Confirm the saved path and remind the user that the
proposal is submitted as a GitHub pull request under the repository's SNEP
process.
