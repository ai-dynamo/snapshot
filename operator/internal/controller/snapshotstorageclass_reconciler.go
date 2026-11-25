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
	// StorageClassProtectionFinalizer keeps a class while content still uses it.
	StorageClassProtectionFinalizer = "nvidia.com/snapshotstorageclass-protection"

	ReasonStoreReachable         = "StoreReachable"
	ReasonInvalidLocation        = "InvalidLocation"
	ReasonDuplicateStore         = "DuplicateStore"
	ReasonClaimNotMounted        = "ClaimNotMounted"
	ReasonStoreUnreachable       = "StoreUnreachable"
	ReasonCredentialsUnavailable = "CredentialsUnavailable"
	ReasonTerminating            = "Terminating"

	defaultClassCheckInterval = 10 * time.Minute
	defaultClassCheckTimeout  = 2 * time.Minute
	classRetryBase            = 10 * time.Second
	classRetryMax             = 5 * time.Minute
	classTerminatingRecheck   = 30 * time.Second
)

// SnapshotStorageClassReconciler tells whether each class names a store that
// can be used: its location is valid and unique in the cluster, and a test
// object can be written, read, listed and deleted there. It also keeps a class
// from being deleted while content still uses it.
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotstorageclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotstorageclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotstorageclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=nvidia.com,resources=podsnapshotcontents,verbs=list
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch
type SnapshotStorageClassReconciler struct {
	client.Client
	// APIReader reads without a cache: credentials, CA bundles and content.
	APIReader client.Reader
	Recorder  record.EventRecorder
	Prober    storageprobe.Prober

	// Namespace is the Snapshot namespace, where classes name their claims,
	// Secrets and ConfigMaps.
	Namespace string
	ClusterID string
	// DefaultClassName is the one class reconciled while binding is off.
	DefaultClassName string
	BindingEnabled   bool
	// MountPath returns where the operator has mounted a claim.
	MountPath func(claimName string) (string, bool)

	CheckInterval time.Duration
	CheckTimeout  time.Duration

	mu       sync.Mutex
	failures map[string]int
}

func (r *SnapshotStorageClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
		Named("snapshotstorageclass").
		For(&snapshotv1alpha1.SnapshotStorageClass{}).
		Watches(&snapshotv1alpha1.SnapshotStorageClass{},
			handler.EnqueueRequestsFromMapFunc(r.classesWithTheSameStore), builder.WithPredicates(createOrDelete)).
		Watches(&snapshotv1alpha1.PodSnapshotContent{},
			handler.EnqueueRequestsFromMapFunc(r.classesOfContent), builder.WithPredicates(deleteOnly)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.classesUsingSecret), builder.OnlyMetadata).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.classesUsingConfigMap), builder.OnlyMetadata).
		Complete(r)
}

func (r *SnapshotStorageClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	class := &snapshotv1alpha1.SnapshotStorageClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		r.forget(req.Name)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !class.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, class)
	}
	if !r.reconciled(class.Name) {
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(class, StorageClassProtectionFinalizer) {
		if err := r.Update(ctx, class); err != nil {
			return ctrl.Result{}, err
		}
	}

	storeID, err := classStoreID(class, r.Namespace)
	if err != nil {
		return r.fail(ctx, class, "", ReasonInvalidLocation, err.Error(), false)
	}
	duplicateOf, err := r.earlierClassWithTheSameStore(ctx, class, storeID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if duplicateOf != "" {
		return r.fail(ctx, class, storeID, ReasonDuplicateStore,
			fmt.Sprintf("class %q, created earlier, names the same store", duplicateOf), false)
	}

	reason, message := r.checkStore(ctx, class)
	if reason != "" {
		return r.fail(ctx, class, storeID, reason, message, true)
	}
	r.forget(class.Name)
	if err := r.setReady(ctx, class, storeID, metav1.ConditionTrue, ReasonStoreReachable,
		"a test object was written, read, listed and deleted in the store"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.checkInterval()}, nil
}

func (r *SnapshotStorageClassReconciler) reconciled(name string) bool {
	return r.BindingEnabled || name == r.DefaultClassName
}

func (r *SnapshotStorageClassReconciler) fail(ctx context.Context, class *snapshotv1alpha1.SnapshotStorageClass,
	storeID, reason, message string, retry bool,
) (ctrl.Result, error) {
	previousReason := readyReason(class)
	if err := r.setReady(ctx, class, storeID, metav1.ConditionFalse, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	if previousReason != reason {
		r.Recorder.Event(class, corev1.EventTypeWarning, reason, message)
	}
	if !retry {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.nextRetry(class.Name)}, nil
}

func readyReason(class *snapshotv1alpha1.SnapshotStorageClass) string {
	if cond := meta.FindStatusCondition(class.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady); cond != nil {
		return cond.Reason
	}
	return ""
}

func (r *SnapshotStorageClassReconciler) setReady(ctx context.Context, class *snapshotv1alpha1.SnapshotStorageClass,
	storeID string, status metav1.ConditionStatus, reason, message string,
) error {
	base := class.DeepCopy()
	if storeID != "" {
		class.Status.StoreID = storeID
	}
	meta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
		Type:               snapshotv1alpha1.PodSnapshotConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: class.Generation,
	})
	if reflect.DeepEqual(base.Status, class.Status) {
		return nil
	}
	return r.Status().Patch(ctx, class, client.MergeFrom(base))
}

// checkStore runs the access check and returns the reason and message of a
// failure, or an empty reason when the store can be used.
func (r *SnapshotStorageClassReconciler) checkStore(ctx context.Context,
	class *snapshotv1alpha1.SnapshotStorageClass,
) (string, string) {
	target := storageprobe.Target{ClusterID: r.ClusterID}
	switch {
	case class.Spec.PVC != nil:
		mount, ok := r.MountPath(class.Spec.PVC.ClaimName)
		if !ok {
			return ReasonClaimNotMounted, fmt.Sprintf("claim %q is not mounted by the operator", class.Spec.PVC.ClaimName)
		}
		basePath := class.Spec.PVC.BasePath
		if basePath == "" {
			basePath = storage.PVCRoot
		}
		target.PVC = &storageprobe.PVC{MountPath: mount, BasePath: basePath}
	case class.Spec.S3 != nil:
		s3Spec := class.Spec.S3
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

func (r *SnapshotStorageClassReconciler) readSecretKey(ctx context.Context, name, key string) ([]byte, error) {
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

func (r *SnapshotStorageClassReconciler) readConfigMapKey(ctx context.Context, name, key string) ([]byte, error) {
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

func (r *SnapshotStorageClassReconciler) earlierClassWithTheSameStore(ctx context.Context,
	class *snapshotv1alpha1.SnapshotStorageClass, storeID string,
) (string, error) {
	classes := &snapshotv1alpha1.SnapshotStorageClassList{}
	if err := r.List(ctx, classes); err != nil {
		return "", err
	}
	for i := range classes.Items {
		other := &classes.Items[i]
		if other.Name == class.Name {
			continue
		}
		otherID, err := classStoreID(other, r.Namespace)
		if err != nil || otherID != storeID {
			continue
		}
		if createdBefore(other, class) {
			return other.Name, nil
		}
	}
	return "", nil
}

func createdBefore(a, b *snapshotv1alpha1.SnapshotStorageClass) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

func (r *SnapshotStorageClassReconciler) reconcileDeletion(ctx context.Context,
	class *snapshotv1alpha1.SnapshotStorageClass,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(class, StorageClassProtectionFinalizer) {
		return ctrl.Result{}, nil
	}
	storeID, _ := classStoreID(class, r.Namespace)
	users, err := r.contentUsing(ctx, class.Name, storeID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if users > 0 {
		message := fmt.Sprintf("%d PodSnapshotContent objects still use this class", users)
		previousReason := readyReason(class)
		if err := r.setReady(ctx, class, "", metav1.ConditionFalse, ReasonTerminating, message); err != nil {
			return ctrl.Result{}, err
		}
		if previousReason != ReasonTerminating {
			r.Recorder.Event(class, corev1.EventTypeWarning, ReasonTerminating, message)
		}
		return ctrl.Result{RequeueAfter: classTerminatingRecheck}, nil
	}
	base := class.DeepCopy()
	controllerutil.RemoveFinalizer(class, StorageClassProtectionFinalizer)
	if err := r.Patch(ctx, class, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	r.forget(class.Name)
	return ctrl.Result{}, nil
}

// contentUsing counts content that names the class, or that has no class name
// and carries its store ID. It reads the spec fields without a cache; the label
// on the content can be edited and only serves queries.
func (r *SnapshotStorageClassReconciler) contentUsing(ctx context.Context, className, storeID string) (int, error) {
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
		if binding.StorageClassName == className ||
			(binding.StorageClassName == "" && storeID != "" && binding.StoreID == storeID) {
			users++
		}
	}
	return users, nil
}

func (r *SnapshotStorageClassReconciler) classesWithTheSameStore(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*snapshotv1alpha1.SnapshotStorageClass)
	if !ok {
		return nil
	}
	storeID, err := classStoreID(changed, r.Namespace)
	if err != nil {
		return nil
	}
	classes := &snapshotv1alpha1.SnapshotStorageClassList{}
	if err := r.List(ctx, classes); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list SnapshotStorageClasses")
		return nil
	}
	var requests []reconcile.Request
	for i := range classes.Items {
		other := &classes.Items[i]
		if other.Name == changed.Name {
			continue
		}
		if otherID, err := classStoreID(other, r.Namespace); err == nil && otherID == storeID {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: other.Name}})
		}
	}
	return requests
}

// classesOfContent names the classes a deleted content may have been holding
// back from deletion.
func (r *SnapshotStorageClassReconciler) classesOfContent(ctx context.Context, obj client.Object) []reconcile.Request {
	content, ok := obj.(*snapshotv1alpha1.PodSnapshotContent)
	if !ok || content.Spec.Storage == nil {
		return nil
	}
	binding := content.Spec.Storage
	if binding.StorageClassName != "" {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: binding.StorageClassName}}}
	}
	classes := &snapshotv1alpha1.SnapshotStorageClassList{}
	if err := r.List(ctx, classes); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list SnapshotStorageClasses")
		return nil
	}
	var requests []reconcile.Request
	for i := range classes.Items {
		if storeID, err := classStoreID(&classes.Items[i], r.Namespace); err == nil && storeID == binding.StoreID {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: classes.Items[i].Name}})
		}
	}
	return requests
}

func (r *SnapshotStorageClassReconciler) classesUsingSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.classesReferencing(ctx, obj, func(spec *snapshotv1alpha1.S3StoreSpec) string {
		if spec.CredentialsSecretRef == nil {
			return ""
		}
		return spec.CredentialsSecretRef.Name
	})
}

func (r *SnapshotStorageClassReconciler) classesUsingConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.classesReferencing(ctx, obj, func(spec *snapshotv1alpha1.S3StoreSpec) string {
		if spec.CABundleConfigMapRef == nil {
			return ""
		}
		return spec.CABundleConfigMapRef.Name
	})
}

func (r *SnapshotStorageClassReconciler) classesReferencing(ctx context.Context, obj client.Object,
	referenced func(*snapshotv1alpha1.S3StoreSpec) string,
) []reconcile.Request {
	if obj.GetNamespace() != r.Namespace {
		return nil
	}
	classes := &snapshotv1alpha1.SnapshotStorageClassList{}
	if err := r.List(ctx, classes); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list SnapshotStorageClasses")
		return nil
	}
	var requests []reconcile.Request
	for i := range classes.Items {
		class := &classes.Items[i]
		if class.Spec.S3 == nil || referenced(class.Spec.S3) != obj.GetName() {
			continue
		}
		r.forget(class.Name)
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: class.Name}})
	}
	return requests
}

func (r *SnapshotStorageClassReconciler) nextRetry(name string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures == nil {
		r.failures = map[string]int{}
	}
	r.failures[name]++
	return retryDelay(r.failures[name])
}

func (r *SnapshotStorageClassReconciler) forget(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, name)
}

func retryDelay(failures int) time.Duration {
	delay := classRetryBase
	for i := 1; i < failures && delay < classRetryMax; i++ {
		delay *= 2
	}
	return min(delay, classRetryMax)
}

func (r *SnapshotStorageClassReconciler) checkInterval() time.Duration {
	if r.CheckInterval > 0 {
		return r.CheckInterval
	}
	return defaultClassCheckInterval
}

func (r *SnapshotStorageClassReconciler) checkTimeout() time.Duration {
	if r.CheckTimeout > 0 {
		return r.CheckTimeout
	}
	return defaultClassCheckTimeout
}

// classStoreID derives the store identity from a class's location fields.
func classStoreID(class *snapshotv1alpha1.SnapshotStorageClass, namespace string) (string, error) {
	switch {
	case class.Spec.PVC != nil && class.Spec.S3 == nil:
		basePath := class.Spec.PVC.BasePath
		if basePath == "" {
			basePath = storage.PVCRoot
		}
		return storage.PVC{Namespace: namespace, ClaimName: class.Spec.PVC.ClaimName, BasePath: basePath}.StoreID()
	case class.Spec.S3 != nil && class.Spec.PVC == nil:
		return storage.S3{
			Endpoint: class.Spec.S3.Endpoint, Region: class.Spec.S3.Region,
			Bucket: class.Spec.S3.Bucket, Prefix: class.Spec.S3.Prefix,
		}.StoreID()
	default:
		return "", fmt.Errorf("exactly one of pvc or s3 must be set")
	}
}
