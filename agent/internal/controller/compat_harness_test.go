// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const gatedRestoreContainer = "main"

func gatedRestoreMappings() []snapshotv1alpha1.RestoreContainerMapping {
	return []snapshotv1alpha1.RestoreContainerMapping{{
		Source:      gatedRestoreContainer,
		Destination: gatedRestoreContainer,
	}}
}

type comparisonCall struct {
	gate   compat.Gate
	source compat.Facts
	target compat.Facts
}

// comparisonSpy stands in for the policy table so a test can decide the verdict
// while the table itself is still being filled in.
type comparisonSpy struct {
	mismatches []compat.Mismatch
	calls      []comparisonCall
}

func (s *comparisonSpy) compare(gate compat.Gate, source, target compat.Facts) []compat.Mismatch {
	s.calls = append(s.calls, comparisonCall{gate: gate, source: source, target: target})
	return s.mismatches
}

// gatedRestore is one restore driven through the compatibility gates: a
// controller, the restore pod, the snapshot it names, the artifact it resolves
// to, and a stubbed comparison. Every test about what a gate does starts from
// one of these, whether the verdict it forces lets the restore through or not.
type gatedRestore struct {
	controller *NodeController
	pod        *corev1.Pod
	artifact   *restoreArtifact
	logs       *logRecorder
	comparison *comparisonSpy
}

func newGatedRestore(t *testing.T, mismatches ...compat.Mismatch) *gatedRestore {
	t.Helper()
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	logs := &logRecorder{}
	w.log = logr.New(&recordingSink{recorder: logs})
	comparison := &comparisonSpy{mismatches: mismatches}
	w.compareFn = comparison.compare

	path := writeTestArtifact(t, w.config.Storage.BasePath, string(content.UID), &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: gatedRestoreContainer},
	})

	return &gatedRestore{
		controller: w,
		pod:        pod,
		artifact: &restoreArtifact{
			SnapshotName:        snapshot.Name,
			ContentUID:          string(content.UID),
			SourceContainerName: gatedRestoreContainer,
			Path:                path,
		},
		logs:       logs,
		comparison: comparison,
	}
}

// writeTestArtifact creates the artifact directory a restore pod resolves to,
// optionally with a manifest in it.
func writeTestArtifact(t *testing.T, basePath, contentUID string, manifest *types.CheckpointManifest) string {
	t.Helper()
	path, err := nsmount.ResolveArtifactPath(basePath, contentUID, gatedRestoreContainer)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	if manifest != nil {
		require.NoError(t, types.WriteManifest(path, manifest))
	}
	return path
}

// reconcile drives the restore the way the queue worker does.
func (r *gatedRestore) reconcile(t *testing.T) {
	t.Helper()
	processQueuedRestorePod(t, r.controller, r.pod)
}

// runRestore drives the restore worker directly, which is where the second gate
// reports from.
func (r *gatedRestore) runRestore(t *testing.T) bool {
	t.Helper()
	return r.controller.restorePodContainers(
		context.Background(),
		r.pod,
		&restorePlan{artifact: r.artifact, mappings: gatedRestoreMappings()},
		fmt.Sprintf("%s/%s", r.pod.Namespace, r.pod.Name),
	)
}

func (r *gatedRestore) clientset(t *testing.T) *fake.Clientset {
	t.Helper()
	clientset, ok := r.controller.clientset.(*fake.Clientset)
	require.Truef(t, ok, "controller clientset is %T, want *fake.Clientset", r.controller.clientset)
	return clientset
}

// condition reads the condition off the last status apply, which is how the
// agent publishes a verdict.
func (r *gatedRestore) condition(t *testing.T) corev1.PodCondition {
	t.Helper()
	var applied struct {
		Status struct {
			Conditions []corev1.PodCondition `json:"conditions"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal(lastPodStatusApply(t, r.controller).GetPatch(), &applied))
	require.Len(t, applied.Status.Conditions, 1)
	return applied.Status.Conditions[0]
}

// events returns every event emitted under one reason, so a test can assert on
// how many there are and not only that there was one.
func (r *gatedRestore) events(t *testing.T, reason string) []*corev1.Event {
	t.Helper()
	return eventsForReason(r.clientset(t), reason)
}

type logRecord struct {
	message string
	fields  map[string]any
}

// logRecorder captures what the agent logged, so a test can assert on the field
// an operator greps for rather than on a formatted sentence.
type logRecorder struct {
	mu      sync.Mutex
	records []logRecord
}

func (r *logRecorder) add(message string, inherited, keysAndValues []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fields := map[string]any{}
	for _, pairs := range [][]any{inherited, keysAndValues} {
		for i := 0; i+1 < len(pairs); i += 2 {
			if key, ok := pairs[i].(string); ok {
				fields[key] = pairs[i+1]
			}
		}
	}
	r.records = append(r.records, logRecord{message: message, fields: fields})
}

// fieldsOf returns the fields of every record logged under one message,
// including the ones inherited from the logger.
func (r *logRecorder) fieldsOf(message string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var matched []map[string]any
	for _, record := range r.records {
		if record.message == message {
			matched = append(matched, record.fields)
		}
	}
	return matched
}

// refusalLog returns the fields of the one refusal the agent logged, which is
// what an operator reads to learn why a restore was turned down.
func (r *gatedRestore) refusalLog(t *testing.T) map[string]any {
	t.Helper()
	logged := r.logs.fieldsOf("Refusing restore; this node cannot run the checkpoint")
	require.Len(t, logged, 1)
	return logged[0]
}

type recordingSink struct {
	recorder *logRecorder
	values   []any
}

var _ logr.LogSink = (*recordingSink)(nil)

func (s *recordingSink) Init(logr.RuntimeInfo) {}
func (s *recordingSink) Enabled(int) bool      { return true }

func (s *recordingSink) Info(_ int, message string, keysAndValues ...any) {
	s.recorder.add(message, s.values, keysAndValues)
}

func (s *recordingSink) Error(err error, message string, keysAndValues ...any) {
	s.recorder.add(message, s.values, append(keysAndValues, "error", err))
}

// WithValues has to accumulate: the gates log through a logger that already
// carries the pod and container, and a test asserting on those must still see
// them on the record.
func (s *recordingSink) WithValues(keysAndValues ...any) logr.LogSink {
	values := make([]any, 0, len(s.values)+len(keysAndValues))
	values = append(values, s.values...)
	values = append(values, keysAndValues...)
	return &recordingSink{recorder: s.recorder, values: values}
}

func (s *recordingSink) WithName(string) logr.LogSink { return s }
