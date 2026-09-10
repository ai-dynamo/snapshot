<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# E2E benchmark dashboard

This directory contains the static dashboard for versioned Snapshot E2E
benchmark history. Vite and TypeScript are build-time tools only: GitHub Pages
serves the generated HTML, CSS, JavaScript, and benchmark index without a Node.js
server or runtime CDN dependency.

The UI discovers suites, cases, and measurements from the history data. It does
not contain a framework-specific result schema, so future E2E suites can publish
the same generic benchmark envelope and appear without new parsing code.

## Local development

Node.js 24 is used in CI. Install dependencies and start Vite from this
directory:

```bash
npm ci
npm run dev
```

The local server reads the small fixture history under `public/index`. Run all
local dashboard checks with:

```bash
npm run typecheck
npm test
npm run validate-data
npm run build
npm exec playwright -- install chromium
npm run test:browser
```

To validate a checked-out durable history branch instead of the fixture data:

```bash
npm run validate-data -- /path/to/e2e-benchmark-history/index
```

History JSON is treated as untrusted input. TypeScript checks the dashboard at
build time, while runtime parsing validates every manifest and NDJSON record.
Values are rendered through DOM text properties rather than raw HTML.

## GitHub Pages deployment

The `E2E Benchmark Dashboard` workflow builds source from `main`, fetches the
data-only `e2e-benchmark-history` branch, validates it, and copies its `index/`
directory into the static artifact. Fixture data is excluded from production
builds. The workflow deploys after dashboard changes and after every scheduled
framework workflow, including runs containing failed or partial benchmarks.

A repository administrator must enable Pages once in **Settings → Pages → Build
and deployment → Source → GitHub Actions**. No Node.js process runs after the
artifact is deployed.
