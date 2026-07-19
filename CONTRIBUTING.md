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
