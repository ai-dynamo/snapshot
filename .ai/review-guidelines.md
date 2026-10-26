<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Review guidelines

Apply these rules to changed code, tests, comments, documentation, Snapshot Enhancement Proposals (SNEPs) under docs/proposals/, and PR text. Keep findings tied to the PR and the behavior it affects. Match surrounding conventions. Do not infer AI authorship from wording or code style.

## Examine every added or changed line

Examine every added or changed line for correctness, necessity, and avoidable complexity. Ask whether the line serves the stated requirement, whether an existing mechanism could replace it, and whether it adds unnecessary state, branching, duplication, dependencies, or maintenance work. Trace surrounding code before reaching a conclusion.

For comments and documentation, check that every claim is accurate and every sentence is useful. Flag vague or grandiose wording, padding, and unnecessarily complicated explanations. Suggest simple wording that preserves the technical meaning.

Raise each concrete concern as an actionable review thread, even when the code works. Give the exact location, explain the consequence, and propose the smallest suitable fix or simpler alternative. Ask for justification when necessity depends on an unstated requirement. Group related lines and repeated instances into one thread.

Do not invent objections, demand justification for obvious code, or repeat questions already answered by the PR, linked issue, or source.

## Raise actionable findings as threads

- Raise a separate review thread for each independently actionable problem, including simplifications and wording fixes. Do not report actionable findings only in the summary or walkthrough.
- Use inline threads for file-specific findings. For PR descriptions and concerns about the whole PR, use a top-level comment and ask the author to resolve the finding through replies.
- Label each finding as a bug, simplification, test gap, documentation/wording issue, or unresolved requirement. Give the exact location, the problem and its consequence, and the smallest suitable fix or replacement wording.
- Combine repeated instances of the same underlying problem into one thread and list the other locations. Do not invent findings or post approval-only comments.

## Verify claims

- Read relevant callers and surrounding code before reporting a bug. Trace state changes, locking, resource ownership, and cleanup where relevant. Explain the condition that triggers the problem and its consequence.
- Search callers before claiming code is unused, and trace write sites before claiming a value is constant. Recheck the source when a finding is challenged.
- Separate verified problems from unresolved requirements and tradeoffs. State missing evidence; do not present speculation as a confirmed bug.
- Do not claim a proposed fix was tested or a performance improvement was measured without supporting evidence. Distinguish source analysis, executed local tests, deployed state, and workload end-to-end evidence.
- For SNEPs, distinguish proposed behavior from existing implementation and measured results. Check the design against the stated problem, requirements, tradeoffs, and validation plan. Raise concrete gaps and contradictions as threads. Do not treat an unimplemented proposal as a bug merely because the current code does not implement it. Follow docs/proposals/README.md and the SNEP template for proposal structure.

## Prefer simple code

- Search for existing helpers and equivalent paths. Flag duplicated lifecycle, storage, retry, validation, and cleanup logic. Name what can be reused and which behavior and error semantics must remain.
- Flag unnecessary abstractions, branches, options, wrappers, and dependencies. Show a concrete simpler alternative that meets the stated requirements. Avoid speculative flexibility and handling for cases the surrounding code rules out.
- Suggest helper extraction when a block has a distinct responsibility or a function mixes preparation, execution, error handling, and cleanup. Identify the lines, helper name, inputs and result, and the concrete readability or reuse benefit. A single caller does not rule out a useful helper.
- Preserve resource ownership, cleanup order, cancellation, and error semantics. Avoid helpers that only forward calls, hide important control flow, or need most of the caller's mutable state. Do not split functions solely by line count or replace clear code with compressed syntax.

## Check tests

- Check that tests prove the changed behavior and relevant failure cases. Raise missing regression coverage and assertions that merely mirror the implementation without checking a meaningful result.
- Flag repeated assertions that can be simplified while preserving distinct failure cases and useful acceptance coverage.

## Use simple, accurate English

- Check changed comments, documentation, and PR descriptions for accuracy against the source. Apply the same writing standard to review comments and suggestions.
- Use short sentences, active voice, one main idea per sentence, and consistent terms. Name the component and action when a pronoun would make the subject unclear. Keep exact identifiers and necessary technical terms; briefly explain unfamiliar terms.
- Flag grandiose wording, vague claims, padding, redundant explanations, and explanations more complicated than the behavior requires. Suggest concrete, simple wording without changing the technical meaning.
- Permanent comments describe current behavior or a non-obvious constraint. Flag stale change narratives such as "the old behavior" or "now does Y instead of Z" when they require change history to understand. Keep useful reasons for design decisions. PR descriptions explain the problem, resulting behavior, relevant tradeoffs, and actual validation.
- Base naming and style suggestions on surrounding conventions, not personal preference. Identify a concrete clarity or consistency problem.

## Check documentation placement and names

- Check each documentation change against docs/development/writing-docs.md. Flag a fact that is on the wrong page, stated twice, or padding the change beyond what the reader needs, and name the page it belongs on or the lines to cut. Flag controller internals, change history, rejected alternatives, and runbooks in reference pages. SNEPs follow their template instead.
- Check each new or changed name (field, Helm value, annotation, label, environment variable, event reason, flag, or term) against docs/development/naming.md and its neighbors. Flag a second name for an existing concept, a user-facing name that names the mechanism instead of the capability, and an internal name that reaches the docs or a message that a user reads.
- Check that every table that lists the thing was updated, that a new page is linked from the index of its section, and that a user-visible change has a CHANGELOG entry.
- Check that a change to docs/reference/architecture/README.md shows the change in about a line (a table row, diagram edge, or numbered step), and that mechanics (queues, retries, failure reasons, timeouts, formats, code paths) are in a detailed page that the overview links to.
- Check that a feature needing a newer driver, CUDA, GPU Operator, kernel, or runtime than the README baseline has a row in docs/reference/compatibility.md, and that the requirement is not stated only in an error message or a comment.

## Suggest practical splits for large PRs

- For a large PR, check whether the changes contain logically independent pieces with separate purposes. Raise a PR-level comment suggesting a split when each resulting PR can build, pass relevant checks, and be understood independently, with a clear merge order for dependencies.
- Name the proposed PRs, the changes belonging to each, their dependencies and merge order, and why the split improves review.
- Apply this check to large SNEPs too. Suggest separate proposals when the design combines independent changes that can be understood and implemented separately. Name the proposed boundaries and dependencies; code build requirements apply to implementation PRs, not proposal-only PRs.
- Do not suggest a split solely by line count or separate tightly coupled changes into broken intermediate states.
