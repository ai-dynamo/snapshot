<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Upgrade Snapshot and the CRDs

Upgrade Snapshot with `helm upgrade`. The operator updates Snapshot's custom
resource definitions (CRDs) for you as part of the upgrade.

## Upgrade Snapshot

Replace `<version>` with the release you want from the
[releases page](https://github.com/ai-dynamo/snapshot/releases). Then upgrade in
one of two ways:

- **Pass your values again.** Run `helm upgrade` with the same `--set` values
  you installed with:

  ```bash
  helm upgrade snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
    --version <version> \
    --namespace snapshot
  ```

- **Keep your existing values.** Add `--reset-then-reuse-values`. Helm keeps the
  values of your current install and takes new defaults from the new chart:

  ```bash
  helm upgrade snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
    --version <version> \
    --namespace snapshot \
    --reset-then-reuse-values
  ```

During the upgrade:

- The node agent restarts on each GPU node.
- The operator restarts and updates the CRDs. See
  [How the CRDs Are Updated](#how-the-crds-are-updated).
- Snapshots taken with an older version still restore. If one cannot, the
  restore fails with a clear error.

If you use a private node agent image, set its pull secret in the same
`helm upgrade`. Otherwise the new chart defaults remove the image access the
agent needs.

## Supported Upgrades

<!--
Review: Oz and Dan to confirm. These points come from the upgrade e2e tests
(e2e/README.md, .github/workflows/e2e-upgrade.yaml), not from existing docs.
-->

- You can upgrade directly from the newest patch release of each of the last
  three minor versions.
- The order in which the parts finish does not matter. The new operator works
  with the old node agents, and the new node agents work with the old operator
  and CRDs, while the rollout completes.

## How the CRDs Are Updated

Helm creates the CRDs on a fresh install, then never updates them. To close that
gap, the operator runs a `crd-installer` init container before it starts. It
applies the CRDs that match the running operator, so every upgrade brings the
CRDs up to date. Nothing is written when nothing changed.

To check what it did:

```bash
kubectl logs deployment/snapshot-operator --namespace snapshot --container crd-installer
```

```text
Applied CRD  {"name": "podsnapshots.nvidia.com", "action": "unchanged"}
Applied CRD  {"name": "podsnapshotcontents.nvidia.com", "action": "unchanged"}
Applied CRD  {"name": "snapshotjobs.nvidia.com", "action": "unchanged"}
CRDs already up to date, no changes applied  {"count": 3}
```

The installer runs whenever the operator pod is recreated. A `helm upgrade`
recreates it when the operator image tag changes, and every release changes the
tag. If you pin a tag that does not change, such as `latest`, restart the
operator yourself to pick up new CRDs:

```bash
kubectl rollout restart deployment/snapshot-operator --namespace snapshot
```

With `rbac.create=true`, the operator gets permission to read and update the
three Snapshot CRDs, and to create a CRD. It cannot change a CRD it does not
own.

## Manage the CRDs Yourself

If a GitOps tool or a cluster admin manages the CRDs, set
`crdUpgrade.enabled=false`. This removes the init container and its extra
permissions. You then update the CRDs yourself after every chart upgrade:

```bash
helm pull oci://ghcr.io/ai-dynamo/snapshot/snapshot --version <version> --untar
kubectl apply --server-side --force-conflicts --filename snapshot/crds/
```
