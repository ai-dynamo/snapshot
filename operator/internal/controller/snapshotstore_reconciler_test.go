// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/ai-dynamo/snapshot/api/storage"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/storageprobe"
)

const storeNamespace = "snapshot"

type fakeProber struct {
	targets []storageprobe.Target
	err     error
}

func (p *fakeProber) Check(_ context.Context, target storageprobe.Target) error {
	p.targets = append(p.targets, target)
	return p.err
}

type storeFixture struct {
	r        *SnapshotStoreReconciler
	c        client.Client
	prober   *fakeProber
	recorder *record.FakeRecorder
}

func newStoreFixture(t *testing.T, objects ...client.Object) *storeFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&snapshotv1alpha1.SnapshotStore{}).Build()
	prober := &fakeProber{}
	recorder := record.NewFakeRecorder(32)
	return &storeFixture{
		r: &SnapshotStoreReconciler{
			Client: c, APIReader: c, Recorder: recorder, Prober: prober,
			Namespace: storeNamespace, ClusterID: "cluster-a", DefaultStoreName: "default", BindingEnabled: true,
			MountPath: func(claim string) (string, bool) {
				if claim == "snapshot-pvc" {
					return "/checkpoints", true
				}
				return "", false
			},
		},
		c: c, prober: prober, recorder: recorder,
	}
}

func (f *storeFixture) reconcile(t *testing.T, name string) ctrl.Result {
	t.Helper()
	result, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err)
	return result
}

func (f *storeFixture) get(t *testing.T, name string) *snapshotv1alpha1.SnapshotStore {
	t.Helper()
	store := &snapshotv1alpha1.SnapshotStore{}
	require.NoError(t, f.c.Get(t.Context(), types.NamespacedName{Name: name}, store))
	return store
}

func (f *storeFixture) ready(t *testing.T, name string) *metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(f.get(t, name).Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, cond)
	return cond
}

func pvcStoreObject(name string, created time.Time) *snapshotv1alpha1.SnapshotStore {
	return &snapshotv1alpha1.SnapshotStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created), Generation: 1},
		Spec: snapshotv1alpha1.SnapshotStoreSpec{
			ReclaimPolicy: snapshotv1alpha1.ReclaimPolicyDelete,
			PVC:           &snapshotv1alpha1.PVCStoreSpec{ClaimName: "snapshot-pvc", SubPath: "/team-a"},
		},
	}
}

func s3StoreObject(name string, created time.Time) *snapshotv1alpha1.SnapshotStore {
	return &snapshotv1alpha1.SnapshotStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created), Generation: 1},
		Spec: snapshotv1alpha1.SnapshotStoreSpec{
			ReclaimPolicy: snapshotv1alpha1.ReclaimPolicyDelete,
			S3: &snapshotv1alpha1.S3StoreSpec{
				Endpoint: "https://s3.example.com", Region: "region-1", Bucket: "snapshots", Prefix: "team-a",
			},
		},
	}
}

var baseTime = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func TestStoreBecomesReadyAndRecordsItsStoreID(t *testing.T) {
	f := newStoreFixture(t, pvcStoreObject("default", baseTime))

	result := f.reconcile(t, "default")

	store := f.get(t, "default")
	want, err := storage.PVC{Namespace: storeNamespace, ClaimName: "snapshot-pvc", BasePath: "/team-a"}.StoreID()
	require.NoError(t, err)
	assert.Equal(t, want, store.Status.StoreID)
	assert.Contains(t, store.Finalizers, SnapshotStoreProtectionFinalizer)
	cond := f.ready(t, "default")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, ReasonStoreReachable, cond.Reason)
	assert.EqualValues(t, 1, cond.ObservedGeneration)
	assert.Equal(t, defaultStoreCheckInterval, result.RequeueAfter)
	require.Len(t, f.prober.targets, 1)
	assert.Equal(t, "cluster-a", f.prober.targets[0].ClusterID)
	assert.Equal(t, &storageprobe.PVC{MountPath: "/checkpoints", BasePath: "/team-a"}, f.prober.targets[0].PVC)
}

func TestStoreWithoutASubPathUsesTheWholeClaim(t *testing.T) {
	store := pvcStoreObject("default", baseTime)
	store.Spec.PVC.SubPath = ""
	f := newStoreFixture(t, store)

	f.reconcile(t, "default")

	require.Len(t, f.prober.targets, 1)
	assert.Equal(t, "/", f.prober.targets[0].PVC.BasePath)
}

func TestStoreOnAClaimTheOperatorHasNotMountedIsNotReady(t *testing.T) {
	store := pvcStoreObject("other", baseTime)
	store.Spec.PVC.ClaimName = "other-pvc"
	f := newStoreFixture(t, store)

	f.reconcile(t, "other")

	cond := f.ready(t, "other")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, ReasonClaimNotMounted, cond.Reason)
	assert.Empty(t, f.prober.targets)
}

func TestStoreWithAnInvalidLocationIsNotReady(t *testing.T) {
	store := s3StoreObject("bad", baseTime)
	store.Spec.S3.Endpoint = "https://s3.example.com/with/a/path"
	f := newStoreFixture(t, store)

	result := f.reconcile(t, "bad")

	cond := f.ready(t, "bad")
	assert.Equal(t, ReasonInvalidLocation, cond.Reason)
	assert.Empty(t, f.get(t, "bad").Status.StoreID)
	assert.Zero(t, result.RequeueAfter, "a location cannot change, so there is nothing to retry")
	assert.Empty(t, f.prober.targets)
}

func TestLaterStoreAtTheSameLocationIsADuplicate(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("first", baseTime), s3StoreObject("second", baseTime.Add(time.Second)))

	f.reconcile(t, "first")
	f.reconcile(t, "second")

	assert.Equal(t, metav1.ConditionTrue, f.ready(t, "first").Status)
	cond := f.ready(t, "second")
	assert.Equal(t, ReasonDuplicateStore, cond.Reason)
	assert.Contains(t, cond.Message, `"first"`)
	assert.Equal(t, f.get(t, "first").Status.StoreID, f.get(t, "second").Status.StoreID)
	assert.Len(t, f.prober.targets, 1, "a duplicate is refused before any check")
}

func TestStoresCreatedAtTheSameTimeAreOrderedByName(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("b", baseTime), s3StoreObject("a", baseTime))

	f.reconcile(t, "b")
	f.reconcile(t, "a")

	assert.Equal(t, metav1.ConditionTrue, f.ready(t, "a").Status)
	assert.Equal(t, ReasonDuplicateStore, f.ready(t, "b").Reason)
}

func TestDuplicateIsRetriedWhenTheOtherStoreIsDeleted(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("first", baseTime), s3StoreObject("second", baseTime.Add(time.Second)))
	f.reconcile(t, "second")
	require.Equal(t, ReasonDuplicateStore, f.ready(t, "second").Reason)

	first := f.get(t, "first")
	requests := f.r.storesAtTheSameLocation(t.Context(), first)
	require.Equal(t, []types.NamespacedName{{Name: "second"}}, namesOf(requests))
	require.NoError(t, f.c.Delete(t.Context(), first))
	f.reconcile(t, "second")

	assert.Equal(t, metav1.ConditionTrue, f.ready(t, "second").Status)
}

func TestStoresAtDifferentLocationsDoNotCollide(t *testing.T) {
	other := s3StoreObject("other", baseTime.Add(time.Second))
	other.Spec.S3.Prefix = "team-b"
	f := newStoreFixture(t, s3StoreObject("first", baseTime), other)

	f.reconcile(t, "first")
	f.reconcile(t, "other")

	assert.Equal(t, metav1.ConditionTrue, f.ready(t, "other").Status)
	assert.Empty(t, f.r.storesAtTheSameLocation(t.Context(), other))
}

func TestFailedAccessCheckNamesTheStep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"store", &storageprobe.Failure{Step: storageprobe.StepWrite, Err: errors.New("denied")}, ReasonStoreUnreachable},
		{"credentials", &storageprobe.Failure{Step: storageprobe.StepWrite, Credentials: true, Err: errors.New("expired")}, ReasonCredentialsUnavailable},
		{"unclassified", errors.New("boom"), ReasonStoreUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStoreFixture(t, s3StoreObject("default", baseTime))
			f.prober.err = tc.err

			result := f.reconcile(t, "default")

			cond := f.ready(t, "default")
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, tc.reason, cond.Reason)
			assert.Equal(t, tc.err.Error(), cond.Message)
			assert.Equal(t, storeRetryBase, result.RequeueAfter)
			assert.NotEmpty(t, f.recorder.Events)
		})
	}
}

func TestRetriesBackOffAndResetOnSuccess(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("default", baseTime))
	f.prober.err = errors.New("unreachable")

	var delays []time.Duration
	for range 8 {
		delays = append(delays, f.reconcile(t, "default").RequeueAfter)
	}
	assert.Equal(t, []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}, delays)

	f.prober.err = nil
	assert.Equal(t, defaultStoreCheckInterval, f.reconcile(t, "default").RequeueAfter)
	f.prober.err = errors.New("unreachable")
	assert.Equal(t, storeRetryBase, f.reconcile(t, "default").RequeueAfter)
}

func TestFailureEventIsEmittedOncePerReason(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("default", baseTime))
	f.prober.err = errors.New("unreachable")

	f.reconcile(t, "default")
	f.reconcile(t, "default")

	assert.Len(t, f.recorder.Events, 1)
}

func s3WithSecretAndCA(name string) *snapshotv1alpha1.SnapshotStore {
	store := s3StoreObject(name, baseTime)
	store.Spec.S3.CredentialsSecretRef = &snapshotv1alpha1.SecretKeyReference{Name: "s3-credentials", Key: "credentials"}
	store.Spec.S3.CABundleConfigMapRef = &snapshotv1alpha1.ConfigMapKeyReference{Name: "s3-ca", Key: "ca.crt"}
	store.Spec.S3.Addressing = "path"
	store.Spec.S3.AllowHTTP = true
	return store
}

func TestS3StorePassesCredentialsAndCAFromTheSnapshotNamespace(t *testing.T) {
	f := newStoreFixture(t, s3WithSecretAndCA("default"),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: storeNamespace, Name: "s3-credentials"},
			Data:       map[string][]byte{"credentials": []byte("[default]\naws_access_key_id = A\naws_secret_access_key = B\n")},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: storeNamespace, Name: "s3-ca"},
			Data:       map[string]string{"ca.crt": "PEM"},
		})

	f.reconcile(t, "default")

	require.Len(t, f.prober.targets, 1)
	s3 := f.prober.targets[0].S3
	require.NotNil(t, s3)
	assert.Equal(t, "[default]\naws_access_key_id = A\naws_secret_access_key = B\n", string(s3.Credentials))
	assert.Equal(t, "PEM", string(s3.CABundle))
	assert.True(t, s3.PathStyle)
	assert.True(t, s3.AllowHTTP)
	assert.Equal(t, "team-a", s3.Prefix)
}

func TestS3StoreWithoutASecretUsesTheWorkloadIdentity(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("default", baseTime))

	f.reconcile(t, "default")

	require.Len(t, f.prober.targets, 1)
	assert.Empty(t, f.prober.targets[0].S3.Credentials)
	assert.False(t, f.prober.targets[0].S3.PathStyle)
}

func TestMissingSecretOrKeyMeansTheCredentialsAreUnavailable(t *testing.T) {
	t.Run("secret", func(t *testing.T) {
		f := newStoreFixture(t, s3WithSecretAndCA("default"))
		f.reconcile(t, "default")
		assert.Equal(t, ReasonCredentialsUnavailable, f.ready(t, "default").Reason)
		assert.Empty(t, f.prober.targets)
	})
	t.Run("key", func(t *testing.T) {
		f := newStoreFixture(t, s3WithSecretAndCA("default"), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: storeNamespace, Name: "s3-credentials"},
			Data:       map[string][]byte{"other": []byte("x")},
		})
		f.reconcile(t, "default")
		cond := f.ready(t, "default")
		assert.Equal(t, ReasonCredentialsUnavailable, cond.Reason)
		assert.Contains(t, cond.Message, `no key "credentials"`)
	})
	t.Run("secret in another namespace", func(t *testing.T) {
		f := newStoreFixture(t, s3WithSecretAndCA("default"), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "s3-credentials"},
			Data:       map[string][]byte{"credentials": []byte("x")},
		})
		f.reconcile(t, "default")
		assert.Equal(t, ReasonCredentialsUnavailable, f.ready(t, "default").Reason)
	})
}

func TestMissingCABundleIsAStoreProblem(t *testing.T) {
	f := newStoreFixture(t, s3WithSecretAndCA("default"), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: storeNamespace, Name: "s3-credentials"},
		Data:       map[string][]byte{"credentials": []byte("x")},
	})

	f.reconcile(t, "default")

	assert.Equal(t, ReasonStoreUnreachable, f.ready(t, "default").Reason)
}

func TestFailureMessageNeverContainsTheCredentials(t *testing.T) {
	f := newStoreFixture(t, s3WithSecretAndCA("default"),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: storeNamespace, Name: "s3-credentials"},
			Data:       map[string][]byte{"credentials": []byte("SECRETVALUE")},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: storeNamespace, Name: "s3-ca"},
			Data:       map[string]string{"ca.crt": "PEM"},
		})
	f.prober.err = &storageprobe.Failure{Step: storageprobe.StepWrite, Credentials: true, Err: errors.New("the credentials were refused")}

	f.reconcile(t, "default")

	assert.NotContains(t, f.ready(t, "default").Message, "SECRETVALUE")
}

func TestSecretAndConfigMapChangesRecheckTheStoresThatUseThem(t *testing.T) {
	other := s3StoreObject("other", baseTime)
	other.Spec.S3.Prefix = "team-b"
	f := newStoreFixture(t, s3WithSecretAndCA("default"), other)

	secret := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: storeNamespace, Name: "s3-credentials"}}
	configMap := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: storeNamespace, Name: "s3-ca"}}
	elsewhere := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "s3-credentials"}}

	assert.Equal(t, []types.NamespacedName{{Name: "default"}}, namesOf(f.r.storesUsingSecret(t.Context(), secret)))
	assert.Equal(t, []types.NamespacedName{{Name: "default"}}, namesOf(f.r.storesUsingConfigMap(t.Context(), configMap)))
	assert.Empty(t, f.r.storesUsingSecret(t.Context(), elsewhere))
	assert.Empty(t, f.r.storesUsingSecret(t.Context(), configMap))
}

func TestChangingASecretResetsTheBackoff(t *testing.T) {
	f := newStoreFixture(t, s3WithSecretAndCA("default"))
	f.reconcile(t, "default")
	f.reconcile(t, "default")
	require.Equal(t, 2, f.r.failures["default"])

	f.r.storesUsingSecret(t.Context(),
		&metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: storeNamespace, Name: "s3-credentials"}})

	assert.Zero(t, f.r.failures["default"])
}

func TestBindingOffReconcilesOnlyTheDefaultStore(t *testing.T) {
	other := s3StoreObject("other", baseTime)
	other.Spec.S3.Prefix = "team-b"
	f := newStoreFixture(t, pvcStoreObject("default", baseTime), other)
	f.r.BindingEnabled = false

	f.reconcile(t, "default")
	f.reconcile(t, "other")

	assert.Equal(t, metav1.ConditionTrue, f.ready(t, "default").Status)
	assert.Empty(t, f.get(t, "other").Status.Conditions)
	assert.Empty(t, f.get(t, "other").Finalizers)
}

func contentFor(name, storeName, storeID string) *snapshotv1alpha1.PodSnapshotContent {
	return &snapshotv1alpha1.PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: snapshotv1alpha1.PodSnapshotContentSpec{
			Storage: &snapshotv1alpha1.CheckpointStorageBinding{StoreID: storeID, SnapshotStoreName: storeName},
		},
	}
}

func TestStoreIsKeptWhileContentNamesIt(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("default", baseTime))
	f.reconcile(t, "default")
	storeID := f.get(t, "default").Status.StoreID
	require.NoError(t, f.c.Create(t.Context(), contentFor("by-name", "default", storeID)))
	require.NoError(t, f.c.Create(t.Context(), contentFor("other-store", "elsewhere", storeID)))
	require.NoError(t, f.c.Delete(t.Context(), f.get(t, "default")))

	result := f.reconcile(t, "default")

	store := f.get(t, "default")
	assert.Contains(t, store.Finalizers, SnapshotStoreProtectionFinalizer)
	cond := f.ready(t, "default")
	assert.Equal(t, ReasonTerminating, cond.Reason)
	assert.Equal(t, "1 PodSnapshotContent objects still use this store", cond.Message)
	assert.Equal(t, storeTerminatingRecheck, result.RequeueAfter)
	assert.NotEmpty(t, f.recorder.Events)
}

func TestStoreIsKeptWhileContentWithoutAStoreNameCarriesItsStoreID(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("default", baseTime))
	f.reconcile(t, "default")
	storeID := f.get(t, "default").Status.StoreID
	require.NoError(t, f.c.Create(t.Context(), contentFor("by-digest", "", storeID)))
	require.NoError(t, f.c.Delete(t.Context(), f.get(t, "default")))

	f.reconcile(t, "default")

	assert.Equal(t, ReasonTerminating, f.ready(t, "default").Reason)
}

func TestStoreIsReleasedWhenNoContentUsesIt(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("default", baseTime))
	f.reconcile(t, "default")
	storeID := f.get(t, "default").Status.StoreID
	require.NoError(t, f.c.Create(t.Context(), contentFor("elsewhere", "elsewhere", storeID)))
	require.NoError(t, f.c.Create(t.Context(), &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}}))
	require.NoError(t, f.c.Delete(t.Context(), f.get(t, "default")))

	f.reconcile(t, "default")

	err := f.c.Get(t.Context(), types.NamespacedName{Name: "default"}, &snapshotv1alpha1.SnapshotStore{})
	assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "the store is gone once the finalizer is removed")
}

func TestStoreIsReleasedAfterItsLastContentIsDeleted(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("default", baseTime))
	f.reconcile(t, "default")
	storeID := f.get(t, "default").Status.StoreID
	content := contentFor("by-name", "default", storeID)
	require.NoError(t, f.c.Create(t.Context(), content))
	require.NoError(t, f.c.Delete(t.Context(), f.get(t, "default")))
	f.reconcile(t, "default")
	require.Equal(t, ReasonTerminating, f.ready(t, "default").Reason)

	require.NoError(t, f.c.Delete(t.Context(), content))
	f.reconcile(t, "default")

	err := f.c.Get(t.Context(), types.NamespacedName{Name: "default"}, &snapshotv1alpha1.SnapshotStore{})
	assert.Error(t, err)
}

func TestTerminatingStoreIsProtectedEvenWhenBindingIsOff(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("other", baseTime))
	f.r.BindingEnabled = true
	f.reconcile(t, "other")
	storeID := f.get(t, "other").Status.StoreID
	require.NoError(t, f.c.Create(t.Context(), contentFor("by-name", "other", storeID)))
	require.NoError(t, f.c.Delete(t.Context(), f.get(t, "other")))
	f.r.BindingEnabled = false

	f.reconcile(t, "other")

	assert.Equal(t, ReasonTerminating, f.ready(t, "other").Reason)
}

func TestReconcileOfAMissingStoreForgetsIt(t *testing.T) {
	f := newStoreFixture(t)
	f.r.failures = map[string]int{"gone": 3}

	f.reconcile(t, "gone")

	assert.Empty(t, f.r.failures)
}

func TestRetryDelay(t *testing.T) {
	assert.Equal(t, 10*time.Second, retryDelay(1))
	assert.Equal(t, 20*time.Second, retryDelay(2))
	assert.Equal(t, 5*time.Minute, retryDelay(6))
	assert.Equal(t, 5*time.Minute, retryDelay(1000))
}

func namesOf(requests []ctrl.Request) []types.NamespacedName {
	var names []types.NamespacedName
	for _, request := range requests {
		names = append(names, request.NamespacedName)
	}
	return names
}

func TestEventIsEmittedWhenAReadyStoreFails(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("default", baseTime))
	f.reconcile(t, "default")
	require.Empty(t, f.recorder.Events)

	f.prober.err = &storageprobe.Failure{Step: storageprobe.StepWrite, Err: errors.New("denied")}
	f.reconcile(t, "default")

	require.Len(t, f.recorder.Events, 1)
	assert.Contains(t, <-f.recorder.Events, ReasonStoreUnreachable)
}

func TestDeletedContentRechecksTheStoreItWasHoldingBack(t *testing.T) {
	f := newStoreFixture(t, s3StoreObject("default", baseTime), s3StoreObject("other", baseTime))
	other := f.get(t, "other")
	other.Spec.S3.Prefix = "team-b"
	require.NoError(t, f.c.Update(t.Context(), other))
	f.reconcile(t, "default")
	storeID := f.get(t, "default").Status.StoreID

	assert.Equal(t, []types.NamespacedName{{Name: "named"}},
		namesOf(f.r.storesOfContent(t.Context(), contentFor("c1", "named", storeID))))
	assert.Equal(t, []types.NamespacedName{{Name: "default"}},
		namesOf(f.r.storesOfContent(t.Context(), contentFor("c2", "", storeID))))
	assert.Empty(t, f.r.storesOfContent(t.Context(), &snapshotv1alpha1.PodSnapshotContent{}))
	assert.Empty(t, f.r.storesOfContent(t.Context(), &corev1.Pod{}))
}

func TestStoreWatchIgnoresStatusOnlyUpdates(t *testing.T) {
	store := &snapshotv1alpha1.SnapshotStore{ObjectMeta: metav1.ObjectMeta{Name: "fast", Generation: 1}}
	statusChanged := store.DeepCopy()
	statusChanged.Status.StoreID = "store-v1-changed"
	specChanged := store.DeepCopy()
	specChanged.Generation = 2
	deleting := store.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	assert.False(t, storeSpecOrDeletionChanged.Update(event.UpdateEvent{ObjectOld: store, ObjectNew: statusChanged}))
	assert.True(t, storeSpecOrDeletionChanged.Update(event.UpdateEvent{ObjectOld: store, ObjectNew: specChanged}))
	assert.True(t, storeSpecOrDeletionChanged.Update(event.UpdateEvent{ObjectOld: store, ObjectNew: deleting}))
	assert.True(t, storeSpecOrDeletionChanged.Create(event.CreateEvent{Object: store}))
	assert.True(t, storeSpecOrDeletionChanged.Delete(event.DeleteEvent{Object: store}))
}
