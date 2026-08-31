// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controller implements the node-local control loop inside snapshot-agent.
// It does not own CRDs or replace the operator. Instead it watches pod, job, and
// lease state on the current node and delegates CRIU/CUDA execution to the
// snapshot executor workflows.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// NodeController watches local-node pods with checkpoint metadata and reconciles
// snapshot execution for checkpoint and restore requests. The restore path is
// driven by a client-go pod informer; the capture path is driven by a dynamic
// informer over PodSnapshotContent work orders filtered to this node, with typed
// reads/writes via an uncached controller-runtime client.
type NodeController struct {
	config                  *types.AgentConfig
	clientset               kubernetes.Interface
	client                  client.Client
	dynClient               dynamic.Interface
	runtime                 snapshotruntime.Runtime
	injector                executor.RestoreMounter
	log                     logr.Logger
	holderID                string
	checkpointFn            func(ctx context.Context, params CheckpointParams) error
	restoreFn               func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error)
	writeControlSentinelFn  func(int, string) error
	controlSentinelExistsFn func(int, string) (bool, error)
	sendSignalFn            func(logr.Logger, int, syscall.Signal, string) error
	restoreQueue            workqueue.TypedDelayingInterface[client.ObjectKey]
	restorePodLister        corev1listers.PodLister

	inFlight   map[string]struct{}
	inFlightMu sync.Mutex

	handledRestores sync.Map

	// contentIndexer is the PodSnapshotContent informer's indexer, indexed by source pod
	// (podRefIndex). The source-pod informer uses it to map a pod event back to its work order.
	contentIndexer cache.Indexer

	stopCh chan struct{}
}

type restoreArtifact struct {
	SnapshotName        string
	ContentUID          string
	SourceContainerName string
	Path                string
}

type restoreTarget struct {
	SnapshotName        string
	ContentUID          string
	SourceContainerName string
}

type restorePlan struct {
	artifact *restoreArtifact
	mappings []snapshotv1alpha1.RestoreContainerMapping
}

type restoreResultState int

const (
	restoreResultPending restoreResultState = iota
	restoreResultSucceeded
	restoreResultFailed
)

type restoreResult struct {
	destination string
	state       restoreResultState
}

type restorePendingError struct {
	reason  string
	message string
}

func (e *restorePendingError) Error() string {
	return e.message
}

type restoreOperation struct {
	controller  *NodeController
	pod         *corev1.Pod
	artifact    *restoreArtifact
	destination string
	containerID string
	startedAt   time.Time
	log         logr.Logger
}

const (
	containerResolveAttemptTimeout     = 1 * time.Second
	restoreContainerResolveInterval    = 50 * time.Millisecond
	restoreContainerResolveTimeout     = 30 * time.Second
	restoreAlreadyCompletedReason      = "RestoreAlreadyCompleted"
	restoreAlreadyPartialReason        = "RestoreAlreadyPartiallySucceeded"
	restoreAlreadyFailedReason         = "RestoreAlreadyFailed"
	restoreFinalizerUpdateFailedReason = "RestoreFinalizerUpdateFailed"
	restoreStatusUpdateFailedReason    = "RestoreStatusUpdateFailed"
	restoreRequestedReason             = "RestoreRequested"
	restoreStatusFieldManager          = "snapshot-agent-restore"
	restorePodFinalizer                = "snapshot/restore-protection"
	snapshotEventComponent             = "snapshot"
	restoreSafetyRequeueInterval       = 30 * time.Second

	// snapshotContentResyncInterval re-drives every PodSnapshotContent work order so a
	// not-yet-Ready source pod is re-checked for quiesce without a busy loop.
	snapshotContentResyncInterval = 10 * time.Second
)

// podSnapshotContentGVR is the cluster-scoped resource the capture informer watches.
var podSnapshotContentGVR = snapshotv1alpha1.GroupVersion.WithResource("podsnapshotcontents")

// NewNodeController creates the node-local controller that runs inside snapshot-agent.
func NewNodeController(
	cfg *types.AgentConfig,
	rt snapshotruntime.Runtime,
	log logr.Logger,
) (*NodeController, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(snapshotv1alpha1.AddToScheme(scheme))

	typedClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create typed client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	nsm := nsmount.New(log)
	return newDefaultController(cfg, clientset, typedClient, dynClient, rt, nsm, log), nil
}

func newDefaultController(
	cfg *types.AgentConfig,
	clientset kubernetes.Interface,
	typedClient client.Client,
	dynClient dynamic.Interface,
	rt snapshotruntime.Runtime,
	injector executor.RestoreMounter,
	log logr.Logger,
) *NodeController {
	w := &NodeController{
		config:    cfg,
		clientset: clientset,
		client:    typedClient,
		dynClient: dynClient,
		runtime:   rt,
		injector:  injector,
		log:       log,
		holderID:  "snapshot-agent/" + uuid.NewString(),
		inFlight:  make(map[string]struct{}),
		stopCh:    make(chan struct{}),
		restoreQueue: workqueue.NewTypedDelayingQueueWithConfig(
			workqueue.TypedDelayingQueueConfig[client.ObjectKey]{Name: "restore-pods"},
		),

		restoreFn:               executor.Restore,
		writeControlSentinelFn:  snapshotruntime.WriteControlSentinel,
		controlSentinelExistsFn: snapshotruntime.ControlSentinelExists,
		sendSignalFn:            snapshotruntime.SendSignalToPID,
	}
	w.checkpointFn = w.executorCheckpoint
	return w
}

// Run starts the local pod informers and processes checkpoint/restore events.
func (w *NodeController) Run(ctx context.Context) error {
	defer w.restoreQueue.ShutDown()
	// Seed the agent logger onto ctx so the capture path resolves it via log.FromContext.
	ctx = logr.NewContext(ctx, w.log)
	w.log.Info("Starting snapshot node controller",
		"node", w.config.NodeName,
		"restore_from_annotation", snapshotv1alpha1.RestoreFromAnnotation,
	)

	w.log.Info("Watching pods cluster-wide (all namespaces)")

	var syncFuncs []cache.InformerSynced

	restoreFactoryOpts := []informers.SharedInformerOption{
		informers.WithTweakListOptions(tweakNodePodListOptions(w.config.NodeName)),
	}

	restoreFactory := informers.NewSharedInformerFactoryWithOptions(
		w.clientset, 30*time.Second, restoreFactoryOpts...,
	)

	restorePods := restoreFactory.Core().V1().Pods()
	restoreInformer := restorePods.Informer()
	w.restorePodLister = restorePods.Lister()
	if _, err := restoreInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: w.enqueueRestorePod,
		UpdateFunc: func(_, newObj interface{}) {
			w.enqueueRestorePod(newObj)
		},
		DeleteFunc: w.forgetRestorePod,
	}); err != nil {
		return fmt.Errorf("failed to add restore informer handler: %w", err)
	}
	go restoreFactory.Start(w.stopCh)
	syncFuncs = append(syncFuncs, restoreInformer.HasSynced)

	// Capture path: a dynamic informer over PodSnapshotContent work orders, filtered at
	// the list/watch level to this node's mirror label. The node-label filter is the
	// node scoping; reconcilePodSnapshotContent keeps a defensive nodeName check.
	nodeContentSelector := labels.SelectorFromSet(labels.Set{snapshotv1alpha1.SnapshotNodeLabel: w.config.NodeName}).String()
	dynFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		w.dynClient, snapshotContentResyncInterval, metav1.NamespaceAll,
		func(opts *metav1.ListOptions) {
			opts.LabelSelector = nodeContentSelector
		},
	)
	contentInformer := dynFactory.ForResource(podSnapshotContentGVR).Informer()
	// Index work orders by their source pod so a source-pod event maps back to its
	// PodSnapshotContent in O(1). Must be registered before the informer starts.
	if err := contentInformer.AddIndexers(cache.Indexers{podRefIndex: podRefIndexFunc}); err != nil {
		return fmt.Errorf("failed to add snapshot-content podRef indexer: %w", err)
	}
	w.contentIndexer = contentInformer.GetIndexer()
	if _, err := contentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if name, ok := contentNameFromInformerObj(obj); ok {
				w.reconcilePodSnapshotContent(ctx, name)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if name, ok := contentNameFromInformerObj(newObj); ok {
				w.reconcilePodSnapshotContent(ctx, name)
			}
		},
	}); err != nil {
		return fmt.Errorf("failed to add snapshot-content informer handler: %w", err)
	}
	go dynFactory.Start(w.stopCh)
	syncFuncs = append(syncFuncs, contentInformer.HasSynced)

	// Source-pod informer: keyed on CaptureEligibleLabel, the promotion label the pre-bind gate
	// (reconcilePodSnapshotContent) adds only after a source pod passes validation, so only
	// gate-validated pods drive the capture path.
	// A pod status change (a checkpoint container crashing, or the target becoming ready) does
	// not touch the PodSnapshotContent, so without this trigger it would only be acted on at the
	// content informer's resync. It needs its own factory: its selector is disjoint from the restore
	// informer's.
	sourceSelector := labels.SelectorFromSet(labels.Set{snapshotv1alpha1.CaptureEligibleLabel: "true"}).String()
	sourceFactoryOpts := []informers.SharedInformerOption{
		informers.WithTweakListOptions(tweakLabeledNodePodListOptions(sourceSelector, w.config.NodeName)),
	}
	sourceFactory := informers.NewSharedInformerFactoryWithOptions(
		w.clientset, 30*time.Second, sourceFactoryOpts...,
	)
	sourceInformer := sourceFactory.Core().V1().Pods().Informer()
	if _, err := sourceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if pod, ok := podFromInformerObj(obj); ok {
				if err := w.reconcileSourcePod(ctx, pod); err != nil {
					w.log.Error(err, "Failed to reconcile source pod", "pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if pod, ok := podFromInformerObj(newObj); ok {
				if err := w.reconcileSourcePod(ctx, pod); err != nil {
					w.log.Error(err, "Failed to reconcile source pod", "pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
				}
			}
		},
	}); err != nil {
		return fmt.Errorf("failed to add source-pod informer handler: %w", err)
	}
	go sourceFactory.Start(w.stopCh)
	syncFuncs = append(syncFuncs, sourceInformer.HasSynced)

	// Close stopCh on cancellation so a stalled cache sync (below) is unblocked by ctx, not only on
	// the happy path.
	var stopOnce sync.Once
	go func() {
		<-ctx.Done()
		w.restoreQueue.ShutDown()
		stopOnce.Do(func() { close(w.stopCh) })
	}()

	if !cache.WaitForCacheSync(w.stopCh, syncFuncs...) {
		return fmt.Errorf("failed to sync informer caches")
	}

	go w.runRestoreQueue(ctx)
	w.log.Info("PodSnapshot node controller started and caches synced")
	<-ctx.Done()
	stopOnce.Do(func() { close(w.stopCh) })
	return nil
}

func tweakNodePodListOptions(nodeName string) func(*metav1.ListOptions) {
	return func(opts *metav1.ListOptions) {
		opts.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
	}
}

func tweakLabeledNodePodListOptions(labelSelector, nodeName string) func(*metav1.ListOptions) {
	return func(opts *metav1.ListOptions) {
		tweakNodePodListOptions(nodeName)(opts)
		opts.LabelSelector = labelSelector
	}
}

func (w *NodeController) enqueueRestorePod(obj interface{}) {
	pod, ok := podFromInformerObj(obj)
	if !ok || !w.restorePodRelevant(pod) {
		return
	}
	w.restoreQueue.Add(client.ObjectKeyFromObject(pod))
}

func (w *NodeController) forgetRestorePod(obj interface{}) {
	pod, ok := podFromInformerObj(obj)
	if !ok {
		return
	}
	w.handledRestores.Delete(string(pod.UID))
}

func (w *NodeController) restorePodRequested(pod *corev1.Pod) bool {
	if pod.Spec.NodeName != w.config.NodeName {
		return false
	}
	_, requested := pod.Annotations[snapshotv1alpha1.RestoreFromAnnotation]
	return requested
}

func (w *NodeController) restorePodRelevant(pod *corev1.Pod) bool {
	return pod.Spec.NodeName == w.config.NodeName &&
		(hasFinalizer(pod, restorePodFinalizer) || w.restorePodRequested(pod))
}

func (w *NodeController) runRestoreQueue(ctx context.Context) {
	for {
		key, shutdown := w.restoreQueue.Get()
		if shutdown {
			return
		}
		go w.processRestoreQueueItem(ctx, key)
	}
}

func (w *NodeController) processRestoreQueueItem(ctx context.Context, key client.ObjectKey) {
	requeue := false
	defer func() {
		w.restoreQueue.Done(key)
		if requeue && ctx.Err() == nil {
			w.restoreQueue.AddAfter(key, restoreSafetyRequeueInterval)
		}
	}()

	pod, err := w.restorePodLister.Pods(key.Namespace).Get(key.Name)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		w.log.Error(err, "Failed to read restore pod from informer cache", "pod", key.String())
		requeue = true
		return
	}
	pod = pod.DeepCopy()
	if w.restoreHandled(pod) {
		requeue = w.removeRestoreFinalizerWithEvent(ctx, pod)
		return
	}
	if isRestoreTerminal(pod) {
		requeue = w.handleTerminalRestorePod(ctx, pod)
		return
	}
	if pod.DeletionTimestamp != nil {
		requeue = w.removeRestoreFinalizerWithEvent(ctx, pod)
		return
	}
	if !isRestorePodActive(pod) {
		requeue = w.failRestorePod(ctx, pod, fmt.Errorf("Pod %s/%s is not eligible for restore in phase %s; expected Pending or Running", pod.Namespace, pod.Name, pod.Status.Phase))
		return
	}
	requeue = w.reconcileRestorePod(ctx, pod)
}

// handleTerminalRestorePod reports the previously recorded terminal outcome
// and removes restore protection without replaying any worker.
func (w *NodeController) handleTerminalRestorePod(ctx context.Context, pod *corev1.Pod) bool {
	condition := findRestoredCondition(pod)
	eventType := corev1.EventTypeWarning
	reason := restoreAlreadyFailedReason
	message := condition.Message

	switch {
	case isRestoreSucceeded(pod):
		eventType = corev1.EventTypeNormal
		reason = restoreAlreadyCompletedReason
		message = "Pod restore already completed; no further action is required"
	case isRestorePartiallySucceeded(pod):
		reason = restoreAlreadyPartialReason
		if message == "" {
			message = "Pod restore partially succeeded; create a new restore Pod to retry failed destinations"
		}
	case message == "":
		message = "Pod restore previously failed; create a new restore Pod to retry"
	}

	emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, eventType, reason, message)
	return w.removeRestoreFinalizerWithEvent(ctx, pod)
}

func (w *NodeController) reconcileRestorePod(ctx context.Context, pod *corev1.Pod) bool {
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	plan, err := w.preflightRestore(ctx, pod)
	if err != nil {
		return w.handleRestorePreflightError(ctx, pod, err)
	}
	if err := w.addRestoreFinalizer(ctx, pod); err != nil {
		w.log.Error(err, "Failed to protect restore Pod", "pod", podKey)
		emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreFinalizerUpdateFailedReason, err.Error())
		return true
	}
	return w.restorePodContainers(ctx, pod, plan, podKey)
}

// preflightRestore validates the restore Pod, its referenced PodSnapshot and
// bound PodSnapshotContent, and the node-local artifact. Restore execution is
// entered only when this method returns a complete artifact.
func (w *NodeController) preflightRestore(ctx context.Context, pod *corev1.Pod) (*restorePlan, error) {
	snapshot, err := w.getPodSnapshotFromPod(ctx, pod)
	if err != nil {
		return nil, err
	}
	if err := validatePodSnapshotForRestore(snapshot); err != nil {
		return nil, err
	}
	content, err := w.getPodSnapshotContentFromSnapshot(ctx, pod, snapshot)
	if err != nil {
		return nil, err
	}
	if err := validatePodSnapshotContentForRestore(content); err != nil {
		return nil, err
	}
	target, mappings, err := validateRestoreTarget(pod, snapshot, content)
	if err != nil {
		return nil, err
	}
	artifact, err := w.resolveRestoreArtifact(fmt.Sprintf("%s/%s", pod.Namespace, pod.Name), target)
	if err != nil {
		return nil, err
	}
	if w.config.CRIU.TcpEstablished && pod.Status.PodIP == "" {
		return nil, newRestorePendingError("PodIPPending", fmt.Sprintf("Waiting for restore Pod %s/%s to receive an IP address", pod.Namespace, pod.Name))
	}
	return &restorePlan{artifact: artifact, mappings: mappings}, nil
}

func (w *NodeController) getPodSnapshotFromPod(ctx context.Context, pod *corev1.Pod) (*snapshotv1alpha1.PodSnapshot, error) {
	snapshotName, err := snapshotv1alpha1.GetRestoreFromSnapshotName(pod.Annotations)
	if err != nil {
		return nil, err
	}
	snapshot := &snapshotv1alpha1.PodSnapshot{}
	key := client.ObjectKey{Namespace: pod.Namespace, Name: snapshotName}
	if err := w.client.Get(ctx, key, snapshot); err != nil {
		if apierrors.IsNotFound(err) {
			if restoreInProgress(pod) {
				return nil, fmt.Errorf("PodSnapshot %s disappeared while restore was in progress", key.String())
			}
			return nil, newRestorePendingError("SnapshotPending", fmt.Sprintf("Waiting for PodSnapshot %s", key.String()))
		}
		w.log.Error(err, "Failed to read PodSnapshot during restore preflight", "pod", client.ObjectKeyFromObject(pod).String(), "snapshot", key.String())
		return nil, newRestorePendingError("SnapshotPending", fmt.Sprintf("Unable to read PodSnapshot %s; retrying", key.String()))
	}
	return snapshot, nil
}

func validatePodSnapshotForRestore(snapshot *snapshotv1alpha1.PodSnapshot) error {
	key := client.ObjectKeyFromObject(snapshot)
	if snapshotv1alpha1.IsPodSnapshotFailed(snapshot) {
		cond := meta.FindStatusCondition(snapshot.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
		message := fmt.Sprintf("PodSnapshot %s failed", key.String())
		if cond != nil && cond.Message != "" {
			message += ": " + cond.Message
		}
		return errors.New(message)
	}
	if !snapshotv1alpha1.IsPodSnapshotSucceeded(snapshot) || snapshot.Status.BoundPodSnapshotContentName == nil ||
		strings.TrimSpace(*snapshot.Status.BoundPodSnapshotContentName) == "" {
		return newRestorePendingError("SnapshotPending", fmt.Sprintf("Waiting for PodSnapshot %s to become Ready", key.String()))
	}
	return nil
}

func (w *NodeController) getPodSnapshotContentFromSnapshot(ctx context.Context, pod *corev1.Pod, snapshot *snapshotv1alpha1.PodSnapshot) (*snapshotv1alpha1.PodSnapshotContent, error) {
	contentName := strings.TrimSpace(ptr.Deref(snapshot.Status.BoundPodSnapshotContentName, ""))
	content := &snapshotv1alpha1.PodSnapshotContent{}
	if err := w.client.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			if restoreInProgress(pod) {
				return nil, fmt.Errorf("bound PodSnapshotContent %s disappeared while restore was in progress", contentName)
			}
			return nil, newRestorePendingError("ContentPending", fmt.Sprintf("Waiting for bound PodSnapshotContent %s", contentName))
		}
		w.log.Error(err, "Failed to read PodSnapshotContent during restore preflight", "snapshot", client.ObjectKeyFromObject(snapshot).String(), "content", contentName)
		return nil, newRestorePendingError("ContentPending", fmt.Sprintf("Unable to read bound PodSnapshotContent %s; retrying", contentName))
	}
	ref := content.Spec.PodSnapshotRef
	if ref.Namespace != snapshot.Namespace || ref.Name != snapshot.Name || ref.UID == "" || ref.UID != snapshot.UID {
		return nil, fmt.Errorf("PodSnapshotContent %s has a stale backlink to %s/%s uid %q", content.Name, ref.Namespace, ref.Name, ref.UID)
	}
	return content, nil
}

func validatePodSnapshotContentForRestore(content *snapshotv1alpha1.PodSnapshotContent) error {
	if snapshotv1alpha1.IsPodSnapshotContentFailed(content) {
		cond := meta.FindStatusCondition(content.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
		message := fmt.Sprintf("PodSnapshotContent %s failed", content.Name)
		if cond != nil && cond.Message != "" {
			message += ": " + cond.Message
		}
		return errors.New(message)
	}
	if !snapshotv1alpha1.IsPodSnapshotContentSucceeded(content) {
		return newRestorePendingError("ContentPending", fmt.Sprintf("Waiting for PodSnapshotContent %s to become Ready", content.Name))
	}
	return nil
}

// validateRestoreTarget resolves the captured source and validates every
// requested destination against the restore Pod.
func validateRestoreTarget(pod *corev1.Pod, snapshot *snapshotv1alpha1.PodSnapshot, content *snapshotv1alpha1.PodSnapshotContent) (*restoreTarget, []snapshotv1alpha1.RestoreContainerMapping, error) {
	containerName, err := singleTargetContainer(content)
	if err != nil {
		return nil, nil, err
	}
	mappings, err := snapshotv1alpha1.RestoreContainerMappingsFromAnnotations(pod.Annotations, containerName)
	if err != nil {
		return nil, nil, err
	}
	if err := validateRestoreMappingsForPod(&pod.Spec, mappings, containerName); err != nil {
		return nil, nil, err
	}
	return &restoreTarget{SnapshotName: snapshot.Name, ContentUID: string(content.UID), SourceContainerName: containerName}, mappings, nil
}

// validateRestoreMappingsForPod validates the mapping contract and ensures
// every destination exists in the restore Pod spec.
func validateRestoreMappingsForPod(spec *corev1.PodSpec, mappings []snapshotv1alpha1.RestoreContainerMapping, capturedSource string) error {
	if err := snapshotv1alpha1.ValidateRestoreContainerMappings(mappings, capturedSource); err != nil {
		return err
	}
	if len(mappings) > 1 {
		hasControlVolume := false
		for _, volume := range spec.Volumes {
			if volume.Name == snapshotv1alpha1.SnapshotControlVolumeName && volume.EmptyDir != nil {
				hasControlVolume = true
				break
			}
		}
		if !hasControlVolume {
			return fmt.Errorf("multi-container restore requires %s emptyDir volume", snapshotv1alpha1.SnapshotControlVolumeName)
		}
	}
	for _, mapping := range mappings {
		var destination *corev1.Container
		for i := range spec.Containers {
			if spec.Containers[i].Name == mapping.Destination {
				destination = &spec.Containers[i]
				break
			}
		}
		if destination == nil {
			return fmt.Errorf("restore pod has no destination container named %q", mapping.Destination)
		}
		if len(mappings) == 1 {
			continue
		}
		validControlMount := false
		for _, mount := range destination.VolumeMounts {
			if mount.Name == snapshotv1alpha1.SnapshotControlVolumeName &&
				mount.MountPath == snapshotv1alpha1.SnapshotControlMountPath &&
				mount.SubPath == mapping.Destination {
				validControlMount = true
				break
			}
		}
		if !validControlMount {
			return fmt.Errorf(
				"multi-container restore destination %q requires %s mounted at %s with subPath %q",
				mapping.Destination,
				snapshotv1alpha1.SnapshotControlVolumeName,
				snapshotv1alpha1.SnapshotControlMountPath,
				mapping.Destination,
			)
		}
	}
	return nil
}

// resolveRestoreArtifact resolves the validated restore target to its physical
// checkpoint directory. A nil error always returns a complete artifact.
func (w *NodeController) resolveRestoreArtifact(podKey string, target *restoreTarget) (*restoreArtifact, error) {
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, target.ContentUID, target.SourceContainerName)
	if err != nil {
		return nil, err
	}
	ready, err := w.restoreArtifactReady(w.log, podKey, path)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			w.log.Error(err, "Unable to inspect restore artifact", "pod", podKey, "artifact_path", path)
			return nil, newRestorePendingError("ArtifactPending", fmt.Sprintf("Unable to inspect artifact %s; retrying", path))
		}
		return nil, err
	}
	if !ready {
		return nil, newRestorePendingError("ArtifactPending", fmt.Sprintf("Waiting for artifact %s", path))
	}
	return &restoreArtifact{
		SnapshotName:        target.SnapshotName,
		ContentUID:          target.ContentUID,
		SourceContainerName: target.SourceContainerName,
		Path:                path,
	}, nil
}

// restorePodContainers owns one restore pass for the Pod. Workers never write
// Pod status; they report one result each and this coordinator publishes the
// aggregate outcome.
func (w *NodeController) restorePodContainers(ctx context.Context, pod *corev1.Pod, plan *restorePlan, podKey string) bool {
	// Read the incoming state before persisting this pass. On agent restart,
	// RestoreInProgress makes each worker check its completion sentinel before
	// considering a CRIU replay.
	recovering := restoreInProgress(pod)
	message := fmt.Sprintf("Restoring %d destination container(s) from PodSnapshot %s", len(plan.mappings), plan.artifact.SnapshotName)
	if err := w.applyRestoredCondition(ctx, pod, corev1.ConditionFalse, snapshotv1alpha1.RestoreReasonInProgress, message); err != nil {
		emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreStatusUpdateFailedReason, err.Error())
		return true
	}

	results := make([]restoreResult, len(plan.mappings))
	var workers sync.WaitGroup
	for i, mapping := range plan.mappings {
		i, destination := i, mapping.Destination
		workers.Go(func() {
			results[i] = w.restoreDestination(ctx, pod, plan.artifact, destination, podKey, recovering)
		})
	}
	workers.Wait()

	return w.recordRestoreResults(ctx, pod, plan.artifact, results)
}

// recordRestoreResults publishes the aggregate Pod outcome after every worker
// in the current pass has returned.
func (w *NodeController) recordRestoreResults(ctx context.Context, pod *corev1.Pod, artifact *restoreArtifact, results []restoreResult) bool {
	byState := make(map[restoreResultState][]string, 3)
	for _, result := range results {
		byState[result.state] = append(byState[result.state], result.destination)
	}
	succeeded := byState[restoreResultSucceeded]
	failed := byState[restoreResultFailed]
	pending := byState[restoreResultPending]

	if len(pending) != 0 {
		message := fmt.Sprintf(
			"Restore from PodSnapshot %s remains in progress: %d succeeded, %d failed, %d pending (%s)",
			artifact.SnapshotName, len(succeeded), len(failed), len(pending), strings.Join(pending, ", "),
		)
		if err := w.applyRestoredCondition(ctx, pod, corev1.ConditionFalse, snapshotv1alpha1.RestoreReasonInProgress, message); err != nil {
			emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreStatusUpdateFailedReason, err.Error())
		}
		return true
	}

	if len(failed) == 0 {
		message := fmt.Sprintf("Restored %d destination container(s) from PodSnapshot %s: %s", len(succeeded), artifact.SnapshotName, strings.Join(succeeded, ", "))
		return w.finishRestore(ctx, pod, corev1.ConditionTrue, snapshotv1alpha1.RestoreReasonSucceeded, message) != nil
	}
	if len(succeeded) != 0 {
		message := fmt.Sprintf("Restored %d of %d destination containers from PodSnapshot %s; failed: %s", len(succeeded), len(results), artifact.SnapshotName, strings.Join(failed, ", "))
		return w.finishRestore(ctx, pod, corev1.ConditionFalse, snapshotv1alpha1.RestoreReasonPartiallySucceeded, message) != nil
	}
	message := fmt.Sprintf("Restore failed for all %d destination container(s) from PodSnapshot %s: %s", len(failed), artifact.SnapshotName, strings.Join(failed, ", "))
	return w.finishRestore(ctx, pod, corev1.ConditionFalse, snapshotv1alpha1.RestoreReasonFailed, message) != nil
}

// restoreDestination resolves and restores one destination independently of
// its siblings, returning only its worker-local outcome to the coordinator.
func (w *NodeController) restoreDestination(
	ctx context.Context,
	pod *corev1.Pod,
	artifact *restoreArtifact,
	destination, podKey string,
	recovering bool,
) restoreResult {
	result := restoreResult{destination: destination, state: restoreResultPending}
	containerID, _ := w.resolveRestoreContainerID(ctx, pod, destination, podKey)
	if containerID == "" {
		return result
	}

	startedAt := time.Now()
	log := w.log.WithValues("pod", podKey, "snapshot", artifact.SnapshotName, "destination", destination)
	log.Info("Restore target detected, triggering external restore",
		"content_uid", artifact.ContentUID,
		"source_container", artifact.SourceContainerName,
		"container_id", containerID,
	)
	emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeNormal, restoreRequestedReason, fmt.Sprintf("Restore requested from PodSnapshot %s for destination %s", artifact.SnapshotName, destination))

	if err := w.runRestore(ctx, pod, artifact, destination, containerID, startedAt, recovering); err != nil {
		result.state = restoreResultFailed
		log.Error(err, "Restore controller worker failed")
		emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeWarning, "RestoreWorkerFailed", err.Error())
		return result
	}
	result.state = restoreResultSucceeded
	return result
}

// resolveRestoreContainerID uses the kubelet-published status fast path, then
// polls the node runtime inline while this Pod's queue key remains in progress.
func (w *NodeController) resolveRestoreContainerID(ctx context.Context, pod *corev1.Pod, destination, podKey string) (string, bool) {
	if containerID := restoreContainerIDFromStatus(pod, destination); containerID != "" {
		return containerID, false
	}

	w.log.V(1).Info("Restore pod has no running container in Kubernetes status yet; polling node runtime",
		"pod", podKey,
		"container", destination,
	)
	deadlineAt := time.Now().Add(restoreContainerResolveTimeout)
	deadline := time.NewTimer(time.Until(deadlineAt))
	defer deadline.Stop()
	tick := time.NewTicker(restoreContainerResolveInterval)
	defer tick.Stop()
	for {
		resolveCtx, cancel := restoreContainerResolveAttemptContext(ctx, deadlineAt)
		containerID, err := w.runtime.ResolveContainerIDByPod(resolveCtx, pod.Name, pod.Namespace, destination)
		cancel()
		if err == nil && containerID != "" {
			w.log.V(1).Info("Resolved restore container via node runtime",
				"pod", podKey,
				"container", destination,
				"container_id", containerID,
			)
			return containerID, false
		}

		select {
		case <-deadline.C:
			w.log.V(1).Info("Timed out polling node runtime for restore container",
				"pod", podKey,
				"container", destination,
			)
			return "", true
		case <-ctx.Done():
			return "", false
		case <-tick.C:
		}
	}
}

// runRestore runs the full restore workflow for one destination container:
//  1. Call executor.Restore (inspect placeholder → nsrestore inside namespace).
//     nsrestore clears any stale restore-complete sentinel on the pod control
//     volume before CRIU, so a prior incarnation cannot release the restored
//     process early.
//  2. Write a restore-complete sentinel: the CRIU-restored process resumes
//     inside the polling loop that waits on this file, exits quiescence,
//     and resumes the engine
func (w *NodeController) runRestore(ctx context.Context, pod *corev1.Pod, artifact *restoreArtifact, destination, containerID string, startedAt time.Time, recovering bool) error {
	op := w.newRestoreOperation(pod, artifact, destination, containerID, startedAt)
	if recovering {
		completed, err := op.recoverCompletedRestore(ctx)
		if err != nil {
			return err
		}
		if completed {
			return nil
		}
	}

	restoreCtx := ctx
	if timeout := w.config.Restore.RestoreTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		restoreCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	placeholderHostPID, err := op.executeRestore(restoreCtx)
	if err != nil {
		var cleanupErr *executor.RestoreCleanupError
		if !errors.As(err, &cleanupErr) {
			return op.failRestore(ctx, err)
		}
		op.log.Error(cleanupErr, "Restore completed with cleanup errors")
		emitPodEvent(ctx, w.clientset, op.log, pod, snapshotEventComponent, corev1.EventTypeWarning, "RestoreCleanupFailed", cleanupErr.Error())
	}
	return op.completeRestore(ctx, placeholderHostPID)
}

// recoverCompletedRestore avoids replaying CRIU when the destination-scoped
// completion sentinel proves the operation already finished.
func (op *restoreOperation) recoverCompletedRestore(ctx context.Context) (bool, error) {
	condition := findRestoredCondition(op.pod)
	if condition == nil || condition.Status != corev1.ConditionFalse || condition.Reason != snapshotv1alpha1.RestoreReasonInProgress {
		return false, nil
	}

	hostPID, _, err := op.controller.runtime.ResolveContainer(ctx, op.containerID)
	if err != nil {
		return false, fmt.Errorf("resolve restore container before checking completion sentinel: %w", err)
	}
	exists, err := op.controller.controlSentinelExistsFn(hostPID, snapshotv1alpha1.RestoreCompleteFile)
	if err != nil {
		return false, fmt.Errorf("check restore completion sentinel: %w", err)
	}
	if !exists {
		return false, nil
	}
	return true, nil
}

func (w *NodeController) newRestoreOperation(
	pod *corev1.Pod,
	artifact *restoreArtifact,
	destination string,
	containerID string,
	startedAt time.Time,
) *restoreOperation {
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	return &restoreOperation{
		controller:  w,
		pod:         pod,
		artifact:    artifact,
		destination: destination,
		containerID: containerID,
		startedAt:   startedAt,
		log:         w.log.WithValues("pod", podKey, "snapshot", artifact.SnapshotName, "content_uid", artifact.ContentUID, "source_container", artifact.SourceContainerName, "destination_container", destination, "container_id", containerID),
	}
}

func (op *restoreOperation) executeRestore(ctx context.Context) (int, error) {
	w := op.controller
	req := executor.RestoreRequest{
		ContentUID:               op.artifact.ContentUID,
		BasePath:                 w.config.Storage.BasePath,
		ContainerID:              op.containerID,
		StartedAt:                op.startedAt,
		PodName:                  op.pod.Name,
		PodNamespace:             op.pod.Namespace,
		TargetPodIP:              op.pod.Status.PodIP,
		ArtifactContainerName:    op.artifact.SourceContainerName,
		DestinationContainerName: op.destination,
		Clientset:                w.clientset,
	}
	return w.restoreFn(ctx, w.runtime, op.log, req, w.injector)
}

func (op *restoreOperation) failRestore(ctx context.Context, restoreErr error) error {
	w := op.controller
	op.log.Error(restoreErr, "External restore failed")
	// Re-resolve because restore may fail before discovering the placeholder PID.
	placeholderHostPID, _, err := w.runtime.ResolveContainer(ctx, op.containerID)
	if err != nil {
		return errors.Join(restoreErr, fmt.Errorf("placeholder PID could not be resolved after restore failure: %w", err))
	}
	if err := w.sendSignalFn(op.log, placeholderHostPID, syscall.SIGKILL, "restore failed"); err != nil {
		return errors.Join(restoreErr, fmt.Errorf("placeholder could not be killed after restore failure: %w", err))
	}
	return restoreErr
}

func (op *restoreOperation) completeRestore(ctx context.Context, placeholderHostPID int) error {
	w := op.controller
	// Any PID inside the container mount namespace reaches the control
	// volume through /host/proc/<pid>/root.
	if err := w.writeControlSentinelFn(placeholderHostPID, snapshotv1alpha1.RestoreCompleteFile); err != nil {
		op.log.Error(err, "Failed to write restore-complete sentinel")
		if killErr := w.sendSignalFn(op.log, placeholderHostPID, syscall.SIGKILL, "restore sentinel failed"); killErr != nil {
			return errors.Join(fmt.Errorf("failed to write restore-complete sentinel: %w", err), fmt.Errorf("placeholder could not be killed: %w", killErr))
		}
		return fmt.Errorf("failed to write restore-complete sentinel: %w", err)
	}
	return nil
}

// applyRestoredCondition uses server-side apply against the status subresource.
// Pod conditions are an associative list keyed by type, so this field manager
// owns only nvidia.com/Restored and does not replace kubelet-owned conditions.
func (w *NodeController) applyRestoredCondition(ctx context.Context, pod *corev1.Pod, status corev1.ConditionStatus, reason, message string) error {
	setPodCondition(&pod.Status, corev1.PodCondition{
		Type:    corev1.PodConditionType(snapshotv1alpha1.RestoredCondition),
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	restored := findRestoredCondition(pod)
	condition := corev1apply.PodCondition().
		WithType(restored.Type).
		WithStatus(restored.Status).
		WithReason(restored.Reason).
		WithMessage(restored.Message).
		WithLastTransitionTime(restored.LastTransitionTime)
	configuration := corev1apply.Pod(pod.Name, pod.Namespace).
		WithStatus(corev1apply.PodStatus().WithConditions(condition))
	_, err := w.clientset.CoreV1().Pods(pod.Namespace).ApplyStatus(ctx, configuration, metav1.ApplyOptions{
		FieldManager: restoreStatusFieldManager,
		Force:        true,
	})
	return err
}

func (w *NodeController) addRestoreFinalizer(ctx context.Context, pod *corev1.Pod) error {
	if hasFinalizer(pod, restorePodFinalizer) {
		return nil
	}
	finalizers := append(append([]string{}, pod.Finalizers...), restorePodFinalizer)
	if err := w.patchRestoreFinalizers(ctx, pod, finalizers); err != nil {
		return fmt.Errorf("add restore protection finalizer: %w", err)
	}
	pod.Finalizers = finalizers
	return nil
}

func (w *NodeController) removeRestoreFinalizer(ctx context.Context, pod *corev1.Pod) error {
	if !hasFinalizer(pod, restorePodFinalizer) {
		return nil
	}
	finalizers := make([]string, 0, len(pod.Finalizers)-1)
	for _, finalizer := range pod.Finalizers {
		if finalizer != restorePodFinalizer {
			finalizers = append(finalizers, finalizer)
		}
	}
	if err := w.patchRestoreFinalizers(ctx, pod, finalizers); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove restore protection finalizer: %w", err)
	}
	pod.Finalizers = finalizers
	return nil
}

func (w *NodeController) patchRestoreFinalizers(ctx context.Context, pod *corev1.Pod, finalizers []string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": finalizers}})
	if err != nil {
		return err
	}
	_, err = w.clientset.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, ktypes.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (w *NodeController) restoreHandled(pod *corev1.Pod) bool {
	_, handled := w.handledRestores.Load(string(pod.UID))
	return handled
}

func (w *NodeController) markRestoreHandled(pod *corev1.Pod) {
	w.handledRestores.Store(string(pod.UID), struct{}{})
}

func (w *NodeController) removeRestoreFinalizerWithEvent(ctx context.Context, pod *corev1.Pod) bool {
	if err := w.removeRestoreFinalizer(ctx, pod); err != nil {
		w.log.Error(err, "Failed to remove restore protection finalizer", "pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
		emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreFinalizerUpdateFailedReason, err.Error())
		return true
	}
	return false
}

func (w *NodeController) finishRestore(
	ctx context.Context,
	pod *corev1.Pod,
	status corev1.ConditionStatus,
	reason, message string,
) error {
	if err := w.applyRestoredCondition(ctx, pod, status, reason, message); err != nil {
		emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreStatusUpdateFailedReason, fmt.Sprintf("Failed to record %s restore status: %v", reason, err))
		return err
	}
	w.markRestoreHandled(pod)
	eventType := corev1.EventTypeWarning
	if status == corev1.ConditionTrue {
		eventType = corev1.EventTypeNormal
	}
	emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, eventType, reason, message)
	finalizerErr := w.removeRestoreFinalizer(ctx, pod)
	if finalizerErr != nil {
		w.log.Error(finalizerErr, "Failed to remove restore protection finalizer", "pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
		emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreFinalizerUpdateFailedReason, finalizerErr.Error())
	}
	return finalizerErr
}

func (w *NodeController) failRestorePod(ctx context.Context, pod *corev1.Pod, cause error) bool {
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	w.log.Error(cause, "Restore request failed", "pod", podKey)
	err := w.finishRestore(
		ctx,
		pod,
		corev1.ConditionFalse,
		snapshotv1alpha1.RestoreReasonFailed,
		cause.Error(),
	)
	return err != nil
}

func (w *NodeController) handleRestorePreflightError(ctx context.Context, pod *corev1.Pod, cause error) bool {
	var pending *restorePendingError
	if !errors.As(cause, &pending) {
		return w.failRestorePod(ctx, pod, cause)
	}

	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if restoreInProgress(pod) {
		w.log.V(1).Info("Restore remains in progress while a dependency is pending", "pod", podKey, "reason", pending.reason, "message", pending.message)
		emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeNormal, pending.reason, pending.message)
		return true
	}
	err := w.applyRestoredCondition(ctx, pod, corev1.ConditionFalse, pending.reason, pending.message)
	if err != nil {
		w.log.Error(err, "Failed to apply pending restore condition", "pod", podKey)
		emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreStatusUpdateFailedReason, fmt.Sprintf("Failed to record pending restore status: %v", err))
	}
	w.log.V(1).Info("Restore preflight is pending", "pod", podKey, "reason", pending.reason, "message", pending.message)
	emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeNormal, pending.reason, pending.message)
	return true
}

func (w *NodeController) tryAcquire(key string) bool {
	w.inFlightMu.Lock()
	defer w.inFlightMu.Unlock()
	if _, held := w.inFlight[key]; held {
		return false
	}
	w.inFlight[key] = struct{}{}
	return true
}

func (w *NodeController) release(key string) {
	w.inFlightMu.Lock()
	defer w.inFlightMu.Unlock()
	delete(w.inFlight, key)
}

// checkpointInFlight reports whether a capture currently holds the in-flight
// guard for key without acquiring it.
func (w *NodeController) checkpointInFlight(key string) bool {
	w.inFlightMu.Lock()
	defer w.inFlightMu.Unlock()
	_, held := w.inFlight[key]
	return held
}

// podRefIndex is the PodSnapshotContent informer index keyed by source pod ("<namespace>/<name>").
const podRefIndex = "byPodRef"

// podRefIndexFunc indexes a PodSnapshotContent by its source pod ("<snapshotRef.namespace>/<source.podRef.name>").
// It runs against the dynamic informer's *unstructured.Unstructured objects; an unexpected type or a
// missing field yields no index entry (nil) rather than an error, so it never poisons the index.
func podRefIndexFunc(obj interface{}) ([]string, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, nil
	}
	ns, _, _ := unstructured.NestedString(u.Object, "spec", "snapshotRef", "namespace")
	name, _, _ := unstructured.NestedString(u.Object, "spec", "source", "podRef", "name")
	if ns == "" || name == "" {
		return nil, nil
	}
	return []string{ns + "/" + name}, nil
}

// contentFromInformerObj converts a dynamic informer object (or its DeletedFinalStateUnknown
// tombstone) to a typed PodSnapshotContent. It returns false on an unexpected type.
func contentFromInformerObj(obj interface{}) (*snapshotv1alpha1.PodSnapshotContent, bool) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false
	}
	content := &snapshotv1alpha1.PodSnapshotContent{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, content); err != nil {
		return nil, false
	}
	return content, true
}

// chooseActiveContent returns the name of the oldest non-terminal PodSnapshotContent among the indexed
// objects (oldest first by CreationTimestamp, ties broken by Name), or "" when none are active.
// Driving the oldest until it finishes gives deterministic, stable selection across pod events.
func chooseActiveContent(objs []interface{}) string {
	var chosen *snapshotv1alpha1.PodSnapshotContent
	for _, obj := range objs {
		content, ok := contentFromInformerObj(obj)
		if !ok || isContentTerminal(content) {
			continue
		}
		if chosen == nil ||
			content.CreationTimestamp.Before(&chosen.CreationTimestamp) ||
			(content.CreationTimestamp.Equal(&chosen.CreationTimestamp) && content.Name < chosen.Name) {
			chosen = content
		}
	}
	if chosen == nil {
		return ""
	}
	return chosen.Name
}

func (w *NodeController) restoreArtifactReady(log logr.Logger, podKey, artifactPath string) (bool, error) {
	info, err := os.Stat(artifactPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.V(1).Info("Artifact not ready on disk, skipping restore", "pod", podKey, "artifact_path", artifactPath)
			return false, nil
		}
		return false, fmt.Errorf("stat artifact path %s: %w", artifactPath, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("artifact path %s is not a directory", artifactPath)
	}
	return true, nil
}

// restoreContainerIDFromStatus is the fast path when kubelet has already
// published the target container ID. The restore controller polls the node
// runtime when status publication lags container creation.
func restoreContainerIDFromStatus(pod *corev1.Pod, containerName string) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName && status.ContainerID != "" && status.State.Running != nil {
			return snapshotruntime.StripCRIScheme(status.ContainerID)
		}
	}
	return ""
}

func restoreContainerResolveAttemptContext(ctx context.Context, deadlineAt time.Time) (context.Context, context.CancelFunc) {
	attemptDeadline := time.Now().Add(containerResolveAttemptTimeout)
	if deadlineAt.Before(attemptDeadline) {
		attemptDeadline = deadlineAt
	}
	return context.WithDeadline(ctx, attemptDeadline)
}

func findRestoredCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		condition := &pod.Status.Conditions[i]
		if condition.Type == corev1.PodConditionType(snapshotv1alpha1.RestoredCondition) {
			return condition
		}
	}
	return nil
}

func restoreInProgress(pod *corev1.Pod) bool {
	condition := findRestoredCondition(pod)
	return condition != nil && condition.Status == corev1.ConditionFalse && condition.Reason == snapshotv1alpha1.RestoreReasonInProgress
}

func isRestoreSucceeded(pod *corev1.Pod) bool {
	return snapshotv1alpha1.ClassifyRestoreOutcome(pod.Status.Conditions) == snapshotv1alpha1.RestoreOutcomeSucceeded
}

// isRestorePartiallySucceeded reports the terminal mixed worker outcome.
func isRestorePartiallySucceeded(pod *corev1.Pod) bool {
	return snapshotv1alpha1.ClassifyRestoreOutcome(pod.Status.Conditions) == snapshotv1alpha1.RestoreOutcomePartiallySucceeded
}

func isRestoreTerminal(pod *corev1.Pod) bool {
	switch snapshotv1alpha1.ClassifyRestoreOutcome(pod.Status.Conditions) {
	case snapshotv1alpha1.RestoreOutcomeSucceeded,
		snapshotv1alpha1.RestoreOutcomeFailed,
		snapshotv1alpha1.RestoreOutcomePartiallySucceeded:
		return true
	default:
		return false
	}
}

func isRestorePodActive(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning
}

func hasFinalizer(pod *corev1.Pod, finalizer string) bool {
	for _, item := range pod.Finalizers {
		if item == finalizer {
			return true
		}
	}
	return false
}

func newRestorePendingError(reason, message string) error {
	return &restorePendingError{reason: reason, message: message}
}
