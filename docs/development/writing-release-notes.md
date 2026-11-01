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

# Writing release notes

How to write the notes for a Snapshot release, for people and for AI coding
agents. Names and terms are in [Naming conventions](naming.md); where other
facts live is in [Writing documentation](writing-docs.md). The first notes
written to this guide are [v0.2.0](../release-notes/v0.2.0.md). Use them as the
model for shape and tone.

Release notes answer one question for someone who runs Snapshot: what changed,
and what do I have to do? They are not a commit log, and they are not the
[CHANGELOG](../../CHANGELOG.md), which holds one short entry per change. A
release's notes are written once, at release time, from the changes in that
release.

## Where they live

- Write the notes in `docs/release-notes/vX.Y.Z.md` and review them in a PR
  before the release is cut.
- Publish the same text as the body of the GitHub release in
  [RELEASE.md](../../RELEASE.md) step 2.
- Notes for an upcoming release can start as a draft in the same place. Put a
  `> **Draft.**` line under the title that names the commit the list was built
  from, and remove it when the release is cut.

## Gather the facts

1. **Get the range.** List the commits between the previous tag and this one:
   `gh api repos/ai-dynamo/snapshot/compare/<previous>...<tag>`. For a draft, use
   `main` as the end. The release page's generated "What's Changed" list is a
   starting point only: it can stop before the last PRs, and it can leave out PRs
   that merged into a stacked branch. Reconcile it against the commit list.
2. **Check every claim against the tag, not `main`.** A flag, annotation or Helm
   value belongs in the notes only if it is in the tagged tree:
   `git grep -n <name> <tag> -- <path>`. Documentation on `main` can describe
   later changes. For a draft, check against `main` and say so in the banner.
3. **Treat the CHANGELOG as a source, not as proof.** Its `[Unreleased]` section
   can still carry entries for changes that already shipped.
4. **Read the PR description.** Its "How was this tested" and "not covered" lines
   say what is verified. A PR that says nothing uses its change yet, or that gates
   it behind a temporary flag, is groundwork.
5. **Compute the counts.** Take the number of PRs and contributors from the range
   you listed, not from the generated page. A contributor is a first-time
   contributor only if they have no merged PR before the previous release:
   `gh pr list --repo ai-dynamo/snapshot --author <login> --state merged`.

## Order by what matters most

- **Lead with the headline.** It is what users can do now that they could not
  before. Find it by comparing [Limitations](../limitations.md) and the README
  limitations between the two releases: a limitation that was removed is the
  headline. v0.2.0 removed "single GPU only" and "x86_64 only", so multi-GPU and
  arm64 lead its notes.
- **Then order by effect on the reader.** Capabilities that change what a user can
  run come first, then speed, then safety and reliability, then platform fit, then
  everything else.
- **Keep one order.** The introduction, the Summary, Features & Improvements and
  the Contributors lines follow the same order.
- **Upgrade Notes come before the feature lists.** They need action, so a reader
  should meet them first.

## Structure

Use these sections, in this order. Omit a section that would be empty, except
Known Issues, which says none were recorded.

| Section | What it holds |
| --- | --- |
| Title and introduction | `# Snapshot vX.Y.Z - Release Notes`, then one paragraph |
| Summary | One `###` subsection per headline theme |
| Baseline | The table of minimum versions, from the README prerequisites and [Compatibility](../reference/compatibility.md) |
| Upgrade Notes | Changes that need action or change behavior |
| Features & Improvements | `###` groups in the same order as the Summary |
| Bug Fixes | One bullet per fix users could notice |
| Testing & CI | Only changes a contributor or integrator would want to know |
| Documentation | New and changed pages |
| Known Issues | Defects, with a workaround where one exists |
| Contributors | Counts, thanks, and first-time contributors |
| Full Changelog | The compare link for the range |

## Write each part

**Introduction.** One paragraph: "Snapshot vX.Y.Z is the Nth minor release of
Snapshot, spanning N merged PRs from N contributors." Then name the headline in
bold, then the next three or four themes in bold, and end with a measured result
when there is one, with what it was measured on.

**Summary.** The heading is `### Theme: Detail & Detail`. The paragraph has the
capability as its subject, takes three to five sentences, and links at most four
PRs. Name a mechanism, such as a library or a protocol, only where the reader
must act on it.

**Features & Improvements.** One bullet per change, led by a bold feature name in
Title Case, then a past-tense sentence that says what the user gets, then the PR
link: `- **Opt-In Annotation:** Added the … annotation … (#123).` Use *Added*,
*Fixed*, *Refused*, *Reworked*, *Removed*. Say what changed for the user, not
which function changed.

**Upgrade Notes.** Describe what changes and who is affected, then give the action
as a quote that starts `> **Migrate:**`. Include every CRD change and every change
to checkpoint or restore semantics, since those affect existing snapshots. Include
a changed default that uses more node resources.

**Known Issues.** A known issue is a defect: something that fails or misbehaves
today. Give each one a `####` title, the symptom, the cause when it is known, and
a `**Workaround:**` line when one exists. Things Snapshot does not support by
design are limitations. They belong on the [Limitations](../limitations.md) page
and stay out of the release notes.

**Groundwork.** A change that nothing uses yet is not a feature. Either leave it
out, or list it in a short "Groundwork For What Comes Next" group at the end of
Features & Improvements, and say what is missing.

**Contributors.** State the counts, thank everyone, and give each first-time
contributor one line that says what they did.

## Words and tone

- Use the terms in [Naming conventions](naming.md): *Snapshot* is the product,
  *snapshot* is the saved state, *checkpoint* and *restore* are verbs, and use
  *snapshot store*, *source Pod*, *restore Pod*, *target container* and *node
  agent*. Do not write *capture* for something a user sees.
- Write in the past tense, in plain sentences. Do not use marketing words, and do
  not write "we".
- Give numbers with their unit and with what they were measured on, and link to
  the document that holds the data.
- Write names exactly as they appear: `nvidia.com/snapshot-multi-gpu`,
  `storage.type: s3`. Do not shorten them.
- Do not mention customers, partners or internal organizations or tools.

## Links

In a Markdown file in the repository, `#123` and `@name` do not turn into links.
Write each PR as `[#123](https://github.com/ai-dynamo/snapshot/pull/123)` and
each person as `[@name](https://github.com/name)`. The same text then works in the
file and in the GitHub release body. Link documents by their address at the tag
(`blob/vX.Y.Z/…`) when the page must match the release, and at `main` when the
page is meant to stay current.

## Before you publish

- Every number is traced to a source, and every flag, annotation and value is in
  the tag.
- Nothing from `docs/limitations.md` is in Known Issues.
- Groundwork is not described as a feature.
- Upgrade Notes cover every CRD change, changed default and changed semantic.
- Terms follow [Naming conventions](naming.md).
- Every link resolves, and the counts match the range.
- The order follows the headline, then user impact.
