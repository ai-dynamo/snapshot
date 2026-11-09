<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Documentation site preview

This directory holds a [Fern](https://buildwithfern.com) configuration that shows the docs navigation as a site, so reviewers can read the structure. It is for review only and is removed before the pull request merges.

The site uses the Fern NVIDIA global theme, which requires a Fern account with access to the `nvidia` organization.

## Preview locally

```bash
npm install -g fern-api@5.168.0
fern login
fern/preview.sh
```

The script copies the docs to a temporary folder, fixes the few things Fern cannot read in the copy, and starts `fern docs dev`. The files in the repository are not changed. Use `fern/preview.sh check` to validate the configuration.
