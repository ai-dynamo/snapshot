// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apiextensionsv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

func init() {
	// client-go's WatchListClient feature makes reflectors fetch initial state
	// through the watch (SendInitialEvents) instead of list+watch. The fake
	// clientset's watch ignores SendInitialEvents, so an informer never syncs
	// against it — force the legacy list+watch protocol in tests. Must be set
	// before client-go first reads its feature gates.
	os.Setenv("KUBE_FEATURE_WatchListClient", "false")

	// The real timeout would make the establishment-timeout tests slow; the
	// fake clientset delivers watch events in milliseconds.
	establishedTimeout = 2 * time.Second
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

func crdWithEstablished(name string, established bool) *apiextensionsv1.CustomResourceDefinition {
	status := apiextensionsv1.ConditionFalse
	if established {
		status = apiextensionsv1.ConditionTrue
	}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
				{Type: apiextensionsv1.Established, Status: status},
			},
		},
	}
}

// establishedCRDClient returns a CRD watch client whose tracker already holds
// an Established CRD for every given name, so waits succeed immediately.
func establishedCRDClient(names ...string) apiextensionsv1client.CustomResourceDefinitionInterface {
	objs := make([]runtime.Object, 0, len(names))
	for _, name := range names {
		objs = append(objs, crdWithEstablished(name, true))
	}
	return apiextensionsfake.NewSimpleClientset(objs...).ApiextensionsV1().CustomResourceDefinitions()
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
	crdClient := establishedCRDClient("podsnapshots.nvidia.com")

	results, err := InstallCRDs(t.Context(), cl, crdClient, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

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
	crdClient := establishedCRDClient("podsnapshots.nvidia.com")

	results, err := InstallCRDs(t.Context(), cl, crdClient, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.NoError(t, err)
	assert.Equal(t, Results{{Name: "podsnapshots.nvidia.com", Action: ActionUpdated}}, results)
	assert.True(t, results.Changed())
}

func TestInstallCRDsIsNoOpWhenUpToDate(t *testing.T) {
	cl := newFakeClient()
	cl.stored["podsnapshots.nvidia.com"] = "7"
	cl.stored["podsnapshotcontents.nvidia.com"] = "9"
	crdClient := establishedCRDClient("podsnapshots.nvidia.com", "podsnapshotcontents.nvidia.com")

	manifests := []string{
		crdManifest("podsnapshots.nvidia.com"),
		crdManifest("podsnapshotcontents.nvidia.com"),
	}
	results, err := InstallCRDs(t.Context(), cl, crdClient, logr.Discard(), manifests)

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

	_, err := InstallCRDs(t.Context(), cl, establishedCRDClient(), logr.Discard(), []string{manifest})

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
	_, err := InstallCRDs(t.Context(), cl, establishedCRDClient(), logr.Discard(), manifests)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `apply CRD "podsnapshots.nvidia.com"`)
	assert.Empty(t, cl.applied)
}

func TestInstallCRDsPropagatesGetError(t *testing.T) {
	cl := newFakeClient()
	cl.getErr = errors.New("api unreachable")

	_, err := InstallCRDs(t.Context(), cl, establishedCRDClient(), logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "api unreachable")
	assert.Empty(t, cl.applied)
}

func TestInstallCRDsWaitsForEstablished(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"
	fcs := apiextensionsfake.NewSimpleClientset(crdWithEstablished("podsnapshots.nvidia.com", false))
	crdClient := fcs.ApiextensionsV1().CustomResourceDefinitions()

	// Flip Established to True after the wait has started; the update arrives
	// as a watch event.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, err := crdClient.Update(context.Background(), crdWithEstablished("podsnapshots.nvidia.com", true), metav1.UpdateOptions{})
		if err != nil {
			t.Errorf("update CRD status: %v", err)
		}
	}()

	results, err := InstallCRDs(t.Context(), cl, crdClient, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.NoError(t, err)
	assert.Equal(t, Results{{Name: "podsnapshots.nvidia.com", Action: ActionCreated}}, results)
}

func TestInstallCRDsTimesOutWaitingForEstablished(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"
	fcs := apiextensionsfake.NewSimpleClientset(crdWithEstablished("podsnapshots.nvidia.com", false))

	_, err := InstallCRDs(t.Context(), cl, fcs.ApiextensionsV1().CustomResourceDefinitions(), logr.Discard(),
		[]string{crdManifest("podsnapshots.nvidia.com")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `wait for CRD "podsnapshots.nvidia.com" to become established`)
	assert.Contains(t, err.Error(), "last observed conditions")
	assert.Contains(t, err.Error(), `"status":"False"`)
}

func TestInstallCRDsFailsFastOnForbiddenListWatch(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"
	fcs := apiextensionsfake.NewSimpleClientset()
	forbidden := apierrors.NewForbidden(schema.GroupResource{
		Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions",
	}, "", errors.New("crd-installer lacks list/watch RBAC"))
	fcs.PrependReactor("list", "customresourcedefinitions", func(clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbidden
	})

	start := time.Now()
	_, err := InstallCRDs(t.Context(), cl, fcs.ApiextensionsV1().CustomResourceDefinitions(), logr.Discard(),
		[]string{crdManifest("podsnapshots.nvidia.com")})

	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err), "underlying Forbidden must survive wrapping, got: %v", err)
	assert.Contains(t, err.Error(), "lacks list/watch RBAC")
	assert.Less(t, time.Since(start), establishedTimeout,
		"an authorization error must fail the wait immediately, not ride out the timeout")
}

func TestInstallCRDsTimeoutIncludesLastListWatchError(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"
	fcs := apiextensionsfake.NewSimpleClientset()
	fcs.PrependReactor("list", "customresourcedefinitions", func(clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	_, err := InstallCRDs(t.Context(), cl, fcs.ApiextensionsV1().CustomResourceDefinitions(), logr.Discard(),
		[]string{crdManifest("podsnapshots.nvidia.com")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `wait for CRD "podsnapshots.nvidia.com" to become established`)
	assert.Contains(t, err.Error(), "last list/watch error")
	assert.Contains(t, err.Error(), "connection refused")
}

func TestInstallCRDsFailsWhenCRDDeletedWhileWaiting(t *testing.T) {
	cl := newFakeClient()
	cl.nextRV["podsnapshots.nvidia.com"] = "1"
	fcs := apiextensionsfake.NewSimpleClientset(crdWithEstablished("podsnapshots.nvidia.com", false))
	crdClient := fcs.ApiextensionsV1().CustomResourceDefinitions()

	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := crdClient.Delete(context.Background(), "podsnapshots.nvidia.com", metav1.DeleteOptions{}); err != nil {
			t.Errorf("delete CRD: %v", err)
		}
	}()

	_, err := InstallCRDs(t.Context(), cl, crdClient, logr.Discard(), []string{crdManifest("podsnapshots.nvidia.com")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "was deleted while waiting")
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
	results, err := InstallCRDs(t.Context(), cl, establishedCRDClient(wantNames...), logr.Discard(), manifests)

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
