<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Review guidelines

Apply these rules to changed code, tests, comments, documentation, Snapshot Enhancement Proposals (SNEPs) under docs/proposals/, and PR text. Keep findings tied to the PR and the behavior it affects. Match surrounding conventions. Do not infer AI authorship from wording or code style.

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

## Suggest practical splits for large PRs

- For a large PR, check whether the changes contain logically independent pieces with separate purposes. Raise a PR-level comment suggesting a split when each resulting PR can build, pass relevant checks, and be understood independently, with a clear merge order for dependencies.
- Name the proposed PRs, the changes belonging to each, their dependencies and merge order, and why the split improves review.
- Apply this check to large SNEPs too. Suggest separate proposals when the design combines independent changes that can be understood and implemented separately. Name the proposed boundaries and dependencies; code build requirements apply to implementation PRs, not proposal-only PRs.
- Do not suggest a split solely by line count or separate tightly coupled changes into broken intermediate states.
