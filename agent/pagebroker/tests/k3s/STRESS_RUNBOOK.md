<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Endurance qualification and campaign

Step 5 prepares these commands. Astra must review the implementation and local
qualification evidence before the import/start commands below are executed.
No AWS deployment is part of step 5. Use an isolated, clean checkout of the test
PR above #564; all required runner code is committed in that PR.

## Build and identify the images locally

Run from the repository root with Docker available. The PageBroker Docker build
downloads the pinned Model Streamer release SDK and verifies its checksum;
no separate artifact-preparation target or local SDK directory is required. These
commands build both images from the selected checkout and override inherited
base-image revision labels. Rerun them if the intended revision changes.

```bash
set -euo pipefail
git diff --exit-code
git diff --cached --exit-code
export HARNESS_SHA="$(git rev-parse HEAD)"
export BROKER_SHA="$HARNESS_SHA"
export HARNESS_TAG="endurance-${HARNESS_SHA:0:12}"
export STRESS_IMAGE="local/pagebroker-stress:$HARNESS_TAG"
export PAGEBROKER_IMAGE="local/pagebroker:$HARNESS_TAG"
export REVIEW_DIR="$(mktemp -d /tmp/snapshot-endurance-review.XXXXXX)"
mkdir "$REVIEW_DIR/docker-config"
printf '%s\n' '{"auths":{}}' > "$REVIEW_DIR/docker-config/config.json"
export DOCKER_CONFIG="$REVIEW_DIR/docker-config"

make docker-build-pagebroker-stress REGISTRY=local VERSION="$HARNESS_TAG" \
  DOCKER_BUILD_ARGS="--load --label org.opencontainers.image.revision=$HARNESS_SHA"
make docker-build-pagebroker REGISTRY=local VERSION="$HARNESS_TAG" \
  DOCKER_BUILD_ARGS="--load --target runtime --label org.opencontainers.image.revision=$BROKER_SHA"
for image in "$STRESS_IMAGE" "$PAGEBROKER_IMAGE"; do
  docker image inspect "$image" --format \
    '{{.Id}} {{json .RepoTags}} {{json .RepoDigests}} {{index .Config.Labels "org.opencontainers.image.revision"}}'
done | tee "$REVIEW_DIR/image-identities.txt"
printf 'harness=%s\nbroker=%s\n' "$HARNESS_SHA" "$BROKER_SHA" \
  > "$REVIEW_DIR/source-revisions.txt"
```

Save the image IDs, source revisions, build logs and local qualification report
with the review. A local Docker image ID is a configuration digest, not a registry
manifest digest. Package the exact images into a reviewed OCI archive. The helper
checks source labels and configuration/layer hashes, then records the resulting
manifest digests. The archive includes tag and immutable reference aliases for
containerd; both references identify the same manifest.

```bash
docker image save "$STRESS_IMAGE" "$PAGEBROKER_IMAGE" -o "$REVIEW_DIR/docker-images.tar"
python3 -B agent/pagebroker/tests/k3s/stress_images.py \
  --docker-archive "$REVIEW_DIR/docker-images.tar" --output "$REVIEW_DIR/images" \
  --client-image "$STRESS_IMAGE" --pagebroker-image "$PAGEBROKER_IMAGE" \
  --harness-revision "$HARNESS_SHA" --broker-revision "$BROKER_SHA"
sha256sum "$REVIEW_DIR/images/images.oci.tar" "$REVIEW_DIR/images/identities.json" \
  > "$REVIEW_DIR/image-archive-checksums.txt"
```

Import that reviewed OCI archive. The launcher requires its `identities.json`,
uses immutable manifest references, and rejects missing node inventory or changed
terminal image identities. It treats CRI configuration IDs and repository-qualified
manifest IDs separately. Changing a tag does not change the selected content.

## Real local integration

The host needs Linux Docker with host networking, a working tmpfs local-volume
driver, OpenSSL, and Python with the existing pinned emulator requirements plus
the e2e YAML dependency. This command starts no Kubernetes resources and never
uses live AWS. It supplies fake credentials only to the loopback TLS emulator.

```bash
uv venv "$REVIEW_DIR/venv"
uv pip install --python "$REVIEW_DIR/venv/bin/python" \
  -r hack/pagebroker-integration/requirements.txt PyYAML==6.0.2
"$REVIEW_DIR/venv/bin/python" -B agent/pagebroker/tests/k3s/stress_local.py \
  --client-image "$STRESS_IMAGE" --pagebroker-image "$PAGEBROKER_IMAGE" \
  --harness-revision "$HARNESS_SHA" --broker-revision "$BROKER_SHA" \
  --output "$REVIEW_DIR/native"
```

Each backend has three warm-up batches, at least two measured batches, a
60-second measured interval, 12-second idle windows and final idle, and a
660-second hard budget. The native Go fixture verifies hashes, paths, types,
permissions and sizes on every restore. One original broker PID/start-time pair
must survive all batches and exit cleanly once. The command independently
revalidates saved manifests, journals, resource streams and shutdown evidence,
checks final tmpfs usage, and lists S3 versions/uploads after shutdown. Only the
owned S3 marker may remain. It records immutable image IDs and clean container
exits. Failed runs preserve reports and bounded role-log tails before removing
only their local containers/tmpfs volume. This is functional qualification;
`resourceAssessment` must remain `INCONCLUSIVE` for these short runs.

## AWS prerequisites after Astra review

Replace the placeholders below with settings for your test environment. Keep the
actual values in local configuration outside the repository. No production Helm
upgrade is required. Both Jobs run native PageBroker directly, and require no GPU
or Snapshot-agent S3 wiring.

```bash
export KUBECONFIG="/path/to/kubeconfig"
export CONTEXT="your-kubernetes-context"
export SNAPSHOT_REMOTE="ssh-user@your-test-host"
export SNAPSHOT_SSH_KEY="/path/to/ssh-private-key"
export NODE="your-kubernetes-node"
export S3_BUCKET="your-test-bucket"
export S3_REGION="your-aws-region"
export S3_PREFIX="pagebroker-endurance"
kubectl --kubeconfig "$KUBECONFIG" --context "$CONTEXT" get node "$NODE" -o wide
kubectl --kubeconfig "$KUBECONFIG" --context "$CONTEXT" get storageclass local-path
```

Both Jobs together request 2.8 CPUs and 2,816 MiB memory; their limits total
7.0 CPUs and 9 GiB memory. Two 1 GiB tmpfs staging mounts count within the
writers' memory limits. Existing allocations need additional headroom. Each
Job has 2 GiB local storage and a 2 GiB results PVC. The 512 MiB export budget
includes 128 MiB reserved for diagnostics. Allocate at least 2 GiB local free
space per collected backend for archive/extraction headroom. Node disk preflight
checks allocation, so also verify actual free node disk before a long campaign.

Credentials must permit bucket versioning inspection, bounded prefix listing,
object read/write/delete, object-version deletion, multipart create/parts/complete/
abort and multipart/part listing. Scope object permissions to the selected prefix.
The readiness probe checks the permissions actually needed by this bucket's
versioning state; it cannot validate a different future state. Fresh unique UUID
subprefixes are mandatory. Ownership markers and failed-batch data are retained.

Use an authenticated AWS CLI source that exports temporary credentials with a
known expiration. Do not print or save `export-credentials` output. Qualification
requires at least 35 minutes remaining at admission (1,200 + 900 seconds).
The full campaign requires at least 5 hours 15 minutes (18,000 + 900 seconds),
plus time spent preparing/submitting; obtain a session of at least six hours.
Shorter or unknown expiry is a blocker. Secret rotation does not refresh existing
PageBroker clients. Both launcher and helper recheck expiry. Never put credential
values in the profiles, command arguments, manifests or review artifacts.

For an S3-compatible test service with non-expiring keys, use the explicit
`--credentials-source static-environment` alternative documented in the README.
Export only its access key and secret key; session-token and expiry variables
must be unset or empty. Supply its HTTPS `--endpoint`, `--addressing path` and
trusted `--ca-bundle` as needed. Record `static-environment` and null expiry in
the run evidence. Temporary AWS credentials retain the lifetime requirements
above; static mode does not add runtime credential refresh. Rebuild/import the
harness image before launching either qualification or the longer campaign.

## Import the reviewed images and run qualification

Run only after review. Import exactly the two reviewed images into k3s.

```bash
gzip -1 -c "$REVIEW_DIR/images/images.oci.tar" |
  ssh -i "$SNAPSHOT_SSH_KEY" -o ConnectTimeout=10 -o ServerAliveInterval=30 \
    "$SNAPSHOT_REMOTE" \
    'bash -o pipefail -c "gzip -d | sudo -n k3s ctr --namespace k8s.io images import -"'

stressctl() {
  uv run --locked --project e2e \
    --with-requirements agent/pagebroker/tests/k3s/stress-requirements.txt \
    python agent/pagebroker/tests/k3s/stress_orchestrate.py \
    --kubeconfig "$KUBECONFIG" --context "$CONTEXT" "$@"
}
export QUALIFY_DIR="$(mktemp -d /tmp/snapshot-aws-qualification.XXXXXX)"
stressctl start --record "$QUALIFY_DIR/run.json" --backend both --node "$NODE" \
  --client-image "$STRESS_IMAGE" --pagebroker-image "$PAGEBROKER_IMAGE" \
  --harness-revision "$HARNESS_SHA" --broker-revision "$BROKER_SHA" \
  --image-identities "$REVIEW_DIR/images/identities.json" \
  --profile agent/pagebroker/tests/k3s/profiles/qualification.json \
  --bucket "$S3_BUCKET" --region "$S3_REGION" --prefix "$S3_PREFIX/qualification" \
  --credentials-source aws-cli
stressctl status --record "$QUALIFY_DIR/run.json"
```

Add `--aws-profile NAME` to `start` if using a named profile. The start command
returns after both baseline handshakes; Jobs then continue independently. Wait
until status shows both Jobs terminal before collecting. Use the same output
directory to resume a failed collection. A successful short qualification needs
execution `SUCCESS`, evidence `COMPLETE`, clean/no-restart container outcomes,
verified fixed broker identity and temporary Secret/exporter cleanup. Its resource
assessment remains `INCONCLUSIVE`.

```bash
stressctl collect --record "$QUALIFY_DIR/run.json" --output "$QUALIFY_DIR/collected"
```

## Four measured hours

Proceed only after the AWS qualification is independently reviewed. Create a
fresh record and profile. Record the tested broker revision's one-hour terminal
transaction retention and 120-second reap interval. These checked-in profiles
leave growth/absolute limits empty; consequently even a complete four-hour run
is `INCONCLUSIVE` for resource assessment. If limits are desired, declare them
after qualification and before starting; retain that exact profile for review.

```bash
export CAMPAIGN_DIR="$(mktemp -d /tmp/snapshot-aws-endurance.XXXXXX)"
python3 - "$BROKER_SHA" "$CAMPAIGN_DIR/profile.json" <<'PY'
import json
from pathlib import Path
import sys
profile = json.loads(Path('agent/pagebroker/tests/k3s/profiles/four-hour.json').read_text())
profile['evidence']['retention_revision'] = sys.argv[1]
Path(sys.argv[2]).write_text(json.dumps(profile, indent=2) + '\n')
PY
stressctl start --record "$CAMPAIGN_DIR/run.json" --backend both --node "$NODE" \
  --client-image "$STRESS_IMAGE" --pagebroker-image "$PAGEBROKER_IMAGE" \
  --harness-revision "$HARNESS_SHA" --broker-revision "$BROKER_SHA" \
  --image-identities "$REVIEW_DIR/images/identities.json" \
  --profile "$CAMPAIGN_DIR/profile.json" \
  --bucket "$S3_BUCKET" --region "$S3_REGION" --prefix "$S3_PREFIX/four-hour" \
  --credentials-source aws-cli
stressctl status --record "$CAMPAIGN_DIR/run.json"
```

Budget: startup 300 + warm-up maximum 1,800 + measured 14,400 + drain 420 +
cooldown 600 + shutdown 60 + margin 420 = 18,000 seconds. Minimum measured work
is 200 complete batches per backend. Expected nominal traffic at 240 measured S3
batches is approximately 90 GiB upload and 180 GiB download, excluding warm-up
and retries. Inspect actual batch/payload totals rather than treating estimates
as observed throughput. Retention is bounded to one successful batch plus its
owner marker; cumulative traffic is not bounded by retention.

After both Jobs stop, collect; for cancellation, use `stop` instead. A deliberate
stop drains the current batch, then shuts down, collects and reports `INTERRUPTED`.
It can take the remaining 420-second drain plus shutdown and host collection.

```bash
stressctl collect --record "$CAMPAIGN_DIR/run.json" --output "$CAMPAIGN_DIR/collected"
# To cancel instead of waiting for completion:
# stressctl stop --record "$CAMPAIGN_DIR/run.json" --output "$CAMPAIGN_DIR/stopped"
```

Keep the local records and collected root `summary.json`. If a record is lost,
`status`, `collect` and `stop` can recover with `--namespace` from either owned
namespace. Namespaces/PVCs and S3 markers/failed objects require a separate,
ownership-checked operator decision after evidence review; the collector removes
only temporary exporters and Secrets. Log tails are bounded diagnostic evidence.
Passing these runs establishes the observed functionality and any declared
resource bounds for this duration; it is never proof that no leaks exist.

PageBroker receives a mounted `[default]` shared credentials file. The launcher
adds that file to the run Secret; only PageBroker mounts it. The S3 cleanup
helper continues using explicit environment credentials from the same Secret.
Both use the admitted startup snapshot for the run.
