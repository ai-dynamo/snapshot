// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

func init() {
	// Real intervals would make the establishment-wait tests slow; polling
	// faster than the fake client can possibly change state is safe in tests.
	establishedPollInterval = time.Millisecond
	establishedTimeout = 200 * time.Millisecond
}

func crdManifest(name string) string {
	return `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ` + name + `
spec:
  group: nvidia.com
  scope: Namespaced
`
}

// client.ApplyConfigurationFromUnstructured wraps the object in an unexported
// type that promotes the *unstructured.Unstructured methods.
type applyConfigUnstructured interface {
	GetName() string
	SetResourceVersion(string)
}

// Models the apiserver's resource version behaviour: it only bumps when the
// stored definition actually changes. controller-runtime's fake client cannot
// stand in, because its Apply bumps on every call including an identical
// re-apply, inverting the no-op case this installer exists to detect.
type fakeClient struct {
	stored               map[string]string // CRD name -> resource version
	nextRV               map[string]string // CRD name -> resource version the apply should yield
	applied              []string
	fieldMgr             string
	forced               bool
	applyErr             error
	getErr               error
	transientGetErrs     int // fail this many Gets of stored CRDs, then succeed
	getsUntilEstablished int // 0 means "established from the first Get"
	gets                 map[string]int
}

func newFakeClient() *fakeClient {
	return &fakeClient{stored: map[string]string{}, nextRV: map[string]string{}, gets: map[string]int{}}
}

func (f *fakeClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if f.getErr != nil {
		return f.getErr
	}
	rv, ok := f.stored[key.Name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{
			Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions",
		}, key.Name)
	}
	// Only Gets of stored CRDs fail, so the pre-apply existence check (which
	// hits the NotFound branch above for new CRDs) is unaffected and these
	// errors land in the establishment wait.
	if f.transientGetErrs > 0 {
		f.transientGetErrs--
		return errors.New("transient: connection reset")
	}
	obj.SetName(key.Name)
	obj.SetResourceVersion(rv)

	f.gets[key.Name]++
	if u, ok := obj.(*unstructured.Unstructured); ok {
		status := "False"
		if f.gets[key.Name] > f.getsUntilEstablished {
			status = "True"
		}
		_ = unstructured.SetNestedSlice(u.Object, []any{
			map[string]any{"type": "Established", "status": status},
		}, "status", "conditions")
	}
	return nil
}

func (f *fakeClient) Apply(_ context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	cfg, ok := obj.(applyConfigUnstructured)
	if !ok {
		return fmt.Errorf("apply configuration %T does not expose the unstructured object", obj)
	}

	applyOpts := &client.ApplyOptions{}
	applyOpts.ApplyOptions(opts)
	f.fieldMgr = applyOpts.FieldManager
	f.forced = applyOpts.Force != nil && *applyOpts.Force

	name := cfg.GetName()
	f.applied = append(f.applied, name)

	rv, ok := f.nextRV[name]
	if !ok {
		rv = f.stored[name]
	}
	f.stored[name] = rv
	cfg.SetResourceVersion(rv)
	return nil
}

func TestInstallCRDsCreatesMissingDefinition(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"

	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.NoError(t, err)
	assert.Equal(t, Results{{Name: "podsnapshots.nvidia.com", Action: ActionCreated}}, results)
	assert.True(t, results.Changed())
	assert.Equal(t, FieldManager, cl.fieldMgr)
	assert.True(t, cl.forced, "server-side apply should force ownership away from a previous manager")
}

func TestInstallCRDsUpdatesChangedDefinition(t *testing.T) {
	cl := newFakeClient()
	cl.stored["podsnapshots.nvidia.com"] = "7"
	cl.nextRV["podsnapshots.nvidia.com"] = "8"

	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.NoError(t, err)
	assert.Equal(t, Results{{Name: "podsnapshots.nvidia.com", Action: ActionUpdated}}, results)
	assert.True(t, results.Changed())
}

func TestInstallCRDsIsNoOpWhenUpToDate(t *testing.T) {
	cl := newFakeClient()
	cl.stored["podsnapshots.nvidia.com"] = "7"
	cl.stored["podsnapshotcontents.nvidia.com"] = "9"

	manifests := []string{
		crdManifest("podsnapshots.nvidia.com"),
		crdManifest("podsnapshotcontents.nvidia.com"),
	}
	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), manifests)

	require.NoError(t, err)
	assert.Equal(t, Results{
		{Name: "podsnapshots.nvidia.com", Action: ActionUnchanged},
		{Name: "podsnapshotcontents.nvidia.com", Action: ActionUnchanged},
	}, results)
	assert.False(t, results.Changed())
}

func TestInstallCRDsRejectsNonCRDManifest(t *testing.T) {
	cl := newFakeClient()
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nope\n"

	_, err := InstallCRDs(t.Context(), cl, logr.Discard(), []string{manifest})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected kind CustomResourceDefinition")
	assert.Empty(t, cl.applied)
}

func TestInstallCRDsStopsOnApplyError(t *testing.T) {
	cl := newFakeClient()
	cl.applyErr = errors.New("boom")

	manifests := []string{
		crdManifest("podsnapshots.nvidia.com"),
		crdManifest("podsnapshotcontents.nvidia.com"),
	}
	_, err := InstallCRDs(t.Context(), cl, logr.Discard(), manifests)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `apply CRD "podsnapshots.nvidia.com"`)
	assert.Empty(t, cl.applied)
}

func TestInstallCRDsPropagatesGetError(t *testing.T) {
	cl := newFakeClient()
	cl.getErr = errors.New("api unreachable")

	_, err := InstallCRDs(t.Context(), cl, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "api unreachable")
	assert.Empty(t, cl.applied)
}

func TestInstallCRDsWaitsForEstablished(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"
	cl.getsUntilEstablished = 3 // first three post-apply Gets report not-yet-established

	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.NoError(t, err)
	assert.Equal(t, Results{{Name: "podsnapshots.nvidia.com", Action: ActionCreated}}, results)
	assert.Greater(t, cl.gets["podsnapshots.nvidia.com"], 3,
		"installer should have polled past the not-established responses")
}

func TestInstallCRDsTimesOutWaitingForEstablished(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"
	cl.getsUntilEstablished = 1 << 30 // never reports Established within the test timeout

	_, err := InstallCRDs(t.Context(), cl, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `wait for CRD "podsnapshots.nvidia.com" to become established`)
	assert.Contains(t, err.Error(), "last observed conditions")
	assert.Contains(t, err.Error(), `"status":"False"`)
}

func TestInstallCRDsToleratesTransientGetErrorsWhileWaiting(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"
	cl.transientGetErrs = 2 // first two polls of the establishment wait fail

	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.NoError(t, err)
	assert.Equal(t, Results{{Name: "podsnapshots.nvidia.com", Action: ActionCreated}}, results)
}

func TestInstallCRDsTimeoutReportsLastGetError(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"
	cl.transientGetErrs = 1 << 30 // every poll fails until the wait times out

	_, err := InstallCRDs(t.Context(), cl, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `wait for CRD "podsnapshots.nvidia.com" to become established`)
	assert.Contains(t, err.Error(), "transient: connection reset")
}

func TestEmbeddedCRDsApplyCleanly(t *testing.T) {
	manifests := crds.All()
	require.NotEmpty(t, manifests)

	// Derive expected names by parsing each manifest directly — independent of
	// InstallCRDs, so a bug that mislabels every applied CRD the same way
	// couldn't slip past this test.
	wantNames := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		obj := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal([]byte(manifest), &obj.Object))
		name := obj.GetName()
		require.NotEmpty(t, name, "manifest has no metadata.name: %s", manifest)
		wantNames = append(wantNames, name)
	}

	cl := newFakeClient()
	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), manifests)

	require.NoError(t, err)
	require.Len(t, results, len(manifests), "expected one result per embedded CRD manifest")

	gotNames := make([]string, 0, len(results))
	for _, res := range results {
		assert.Equal(t, ActionCreated, res.Action)
		gotNames = append(gotNames, res.Name)
	}
	// InstallCRDs processes manifests in order and fakeClient.Apply records
	// calls in the order they happen, so ordered equality is both correct and
	// strictly stronger than ElementsMatch — it also catches a manifest being
	// applied under the wrong name (e.g. an index mix-up), which membership
	// alone would miss.
	assert.Equal(t, wantNames, gotNames)
	assert.Equal(t, wantNames, cl.applied)
}

var _ Client = (*fakeClient)(nil)
var _ applyConfigUnstructured = client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{}).(applyConfigUnstructured)
