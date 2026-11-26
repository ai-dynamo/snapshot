// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/maintenance"
)

// A delete that reaches the API server after the content is created and before
// its name is written to status must still remove the content: nothing else
// does, and the node agent checkpoints the source pod for it.
func TestSnapshotReconciler_DeleteBetweenContentCreateAndBindRemovesTheContent(t *testing.T) {
	s := snapshotReconcilerScheme()
	snap := makeSnapshotForReconcile()
	key := types.NamespacedName{Namespace: "inference", Name: snap.Name}
	deleted := false
	funcs := interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if _, ok := obj.(*snapshotv1alpha1.PodSnapshot); ok && !deleted {
			// The user's delete lands first, so the binding is written against an old version.
			deleted = true
			if err := c.Delete(ctx, &snapshotv1alpha1.PodSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}); err != nil {
				return err
			}
		}
		return c.SubResource(subResource).Update(ctx, obj, opts...)
	}}
	r := makeSnapshotReconcilerWithInterceptor(s, funcs, snap, scheduledPod())

	// The content is created, then recording it fails.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.True(t, apierrors.IsConflict(err), "the binding must lose to the delete, got %v", err)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "podsnapshotcontent-snap-uid"}, &snapshotv1alpha1.PodSnapshotContent{}))
	deleting := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), key, deleting))
	require.False(t, deleting.DeletionTimestamp.IsZero())
	require.Nil(t, deleting.Status.BoundPodSnapshotContentName)

	// The content is deleted like a bound one, and the PodSnapshot waits for it.
	res := reconcileSnapshot(t, r, snap.Name)
	content := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "podsnapshotcontent-snap-uid"}, content))
	require.False(t, content.DeletionTimestamp.IsZero(), "the content is not being deleted, so it outlives its PodSnapshot")
	assert.Equal(t, snapshotContentDeleteRequeue, res.RequeueAfter)
	require.NoError(t, r.Get(context.Background(), key, &snapshotv1alpha1.PodSnapshot{}))

	// The maintenance queue removes the artifact and releases the content.
	controllerutil.RemoveFinalizer(content, maintenance.PodSnapshotContentArtifactCleanupFinalizer)
	require.NoError(t, r.Update(context.Background(), content))
	reconcileSnapshot(t, r, snap.Name)

	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Name: "podsnapshotcontent-snap-uid"}, &snapshotv1alpha1.PodSnapshotContent{})))
	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), key, &snapshotv1alpha1.PodSnapshot{})))
}

// A delete that reaches the API server while the finalizer is being added
// removes the PodSnapshot at once: there is no deletion to reconcile. The
// reconcile that read it a moment earlier must take its content back, whether
// the name is then free or already belongs to a new PodSnapshot.
func TestSnapshotReconciler_BindToAPodSnapshotThatIsGoneRemovesTheContent(t *testing.T) {
	for _, recreated := range []bool{false, true} {
		name := "deleted"
		if recreated {
			name = "deleted and created again"
		}
		t.Run(name, func(t *testing.T) {
			s := snapshotReconcilerScheme()
			snap := makeSnapshotForReconcile()
			key := types.NamespacedName{Namespace: "inference", Name: snap.Name}
			removed := false
			funcs := interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*snapshotv1alpha1.PodSnapshot); ok && !removed {
					removed = true
					live := &snapshotv1alpha1.PodSnapshot{}
					if err := c.Get(ctx, key, live); err != nil {
						return err
					}
					live.Finalizers = nil
					if err := c.Update(ctx, live); err != nil {
						return err
					}
					if err := c.Delete(ctx, live); err != nil {
						return err
					}
					if recreated {
						again := makeSnapshotForReconcile()
						again.UID = "snap-uid-2"
						again.Finalizers = nil
						if err := c.Create(ctx, again); err != nil {
							return err
						}
					}
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			}}
			r := makeSnapshotReconcilerWithInterceptor(s, funcs, snap, scheduledPod())

			reconcileSnapshot(t, r, snap.Name)

			require.True(t, removed)
			content := &snapshotv1alpha1.PodSnapshotContent{}
			err := r.Get(context.Background(), types.NamespacedName{Name: "podsnapshotcontent-snap-uid"}, content)
			if err == nil {
				assert.False(t, content.DeletionTimestamp.IsZero(), "the content is not being deleted, so it outlives its PodSnapshot")
			} else {
				assert.True(t, apierrors.IsNotFound(err))
			}
		})
	}
}

// A binding that fails while the PodSnapshot still exists is retried, and the
// content stays for the retry to adopt.
func TestSnapshotReconciler_BindFailureOfALivePodSnapshotKeepsTheContent(t *testing.T) {
	s := snapshotReconcilerScheme()
	snap := makeSnapshotForReconcile()
	failed := false
	funcs := interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if _, ok := obj.(*snapshotv1alpha1.PodSnapshot); ok && !failed {
			failed = true
			return apierrors.NewConflict(schema.GroupResource{Group: "nvidia.com", Resource: "podsnapshots"}, snap.Name, errors.New("the object has been modified"))
		}
		return c.SubResource(subResource).Update(ctx, obj, opts...)
	}}
	r := makeSnapshotReconcilerWithInterceptor(s, funcs, snap, scheduledPod())

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "inference", Name: snap.Name}})
	require.True(t, apierrors.IsConflict(err), "got %v", err)
	content := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "podsnapshotcontent-snap-uid"}, content))
	assert.True(t, content.DeletionTimestamp.IsZero())

	reconcileSnapshot(t, r, snap.Name)
	bound := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "inference", Name: snap.Name}, bound))
	require.NotNil(t, bound.Status.BoundPodSnapshotContentName)
	assert.Equal(t, "podsnapshotcontent-snap-uid", *bound.Status.BoundPodSnapshotContentName)
}

// A content that holds this PodSnapshot's content name with another back
// reference made the PodSnapshot fail with ContentConflict. The operator did
// not create it for this PodSnapshot, so the lookup for an unrecorded content
// must not return it.
func TestSnapshotReconciler_DeleteLeavesTheContentOfAContentConflictAlone(t *testing.T) {
	s := snapshotReconcilerScheme()
	now := metav1.Now()
	snap := makeSnapshotForReconcile()
	snap.DeletionTimestamp = &now
	content := &snapshotv1alpha1.PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "podsnapshotcontent-snap-uid"},
		Spec: snapshotv1alpha1.PodSnapshotContentSpec{
			PodSnapshotRef: snapshotv1alpha1.PodSnapshotReference{Namespace: "inference", Name: snap.Name, UID: "other-snap-uid"},
			Source:         snapshotv1alpha1.PodSnapshotContentSource{PodRef: snapshotv1alpha1.PodReference{Name: "worker-0", UID: "pod-uid-9"}, NodeName: "node-a"},
		},
	}
	r := makeSnapshotReconciler(s, snap, content)

	reconcileSnapshot(t, r, snap.Name)

	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(),
		types.NamespacedName{Namespace: "inference", Name: snap.Name}, &snapshotv1alpha1.PodSnapshot{})))
	kept := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "podsnapshotcontent-snap-uid"}, kept))
	assert.True(t, kept.DeletionTimestamp.IsZero())
}
