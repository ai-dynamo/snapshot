// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

// applyConfigUnstructured recovers the object behind a runtime.ApplyConfiguration
// built by client.ApplyConfigurationFromUnstructured, which wraps it in an
// unexported type that promotes the *unstructured.Unstructured methods.
type applyConfigUnstructured interface {
	GetName() string
	SetResourceVersion(string)
}

// fakeClient records applies and models the API server's resource version
// behaviour: it only bumps when the stored definition actually changes.
type fakeClient struct {
	stored   map[string]string // CRD name -> resource version
	nextRV   map[string]string // CRD name -> resource version the apply should yield
	applied  []string
	fieldMgr string
	forced   bool
	applyErr error
	getErr   error
}

func newFakeClient() *fakeClient {
	return &fakeClient{stored: map[string]string{}, nextRV: map[string]string{}}
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
	obj.SetName(key.Name)
	obj.SetResourceVersion(rv)
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

	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), [][]byte{[]byte(crdManifest("podsnapshots.nvidia.com"))})

	require.NoError(t, err)
	assert.Equal(t, []Result{{Name: "podsnapshots.nvidia.com", Action: ActionCreated}}, results)
	assert.True(t, Changed(results))
	assert.Equal(t, FieldManager, cl.fieldMgr)
	assert.True(t, cl.forced, "server-side apply should force ownership away from a previous manager")
}

func TestInstallCRDsUpdatesChangedDefinition(t *testing.T) {
	cl := newFakeClient()
	cl.stored["podsnapshots.nvidia.com"] = "7"
	cl.nextRV["podsnapshots.nvidia.com"] = "8"

	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), [][]byte{[]byte(crdManifest("podsnapshots.nvidia.com"))})

	require.NoError(t, err)
	assert.Equal(t, []Result{{Name: "podsnapshots.nvidia.com", Action: ActionUpdated}}, results)
	assert.True(t, Changed(results))
}

func TestInstallCRDsIsNoOpWhenUpToDate(t *testing.T) {
	cl := newFakeClient()
	cl.stored["podsnapshots.nvidia.com"] = "7"
	cl.stored["podsnapshotcontents.nvidia.com"] = "9"

	docs := [][]byte{
		[]byte(crdManifest("podsnapshots.nvidia.com")),
		[]byte(crdManifest("podsnapshotcontents.nvidia.com")),
	}
	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), docs)

	require.NoError(t, err)
	assert.Equal(t, []Result{
		{Name: "podsnapshots.nvidia.com", Action: ActionUnchanged},
		{Name: "podsnapshotcontents.nvidia.com", Action: ActionUnchanged},
	}, results)
	assert.False(t, Changed(results))
}

func TestInstallCRDsRejectsNonCRDManifest(t *testing.T) {
	cl := newFakeClient()
	doc := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nope\n")

	_, err := InstallCRDs(t.Context(), cl, logr.Discard(), [][]byte{doc})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected kind CustomResourceDefinition")
	assert.Empty(t, cl.applied)
}

func TestInstallCRDsStopsOnApplyError(t *testing.T) {
	cl := newFakeClient()
	cl.applyErr = errors.New("boom")

	docs := [][]byte{
		[]byte(crdManifest("podsnapshots.nvidia.com")),
		[]byte(crdManifest("podsnapshotcontents.nvidia.com")),
	}
	_, err := InstallCRDs(t.Context(), cl, logr.Discard(), docs)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `apply CRD "podsnapshots.nvidia.com"`)
	assert.Empty(t, cl.applied)
}

func TestInstallCRDsPropagatesGetError(t *testing.T) {
	cl := newFakeClient()
	cl.getErr = errors.New("api unreachable")

	_, err := InstallCRDs(t.Context(), cl, logr.Discard(), [][]byte{[]byte(crdManifest("podsnapshots.nvidia.com"))})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "api unreachable")
	assert.Empty(t, cl.applied)
}

func TestLoadDirReadsYAMLFilesInOrderAndSkipsOthers(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a_first.yaml"), []byte("---\n"+crdManifest("a.nvidia.com")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b_second.yml"), []byte(crdManifest("b.nvidia.com")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("not a manifest"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested.yaml"), 0o755))

	docs, err := LoadDir(dir)

	require.NoError(t, err)
	require.Len(t, docs, 2)
	assert.Contains(t, string(docs[0]), "a.nvidia.com")
	assert.Contains(t, string(docs[1]), "b.nvidia.com")
}

func TestLoadDirSplitsMultiDocumentFiles(t *testing.T) {
	dir := t.TempDir()
	content := crdManifest("a.nvidia.com") + "---\n" + crdManifest("b.nvidia.com") + "---\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "both.yaml"), []byte(content), 0o600))

	docs, err := LoadDir(dir)

	require.NoError(t, err)
	require.Len(t, docs, 2, "the trailing empty document should be dropped")
}

func TestLoadDirReturnsEmptyForNoManifests(t *testing.T) {
	docs, err := LoadDir(t.TempDir())

	require.NoError(t, err)
	assert.Empty(t, docs)
}

func TestLoadDirErrorsOnMissingDirectory(t *testing.T) {
	_, err := LoadDir(filepath.Join(t.TempDir(), "absent"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read CRD directory")
}

// The chart's committed CRDs are what the init container mounts, so they must
// round-trip through the loader and the apply path.
func TestChartCRDsApplyCleanly(t *testing.T) {
	docs, err := LoadDir("../../../charts/snapshot/crds")
	require.NoError(t, err)
	require.NotEmpty(t, docs)

	cl := newFakeClient()
	results, err := InstallCRDs(t.Context(), cl, logr.Discard(), docs)

	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]string{"podsnapshots.nvidia.com", "podsnapshotcontents.nvidia.com"},
		cl.applied)
	for _, res := range results {
		assert.Equal(t, ActionCreated, res.Action)
	}
}

var _ Client = (*fakeClient)(nil)
var _ applyConfigUnstructured = client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{}).(applyConfigUnstructured)
