// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ai-dynamo/snapshot/api/storage"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/storageprobe"
)

const (
	// SnapshotStoreProtectionFinalizer keeps a store while content uses it.
	SnapshotStoreProtectionFinalizer = "nvidia.com/snapshotstore-protection"

	ReasonStoreReachable         = "StoreReachable"
	ReasonInvalidLocation        = "InvalidLocation"
	ReasonDuplicateStore         = "DuplicateStore"
	ReasonClaimNotMounted        = "ClaimNotMounted"
	ReasonStoreUnreachable       = "StoreUnreachable"
	ReasonCredentialsUnavailable = "CredentialsUnavailable"
	ReasonTerminating            = "Terminating"

	defaultStoreCheckInterval = 10 * time.Minute
	defaultStoreCheckTimeout  = 2 * time.Minute
	storeRetryBase            = 10 * time.Second
	storeRetryMax             = 5 * time.Minute
	storeTerminatingRecheck   = 30 * time.Second
)

// SnapshotStoreReconciler sets the store ID and the Ready condition of
// each store. A store is Ready when its location is valid, no earlier store
// has the same location, and a test object can be written, read, listed and
// deleted there. A finalizer keeps a store while content uses it.
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotstores,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotstores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotstores/finalizers,verbs=update
// +kubebuilder:rbac:groups=nvidia.com,resources=podsnapshotcontents,verbs=list
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch
type SnapshotStoreReconciler struct {
	client.Client
	// APIReader reads without a cache.
	APIReader client.Reader
	Recorder  record.EventRecorder
	Prober    storageprobe.Prober

	// Namespace is the Snapshot namespace.
	Namespace string
	ClusterID string
	// DefaultStoreName is the only store reconciled while binding is off.
	DefaultStoreName string
	BindingEnabled   bool
	// MountPath returns the mount path of a claim.
	MountPath func(claimName string) (string, bool)

	CheckInterval time.Duration
	CheckTimeout  time.Duration

	mu       sync.Mutex
	failures map[string]int
}

func (r *SnapshotStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	createOrDelete := predicate.Funcs{
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	deleteOnly := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("snapshotstore").
		For(&snapshotv1alpha1.SnapshotStore{}, builder.WithPredicates(storeSpecOrDeletionChanged)).
		Watches(&snapshotv1alpha1.SnapshotStore{},
			handler.EnqueueRequestsFromMapFunc(r.storesAtTheSameLocation), builder.WithPredicates(createOrDelete)).
		Watches(&snapshotv1alpha1.PodSnapshotContent{},
			handler.EnqueueRequestsFromMapFunc(r.storesOfContent), builder.WithPredicates(deleteOnly)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.storesUsingSecret), builder.OnlyMetadata).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.storesUsingConfigMap), builder.OnlyMetadata).
		Complete(r)
}

// storeSpecOrDeletionChanged drops updates that change only status or
// metadata, so the controller's own status write does not queue the store
// ahead of its retry delay.
var storeSpecOrDeletionChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		if e.ObjectOld == nil || e.ObjectNew == nil {
			return false
		}
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
			e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
	},
}

func (r *SnapshotStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	store := &snapshotv1alpha1.SnapshotStore{}
	if err := r.Get(ctx, req.NamespacedName, store); err != nil {
		r.forget(req.Name)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !store.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, store)
	}
	if !r.reconciled(store.Name) {
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(store, SnapshotStoreProtectionFinalizer) {
		if err := r.Update(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
	}

	storeID, err := storeIDOf(store, r.Namespace)
	if err != nil {
		return r.fail(ctx, store, "", ReasonInvalidLocation, err.Error(), false)
	}
	duplicateOf, err := r.earlierStoreAtTheSameLocation(ctx, store, storeID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if duplicateOf != "" {
		return r.fail(ctx, store, storeID, ReasonDuplicateStore,
			fmt.Sprintf("store %q, created earlier, has the same location", duplicateOf), false)
	}

	reason, message := r.checkStore(ctx, store)
	if reason != "" {
		return r.fail(ctx, store, storeID, reason, message, true)
	}
	r.forget(store.Name)
	if err := r.setReady(ctx, store, storeID, metav1.ConditionTrue, ReasonStoreReachable,
		"a test object was written, read, listed and deleted in the store"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.checkInterval()}, nil
}

func (r *SnapshotStoreReconciler) reconciled(name string) bool {
	return r.BindingEnabled || name == r.DefaultStoreName
}

func (r *SnapshotStoreReconciler) fail(ctx context.Context, store *snapshotv1alpha1.SnapshotStore,
	storeID, reason, message string, retry bool,
) (ctrl.Result, error) {
	previousReason := readyReason(store)
	if err := r.setReady(ctx, store, storeID, metav1.ConditionFalse, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	if previousReason != reason {
		r.Recorder.Event(store, corev1.EventTypeWarning, reason, message)
	}
	if !retry {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.nextRetry(store.Name)}, nil
}

func readyReason(store *snapshotv1alpha1.SnapshotStore) string {
	if cond := meta.FindStatusCondition(store.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady); cond != nil {
		return cond.Reason
	}
	return ""
}

func (r *SnapshotStoreReconciler) setReady(ctx context.Context, store *snapshotv1alpha1.SnapshotStore,
	storeID string, status metav1.ConditionStatus, reason, message string,
) error {
	base := store.DeepCopy()
	if storeID != "" {
		store.Status.StoreID = storeID
	}
	meta.SetStatusCondition(&store.Status.Conditions, metav1.Condition{
		Type:               snapshotv1alpha1.PodSnapshotConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: store.Generation,
	})
	if reflect.DeepEqual(base.Status, store.Status) {
		return nil
	}
	return r.Status().Patch(ctx, store, client.MergeFrom(base))
}

// checkStore runs the access check. It returns the reason and message of a
// failure, or an empty reason when the store can be used.
func (r *SnapshotStoreReconciler) checkStore(ctx context.Context,
	store *snapshotv1alpha1.SnapshotStore,
) (string, string) {
	target := storageprobe.Target{ClusterID: r.ClusterID}
	switch {
	case store.Spec.PVC != nil:
		mount, ok := r.MountPath(store.Spec.PVC.ClaimName)
		if !ok {
			return ReasonClaimNotMounted, fmt.Sprintf("claim %q is not mounted by the operator", store.Spec.PVC.ClaimName)
		}
		subPath := store.Spec.PVC.SubPath
		if subPath == "" {
			subPath = storage.PVCRoot
		}
		target.PVC = &storageprobe.PVC{MountPath: mount, BasePath: subPath}
	case store.Spec.S3 != nil:
		s3Spec := store.Spec.S3
		target.S3 = &storageprobe.S3{
			Endpoint:  s3Spec.Endpoint,
			Region:    s3Spec.Region,
			Bucket:    s3Spec.Bucket,
			Prefix:    s3Spec.Prefix,
			PathStyle: s3Spec.Addressing == "path",
			AllowHTTP: s3Spec.AllowHTTP,
		}
		if ref := s3Spec.CredentialsSecretRef; ref != nil {
			data, err := r.readSecretKey(ctx, ref.Name, ref.Key)
			if err != nil {
				return ReasonCredentialsUnavailable, err.Error()
			}
			target.S3.Credentials = data
		}
		if ref := s3Spec.CABundleConfigMapRef; ref != nil {
			data, err := r.readConfigMapKey(ctx, ref.Name, ref.Key)
			if err != nil {
				return ReasonStoreUnreachable, err.Error()
			}
			target.S3.CABundle = data
		}
	default:
		return ReasonInvalidLocation, "exactly one of pvc or s3 must be set"
	}

	ctx, cancel := context.WithTimeout(ctx, r.checkTimeout())
	defer cancel()
	err := r.Prober.Check(ctx, target)
	if err == nil {
		return "", ""
	}
	var failure *storageprobe.Failure
	if errors.As(err, &failure) && failure.Credentials {
		return ReasonCredentialsUnavailable, err.Error()
	}
	return ReasonStoreUnreachable, err.Error()
}

func (r *SnapshotStoreReconciler) readSecretKey(ctx context.Context, name, key string) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("read Secret %q: %w", name, err)
	}
	data := secret.Data[key]
	if len(data) == 0 {
		return nil, fmt.Errorf("Secret %q has no key %q", name, key)
	}
	return data, nil
}

func (r *SnapshotStoreReconciler) readConfigMapKey(ctx context.Context, name, key string) ([]byte, error) {
	configMap := &corev1.ConfigMap{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, configMap); err != nil {
		return nil, fmt.Errorf("read ConfigMap %q: %w", name, err)
	}
	if value := configMap.Data[key]; value != "" {
		return []byte(value), nil
	}
	if value := configMap.BinaryData[key]; len(value) > 0 {
		return value, nil
	}
	return nil, fmt.Errorf("ConfigMap %q has no key %q", name, key)
}

func (r *SnapshotStoreReconciler) earlierStoreAtTheSameLocation(ctx context.Context,
	store *snapshotv1alpha1.SnapshotStore, storeID string,
) (string, error) {
	stores := &snapshotv1alpha1.SnapshotStoreList{}
	if err := r.List(ctx, stores); err != nil {
		return "", err
	}
	for i := range stores.Items {
		other := &stores.Items[i]
		if other.Name == store.Name {
			continue
		}
		otherID, err := storeIDOf(other, r.Namespace)
		if err != nil || otherID != storeID {
			continue
		}
		if createdBefore(other, store) {
			return other.Name, nil
		}
	}
	return "", nil
}

func createdBefore(a, b *snapshotv1alpha1.SnapshotStore) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

func (r *SnapshotStoreReconciler) reconcileDeletion(ctx context.Context,
	store *snapshotv1alpha1.SnapshotStore,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(store, SnapshotStoreProtectionFinalizer) {
		return ctrl.Result{}, nil
	}
	storeID, _ := storeIDOf(store, r.Namespace)
	users, err := r.contentUsing(ctx, store.Name, storeID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if users > 0 {
		message := fmt.Sprintf("%d PodSnapshotContent objects still use this store", users)
		previousReason := readyReason(store)
		if err := r.setReady(ctx, store, "", metav1.ConditionFalse, ReasonTerminating, message); err != nil {
			return ctrl.Result{}, err
		}
		if previousReason != ReasonTerminating {
			r.Recorder.Event(store, corev1.EventTypeWarning, ReasonTerminating, message)
		}
		return ctrl.Result{RequeueAfter: storeTerminatingRecheck}, nil
	}
	base := store.DeepCopy()
	controllerutil.RemoveFinalizer(store, SnapshotStoreProtectionFinalizer)
	if err := r.Patch(ctx, store, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	r.forget(store.Name)
	return ctrl.Result{}, nil
}

// contentUsing counts content that names the store, or that names no store and
// carries its store ID. It reads the spec fields without a cache, not the
// label.
func (r *SnapshotStoreReconciler) contentUsing(ctx context.Context, storeName, storeID string) (int, error) {
	contents := &snapshotv1alpha1.PodSnapshotContentList{}
	if err := r.APIReader.List(ctx, contents); err != nil {
		return 0, err
	}
	users := 0
	for i := range contents.Items {
		binding := contents.Items[i].Spec.Storage
		if binding == nil {
			continue
		}
		if binding.SnapshotStoreName == storeName ||
			(binding.SnapshotStoreName == "" && storeID != "" && binding.StoreID == storeID) {
			users++
		}
	}
	return users, nil
}

func (r *SnapshotStoreReconciler) storesAtTheSameLocation(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*snapshotv1alpha1.SnapshotStore)
	if !ok {
		return nil
	}
	storeID, err := storeIDOf(changed, r.Namespace)
	if err != nil {
		return nil
	}
	stores := &snapshotv1alpha1.SnapshotStoreList{}
	if err := r.List(ctx, stores); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list SnapshotStores")
		return nil
	}
	var requests []reconcile.Request
	for i := range stores.Items {
		other := &stores.Items[i]
		if other.Name == changed.Name {
			continue
		}
		if otherID, err := storeIDOf(other, r.Namespace); err == nil && otherID == storeID {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: other.Name}})
		}
	}
	return requests
}

// storesOfContent returns the stores that a deleted content may have kept
// from being deleted.
func (r *SnapshotStoreReconciler) storesOfContent(ctx context.Context, obj client.Object) []reconcile.Request {
	content, ok := obj.(*snapshotv1alpha1.PodSnapshotContent)
	if !ok || content.Spec.Storage == nil {
		return nil
	}
	binding := content.Spec.Storage
	if binding.SnapshotStoreName != "" {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: binding.SnapshotStoreName}}}
	}
	stores := &snapshotv1alpha1.SnapshotStoreList{}
	if err := r.List(ctx, stores); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list SnapshotStores")
		return nil
	}
	var requests []reconcile.Request
	for i := range stores.Items {
		if storeID, err := storeIDOf(&stores.Items[i], r.Namespace); err == nil && storeID == binding.StoreID {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: stores.Items[i].Name}})
		}
	}
	return requests
}

func (r *SnapshotStoreReconciler) storesUsingSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.storesReferencing(ctx, obj, func(spec *snapshotv1alpha1.S3StoreSpec) string {
		if spec.CredentialsSecretRef == nil {
			return ""
		}
		return spec.CredentialsSecretRef.Name
	})
}

func (r *SnapshotStoreReconciler) storesUsingConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.storesReferencing(ctx, obj, func(spec *snapshotv1alpha1.S3StoreSpec) string {
		if spec.CABundleConfigMapRef == nil {
			return ""
		}
		return spec.CABundleConfigMapRef.Name
	})
}

func (r *SnapshotStoreReconciler) storesReferencing(ctx context.Context, obj client.Object,
	referenced func(*snapshotv1alpha1.S3StoreSpec) string,
) []reconcile.Request {
	if obj.GetNamespace() != r.Namespace {
		return nil
	}
	stores := &snapshotv1alpha1.SnapshotStoreList{}
	if err := r.List(ctx, stores); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list SnapshotStores")
		return nil
	}
	var requests []reconcile.Request
	for i := range stores.Items {
		store := &stores.Items[i]
		if store.Spec.S3 == nil || referenced(store.Spec.S3) != obj.GetName() {
			continue
		}
		r.forget(store.Name)
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: store.Name}})
	}
	return requests
}

func (r *SnapshotStoreReconciler) nextRetry(name string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures == nil {
		r.failures = map[string]int{}
	}
	r.failures[name]++
	return retryDelay(r.failures[name])
}

func (r *SnapshotStoreReconciler) forget(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, name)
}

func retryDelay(failures int) time.Duration {
	delay := storeRetryBase
	for i := 1; i < failures && delay < storeRetryMax; i++ {
		delay *= 2
	}
	return min(delay, storeRetryMax)
}

func (r *SnapshotStoreReconciler) checkInterval() time.Duration {
	if r.CheckInterval > 0 {
		return r.CheckInterval
	}
	return defaultStoreCheckInterval
}

func (r *SnapshotStoreReconciler) checkTimeout() time.Duration {
	if r.CheckTimeout > 0 {
		return r.CheckTimeout
	}
	return defaultStoreCheckTimeout
}

// storeIDOf derives the store ID from the location fields of a store.
func storeIDOf(store *snapshotv1alpha1.SnapshotStore, namespace string) (string, error) {
	switch {
	case store.Spec.PVC != nil && store.Spec.S3 == nil:
		subPath := store.Spec.PVC.SubPath
		if subPath == "" {
			subPath = storage.PVCRoot
		}
		return storage.PVC{Namespace: namespace, ClaimName: store.Spec.PVC.ClaimName, BasePath: subPath}.StoreID()
	case store.Spec.S3 != nil && store.Spec.PVC == nil:
		return storage.S3{
			Endpoint: store.Spec.S3.Endpoint, Region: store.Spec.S3.Region,
			Bucket: store.Spec.S3.Bucket, Prefix: store.Spec.S3.Prefix,
		}.StoreID()
	default:
		return "", fmt.Errorf("exactly one of pvc or s3 must be set")
	}
}
