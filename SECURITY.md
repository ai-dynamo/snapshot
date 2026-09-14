<!--
SPDX-FileCopyrightText: Copyright (c) 2024-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

# Report a Security Vulnerability

To report a potential security vulnerability in any NVIDIA product, please use either:
* This web form: [Security Vulnerability Submission Form](https://www.nvidia.com/en-us/support/submit-security-vulnerability/), or
* Send email to: [NVIDIA PSIRT](mailto:psirt@nvidia.com)

If reporting a potential vulnerability via email, please encrypt it using NVIDIA’s public PGP key ([see PGP Key page](https://www.nvidia.com/en-us/security/pgp-key/)) and include the following information:
1. Product/Driver name and version/branch that contains the vulnerability
2. Type of vulnerability (code execution, denial of service, buffer overflow, etc.)
3. Instructions to reproduce the vulnerability
4. Proof-of-concept or exploit code
5. Potential impact of the vulnerability, including how an attacker could exploit the vulnerability

See https://www.nvidia.com/en-us/security/ for past NVIDIA Security Bulletins and Notices.

## Coordinated disclosure

NVIDIA PSIRT triages every report and coordinates any resulting fix and
disclosure under NVIDIA's coordinated vulnerability disclosure process. What
that process covers — how reports are evaluated, and what NVIDIA does and does
not commit to — is described on the
[NVIDIA PSIRT policies page](https://www.nvidia.com/en-us/security/psirt-policies/).
Snapshot does not set separate terms; that policy governs. Note in particular
that NVIDIA does not guarantee a specific resolution for every reported issue.

## Reporter credit

With the reporter's agreement, NVIDIA PSIRT may recognize them for a valid,
privately reported vulnerability — on NVIDIA's acknowledgement page, or in the
security bulletin for the issue. Recognition is discretionary and arranged by
PSIRT as part of the process above. It is never given through GitHub Security
Advisories; Snapshot does not publish those.

## Supported versions

Snapshot is pre-1.0. Security fixes are applied to the most recent minor release
line only.

If you are running a pre-release or an older patch version, upgrade to the latest
patch release on the supported line to pick up a fix. This policy is revisited at
1.0, when more than one line may be supported at a time.
