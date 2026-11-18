// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package group

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const agentNamespace = "snapshot-system"

var agentLabels = map[string]string{"app.kubernetes.io/component": "snapshot-agent"}

func agentPod(name, node, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentNamespace, Labels: agentLabels},
		Spec:       corev1.PodSpec{NodeName: node, HostNetwork: true},
		Status:     corev1.PodStatus{PodIP: ip},
	}
}

func testLocator(t *testing.T, objects ...runtime.Object) (*Locator, *fake.Clientset) {
	client := fake.NewClientset(objects...)
	return &Locator{
		Client:     client,
		Namespace:  agentNamespace,
		Selector:   labels.SelectorFromSet(agentLabels),
		Port:       DefaultPort,
		ServerName: "snapshot-coordinator.snapshot-system",
		CAFile:     filepath.Join(t.TempDir(), "ca.crt"),
		TokenFile:  filepath.Join(t.TempDir(), "token"),
		Interval:   time.Millisecond,
		Log:        testr.New(t),
	}, client
}

func TestLocatorWaitsForTheRankZeroNode(t *testing.T) {
	l, client := testLocator(t, agentPod("agent-a", "node-a", "10.0.0.5"))
	var reads atomic.Int32
	sessionNode := func(context.Context) (string, error) {
		if reads.Add(1) < 3 {
			return "", nil
		}
		return "node-a", nil
	}

	cfg, err := l.Config(within(t), "attempt-1", "member-0", sessionNode)
	require.NoError(t, err)
	assert.EqualValues(t, 3, reads.Load(), "a node the status does not record yet is waited for")
	assert.Equal(t, []string{"tls://10.0.0.5:4224"}, cfg.URLs)
	assert.Equal(t, "snapshot-coordinator.snapshot-system", cfg.ServerName)
	assert.Equal(t, "attempt-1", cfg.Attempt)
	assert.Equal(t, "member-0", cfg.Member)

	var lists []clienttesting.ListAction
	for _, action := range client.Actions() {
		if list, ok := action.(clienttesting.ListAction); ok {
			lists = append(lists, list)
		}
	}
	require.Len(t, lists, 1, "the agent Pods are read once the node is known")
	assert.Equal(t, agentNamespace, lists[0].GetNamespace())
	assert.Equal(t, "spec.nodeName=node-a", lists[0].GetListRestrictions().Fields.String())
	assert.Equal(t, "app.kubernetes.io/component=snapshot-agent", lists[0].GetListRestrictions().Labels.String())
}

func TestLocatorWaitsForTheAgentPodsIP(t *testing.T) {
	l, client := testLocator(t, agentPod("agent-a", "node-a", ""))
	var reads atomic.Int32
	sessionNode := func(ctx context.Context) (string, error) {
		if reads.Add(1) == 3 {
			pod := agentPod("agent-a", "node-a", "10.0.0.5")
			if _, err := client.CoreV1().Pods(agentNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
				return "", err
			}
		}
		return "node-a", nil
	}

	cfg, err := l.Config(within(t), "attempt-1", "member-0", sessionNode)
	require.NoError(t, err)
	assert.EqualValues(t, 3, reads.Load())
	assert.Equal(t, []string{"tls://10.0.0.5:4224"}, cfg.URLs)
}

func TestLocatorFormatsAnIPv6Address(t *testing.T) {
	l, _ := testLocator(t, agentPod("agent-a", "node-a", "fd00::5"))
	cfg, err := l.Config(within(t), "attempt-1", "member-0", func(context.Context) (string, error) { return "node-a", nil })
	require.NoError(t, err)
	assert.Equal(t, []string{"tls://[fd00::5]:4224"}, cfg.URLs)
}

func TestLocatorPrefersAnAgentPodThatIsNotBeingDeleted(t *testing.T) {
	old := agentPod("agent-old", "node-a", "10.0.0.4")
	old.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	assert.Equal(t, "10.0.0.5", agentIP([]corev1.Pod{*old, *agentPod("agent-new", "node-a", "10.0.0.5")}))
	assert.Equal(t, "10.0.0.4", agentIP([]corev1.Pod{*old}), "a Pod being deleted still has the node's IP")
	assert.Empty(t, agentIP(nil))
}

func TestLocatorStopsWaiting(t *testing.T) {
	t.Run("when the context ends", func(t *testing.T) {
		l, _ := testLocator(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := l.Config(ctx, "attempt-1", "member-0", func(context.Context) (string, error) { return "", nil })
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "the rank-0 member has no node yet")
	})
	t.Run("when the group resource is gone", func(t *testing.T) {
		l, _ := testLocator(t)
		gone := fmt.Errorf("PodGroupSnapshot inference/pair: %w", ErrNoAttempt)
		_, err := l.Config(within(t), "attempt-1", "member-0", func(context.Context) (string, error) { return "", gone })
		require.ErrorIs(t, err, ErrNoAttempt)
	})
	t.Run("not while a read of the group resource fails", func(t *testing.T) {
		l, _ := testLocator(t, agentPod("agent-a", "node-a", "10.0.0.5"))
		var reads atomic.Int32
		cfg, err := l.Config(within(t), "attempt-1", "member-0", func(context.Context) (string, error) {
			if reads.Add(1) < 3 {
				return "", errors.New("the API server is unavailable")
			}
			return "node-a", nil
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"tls://10.0.0.5:4224"}, cfg.URLs)
	})
}

func TestLocatorReadsTheCAAndTheTokenOnEveryConnect(t *testing.T) {
	l, _ := testLocator(t, agentPod("agent-a", "node-a", "10.0.0.5"))
	cfg, err := l.Config(within(t), "attempt-1", "member-0", func(context.Context) (string, error) { return "node-a", nil })
	require.NoError(t, err)

	_, err = cfg.RootCAs()
	require.ErrorIs(t, err, os.ErrNotExist, "a missing bundle fails only the handshake that needs it")
	_, err = cfg.Token()
	require.ErrorIs(t, err, os.ErrNotExist)

	first, second := testCertificate(t, "first"), testCertificate(t, "second")
	require.NoError(t, os.WriteFile(l.CAFile, first, 0o600))
	require.NoError(t, os.WriteFile(l.TokenFile, []byte("token-1\n"), 0o600))
	pool, err := cfg.RootCAs()
	require.NoError(t, err)
	assert.True(t, pool.Equal(certPool(t, first)))
	token, err := cfg.Token()
	require.NoError(t, err)
	assert.Equal(t, "token-1", token)

	require.NoError(t, os.WriteFile(l.CAFile, second, 0o600))
	require.NoError(t, os.WriteFile(l.TokenFile, []byte("token-2"), 0o600))
	pool, err = cfg.RootCAs()
	require.NoError(t, err)
	assert.True(t, pool.Equal(certPool(t, second)), "a replaced CA is trusted from the next connect on")
	token, err = cfg.Token()
	require.NoError(t, err)
	assert.Equal(t, "token-2", token, "a rotated token is presented from the next connect on")

	require.NoError(t, os.WriteFile(l.CAFile, []byte("not a certificate"), 0o600))
	require.NoError(t, os.WriteFile(l.TokenFile, []byte(" \n"), 0o600))
	_, err = cfg.RootCAs()
	require.ErrorContains(t, err, "holds no certificate")
	_, err = cfg.Token()
	require.EqualError(t, err, "the coordinator token is empty")
}

func TestLocatorDefaultsToTheChartsPaths(t *testing.T) {
	l, _ := testLocator(t)
	l.CAFile, l.TokenFile = "", ""
	cfg := l.config("attempt-1", "member-0", "10.0.0.5")
	_, err := cfg.RootCAs()
	require.ErrorContains(t, err, CAFile)
	_, err = cfg.Token()
	require.ErrorContains(t, err, TokenFile)
}

func testCertificate(t *testing.T, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func certPool(t *testing.T, bundle []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(bundle))
	return pool
}
