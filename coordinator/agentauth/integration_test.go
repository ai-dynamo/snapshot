//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Requires envtest's kube-apiserver and etcd binaries through KUBEBUILDER_ASSETS.
// The Authorizer acts as the agents' ServiceAccount, with RBAC to create
// TokenReviews and nothing else but Kubernetes's defaults.
func TestAuthorizeAgainstAnAPIServer(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	admin, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	k := cluster{t: t, admin: admin}

	k.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agents.Namespace}})
	k.create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: agents.Name}})
	k.create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "workload"}})
	k.create(&rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-coordinator"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{authenticationv1.GroupName}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"},
		}},
	})
	k.create(&rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-coordinator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "snapshot-coordinator"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: agents.Namespace, Name: agents.Name}},
	})
	agent := k.pod("snapshot-agent-0", agents.Name)
	doomed := k.pod("snapshot-agent-1", agents.Name)
	workload := k.pod("workload-0", "workload")

	agentCfg := rest.CopyConfig(cfg)
	agentCfg.Impersonate = rest.ImpersonationConfig{
		UserName: serviceAccountUser(agents),
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + agents.Namespace, "system:authenticated"},
	}
	agentClient, err := kubernetes.NewForConfig(agentCfg)
	require.NoError(t, err)
	k.waitUntilAllowed(agentClient)
	reviews := &countedReviews{TokenReviewInterface: agentClient.AuthenticationV1().TokenReviews()}
	newAuthorizer := func() *Authorizer {
		auth, err := New(t.Context(), Config{
			Placement: &placement{nodes: map[types.UID]map[string]string{
				attempt: {member: node, "member-1": "node-1"},
			}},
			TokenReviews:   reviews,
			RESTConfig:     agentCfg,
			Audience:       audience,
			ServiceAccount: agents,
			Log:            testr.NewWithOptions(t, testr.Options{Verbosity: 1}),
		})
		require.NoError(t, err)
		return auth
	}
	auth := newAuthorizer()
	authorize := func(auth *Authorizer, member, token string) error {
		ctx, cancel := context.WithTimeout(t.Context(), server.AuthorizeTimeout)
		defer cancel()
		return auth.Authorize(ctx, attempt, member, token)
	}
	token := k.token(agents.Name, agent, audience)

	t.Run("the member on the token's node", func(t *testing.T) {
		require.NoError(t, authorize(auth, member, token))
	})
	for name, tc := range map[string]struct {
		member, token string
		want          error
	}{
		"another member": {member: "member-9", token: token, want: server.ErrPermissionDenied},
		"another node":   {member: "member-1", token: token, want: server.ErrPermissionDenied},
		"another audience": {
			member: member, token: k.token(agents.Name, agent, "another-audience"), want: server.ErrUnauthenticated,
		},
		"another ServiceAccount": {
			member: member, token: k.token("workload", workload, audience), want: server.ErrPermissionDenied,
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := reviews.n.Load()
			require.ErrorIs(t, authorize(auth, tc.member, tc.token), tc.want)
			assert.Equal(t, before, reviews.n.Load(), "reviewed the token")
		})
	}
	t.Run("the Pod deleted", func(t *testing.T) {
		token := k.token(agents.Name, doomed, audience)
		require.NoError(t, authorize(newAuthorizer(), member, token))
		require.NoError(t, admin.CoreV1().Pods(agents.Namespace).Delete(t.Context(), doomed.Name,
			metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}))
		// The API server reuses a token's authentication for 10 s, and an
		// Authorizer its TokenReview for longer, so ask a new one each time.
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			before := reviews.n.Load()
			assert.ErrorIs(c, authorize(newAuthorizer(), member, token), server.ErrUnauthenticated)
			assert.Equal(c, before+1, reviews.n.Load(), "did not review the token")
		}, 30*time.Second, 500*time.Millisecond)
	})
}

// cluster creates the test's objects.
type cluster struct {
	t     *testing.T
	admin *kubernetes.Clientset
}

// create creates obj, in the agents' namespace when it is namespaced.
func (k cluster) create(obj any) {
	k.t.Helper()
	ctx, ns := k.t.Context(), agents.Namespace
	var err error
	switch o := obj.(type) {
	case *corev1.Namespace:
		_, err = k.admin.CoreV1().Namespaces().Create(ctx, o, metav1.CreateOptions{})
	case *corev1.ServiceAccount:
		_, err = k.admin.CoreV1().ServiceAccounts(ns).Create(ctx, o, metav1.CreateOptions{})
	case *rbacv1.ClusterRole:
		_, err = k.admin.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
	case *rbacv1.ClusterRoleBinding:
		_, err = k.admin.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
	default:
		k.t.Fatalf("cannot create a %T", obj)
	}
	require.NoError(k.t, err)
}

// pod creates a Pod of the ServiceAccount sa on node.
func (k cluster) pod(name, sa string) *corev1.Pod {
	k.t.Helper()
	pod, err := k.admin.CoreV1().Pods(agents.Namespace).Create(k.t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			ServiceAccountName: sa,
			NodeName:           node,
			Containers:         []corev1.Container{{Name: "main", Image: "main"}},
		},
	}, metav1.CreateOptions{})
	require.NoError(k.t, err)
	return pod
}

// token requests a token of the ServiceAccount sa for audience, bound to pod.
func (k cluster) token(sa string, pod *corev1.Pod, audience string) string {
	k.t.Helper()
	r, err := k.admin.CoreV1().ServiceAccounts(agents.Namespace).CreateToken(k.t.Context(), sa,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: ptr.To[int64](600),
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID,
			},
		}}, metav1.CreateOptions{})
	require.NoError(k.t, err)
	return r.Status.Token
}

// waitUntilAllowed waits until client may create TokenReviews, once the
// API server's authorizer has seen the binding.
func (k cluster) waitUntilAllowed(client *kubernetes.Clientset) {
	k.t.Helper()
	require.EventuallyWithT(k.t, func(c *assert.CollectT) {
		r, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(k.t.Context(),
			&authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Group: authenticationv1.GroupName, Resource: "tokenreviews", Verb: "create",
				},
			}}, metav1.CreateOptions{})
		if assert.NoError(c, err) {
			assert.True(c, r.Status.Allowed)
		}
	}, 10*time.Second, 100*time.Millisecond)
}

// countedReviews counts the TokenReviews it creates.
type countedReviews struct {
	authenticationv1client.TokenReviewInterface
	n atomic.Int32
}

// Create implements TokenReviewInterface.
func (r *countedReviews) Create(ctx context.Context, review *authenticationv1.TokenReview,
	opts metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	r.n.Add(1)
	return r.TokenReviewInterface.Create(ctx, review, opts)
}
