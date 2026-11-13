<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Writing Documentation

How to add or change Snapshot documentation, for people and for AI coding
agents. Names and terms are in [Naming Conventions](naming.md).

A reader opens one page to answer one question, so each fact lives on one page.
A documentation change is usually small: a few table rows and a few sentences.
When one is much larger, ask whether part of it belongs on another page, and
keep it here only if it really does.

These are defaults for the common case. When following one would make the docs
worse for the reader, do what serves the reader and say why in the PR
description.

## Where Facts Live

| The reader asks | Page |
| --- | --- |
| Which fields, annotations, labels, or conditions exist, and what are their defaults? | [API Reference](../reference/api.md) |
| Which Helm values exist? | [Helm Chart Values](../reference/helm-values.md) |
| Which driver, CUDA, GPU Operator, kernel, or runtime versions do I need? | [Prerequisites](../get-started/prerequisites.md), the baseline and the requirements per feature |
| What must my workload or Pod do? | [Workload Contract](../reference/workload-contract.md), [Restore Pod Contract](../reference/restore-pod-contract.md) |
| How do I install or uninstall? | [Installation](../get-started/installation.md) |
| How do I configure storage or upgrade? | [`docs/operate/`](../operate) |
| How do I checkpoint a pod and restore it for the first time? | [Using Snapshot](../using-snapshot/README.md) |
| It failed. Why, and what do I do? | [`docs/troubleshooting/`](../troubleshooting), starting at [Reading a Failure Message](../troubleshooting/reading-a-failure-message.md) |
| What is not supported? | [Limitations and Known Issues](../overview/limitations.md) |
| How do the pieces fit together? | [Architecture](../overview/architecture-overview.md) |
| How does one subsystem or flow work inside? | A detailed page in [`docs/design/`](../design), such as [Multi-GPU](../design/multi-gpu/README.md) |
| Why was it designed this way? | A [Snapshot Enhancement Proposal (SNEP)](../design/README.md) |
| What changed in this release? | [CHANGELOG.md](../../CHANGELOG.md) |

One set of facts is listed twice, so a change updates both: the baseline
prerequisites (the README [Prerequisites](../../README.md#prerequisites) and the
[Prerequisites](../get-started/prerequisites.md) page). State every other fact
once, and link to it from other pages.

## What a Change Touches

Change a page when the change alters the answer to the question that page
answers. A new option or feature usually alters these:

1. The API reference: its rows.
2. A contract page: what it requires of a workload or Pod, if anything.
3. One section in `docs/operate/`, if an operator must do something.
4. [Prerequisites](../get-started/prerequisites.md): one row, if the feature needs
   a newer driver, CUDA, GPU Operator, kernel, or runtime than the baseline.
5. Architecture: the overview, if the change adds or removes a component, a
   resource, or a step of the checkpoint or restore flow; a detailed page, if it
   changes how a subsystem works. See [Architecture Docs](#architecture-docs).
6. The CHANGELOG: one entry.

It rarely alters the answer on these pages, so change them when it does and
leave them alone when it does not:

| Page | Changes when |
| --- | --- |
| Using Snapshot | The default first-run path for checkpoint and restore changes |
| Troubleshooting | An issue, a support case, or the request reports a failure that users hit |
| Limitations | The change adds or removes an unsupported case |
| [docs/README.md](../README.md) | A page is added or removed |

Where the change matters on any other page, add one sentence with a link.

## Entry Shapes and Sizes

The sizes are what a feature usually needs, not limits. Write what the reader
needs. A much larger entry is a prompt to ask whether the content belongs on
this page.

| Page | One entry is | Usually |
| --- | --- | --- |
| API reference | A table row per field, value, annotation, label, or condition: name, type, required, default, one sentence. Behavior a row cannot hold goes in a few sentences under the table, with a link to the page that has the procedure | Rows and a few sentences |
| Contract | Short requirements that use the exact names the workload sees: environment variables, paths, annotations | About 12 lines |
| Operate | One section: what it does, the settings, how to check it, where to look when it fails | About 25 lines |
| Troubleshooting | One section per symptom, titled as the user sees it: the cause in a sentence or two, commands to confirm it, the fix | About 12 lines |
| Prerequisites | One table row: the feature, the minimum versions it needs, a link to the page that documents it, and, when it is not obvious, what a node below the minimum shows | 1 row |
| Limitations | One bullet: the limit, and a link to the workaround if there is one | A bullet |
| CHANGELOG | What is new or changed, what to do on upgrade, a flag on CRD changes, and a link to the details. Do not restate the details | About 5 lines |
| Guide | Numbered steps with runnable commands on one path. No alternatives, worked examples of options, or production hardening | |
| Architecture overview | What shows the change: a table row, a diagram edge, or a numbered step | A line |
| Detailed architecture page | One subsystem or flow: the mechanism, the failure behavior, and a code map | As long as the mechanism needs |

## Architecture Docs

Architecture has two levels. Keep them apart, so the overview stays short and
easy to read.

| | Overview | Detailed page |
| --- | --- | --- |
| Page | [Architecture](../overview/architecture-overview.md) | One per subsystem or flow in [`docs/design/`](../design) |
| Reader | Anyone who wants the mental model | A contributor changing that subsystem |
| Holds | The components and what each is responsible for, the resources, how a checkpoint and a restore proceed in a few steps, where data lives, the trust boundaries | How the subsystem works: algorithms, ordering and why, queues and retries, failure reasons and timeouts, protocols and file formats |
| Names | Components and resources | Functions, queues, files, reasons, and settings, as the code spells them |

Changing the overview:

- Add to it when a change adds or removes a component, a resource, or a step of
  the checkpoint or restore flow.
- Add what shows the change: usually one table row, one diagram edge, or one
  numbered step.
- Show the change once, where a reader first meets it. Do not thread it through
  every section that touches it. A new optional component, for example, adds one
  row to the Components table and one edge to the diagram. How it works goes in
  its detailed page.
- A diagram shows components and their handoffs, not the steps inside one
  component.
- Put the mechanism in a detailed page, and link to it from one line in the
  overview.

A detailed page covers one subsystem or flow. It opens with its scope and a link
back to the overview, explains the mechanism and what happens on failure, and
ends with a code map of the packages that implement it, taken from the code. If
the subsystem has no page yet, add one. A mechanism is explained once, in its detailed page.

## Version Requirements

State a requirement as a minimum version ("580 or newer"). Say how to check it
when that is not obvious, and what a node below it shows, such as a refusal that
names a compatibility check or a CUDA error code. A feature that needs more than
the baseline gets one row in [Prerequisites](../get-started/prerequisites.md); see
[CONTRIBUTING.md](../../CONTRIBUTING.md#documenting-a-feature-specific-version-floor).
The feature's own page links to that row and does not repeat the number.

## Before You Write

1. Search for the fact:
   `rg -n '<identifier or phrase>' README.md docs CHANGELOG.md`.
   If it exists, edit that text. Do not add a second copy.
2. Write only what the change description, the code, the chart, the CRD, or a
   test states. A value or behavior you were not given is a question for the
   author. List it in your PR description and leave it out of the docs.
3. Translate loose wording from the issue or PR, such as "dump", "helper", or
   "saved state", into the terms in
   [Naming Conventions](naming.md). Describe a feature by what it does for the
   reader, not by the mechanism behind it.
4. Edit a sentence when the change makes it false or incomplete. Leave the rest
   of the page alone.

## What Does Not Belong in the Docs

| Content | Put it in |
| --- | --- |
| How the controller or agent works: algorithms, ordering, retries, locking, event de-duplication | A detailed architecture page, or code comments |
| Why this design, and the alternatives that lost | A SNEP |
| What behaved differently before, or what this change did | The CHANGELOG entry and the PR description |
| How you tested it | The PR description |
| Work that is planned but not done | A GitHub issue; a [Limitations](../overview/limitations.md) bullet if users hit the gap |
| A warning about a mistake nobody has made | Nowhere |

## Style

- Give each paragraph one point, and make it as long as the point needs. A long
  paragraph is fine when the reasoning really needs it. Padding is not.
- Ask of each sentence whether it helps the reader act, decide, or understand,
  and cut it if it does not. Usual culprits: a sentence that restates its
  heading, describes the page, or repeats the one before it.
- Keep a detail when the reader would act differently without it: a name, a
  default, a limit, or a command the reader types. How the controller reaches the
  result belongs in a detailed architecture page.
- Use a table, a list, or a command for a set of facts or steps.
- Wrap prose at 80 columns. Tables and code blocks are exempt.
- Use Title Case for headings. Use one H1 per page and do not skip levels.
- Make link text the title of the page it links to.
- Do not use em dashes. Use a period, a comma, a colon, or parentheses.
- Write notes as `> [!NOTE]`, not as bold text.
- Do not start a sentence with "Because" or "So". Write a direct statement.
- Write full words instead of contractions: "do not", "cannot", "it is".
- Keep sentences under 30 words.
- Cut filler ("simply", "just", "in order to", "it is important to note") and
  marketing words ("seamless", "powerful", "leverage", "comprehensive").
- Name the thing: write the field, flag, or command, not "configure the
  appropriate settings".
- Put the condition before the instruction: "To enable X, set `y`".
- Write one action per numbered step.
- Introduce every list, table, and code block with a complete sentence.
- Use "can" for capability and "may" only for an uncertain outcome.
- Write "for example", "that is", and "and so on" instead of "e.g.", "i.e.",
  and "etc.".
- Use the Oxford comma, do not write "please", and write "NVIDIA" in capitals.
- Use "backend" only for `pvc` and `s3`. Use "storage option" for anything else.
- Open every page with an intro that says what the page covers.
- End a page with Next Steps as `- [Link] - Description`, with a capital letter
  after the hyphen.
- Write short, active, present-tense sentences. Name the actor: "The operator
  deletes the directory", not "The directory is deleted".
- Address the reader as "you", and write steps as imperatives.
- Define a term at first use, or leave it out. Do not introduce a concept the
  reader does not need in order to act.
- Put identifiers in backticks: fields, values, flags, environment variables,
  paths, and kinds.
- In commands, use `<namespace>`, `<registry>`, and `<tag>` as placeholders and
  set `WORKLOAD_NAMESPACE` once. In guides, spell out long options
  (`--namespace`).
- Tag every fenced block with its language, such as `bash` or `yaml`.
- Link with relative paths. Link to the page that owns a fact instead of
  restating it.
- Align the pipes of a table like the tables around it.

## Before You Push

- [ ] Each page the change touches has a new answer to its question, and the
  change is no larger than the reader needs.
- [ ] The change does one thing. A fix to unrelated text is a separate PR.
- [ ] No fact is stated twice, and every table that lists the thing is updated.
- [ ] The architecture overview shows the change in a line, and the mechanics are
  in a detailed page.
- [ ] Names and terms match [Naming Conventions](naming.md).
- [ ] A new page is linked from the index of its section: the
  [Using Snapshot](../using-snapshot/README.md) for a guide, one line in the architecture
  overview for a detailed architecture page, and
  [docs/README.md](../README.md) for any other page.
- [ ] A version requirement beyond the baseline has a row in Prerequisites.
- [ ] A user-visible change has a CHANGELOG entry.
- [ ] Every relative link resolves.

Reviewers, human and AI, check changes against this page.
