// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package group

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/ai-dynamo/snapshot/coordinator"
)

// What the chart gives every agent for reaching a coordinator, at fixed paths.
const (
	// CAFile is the trust bundle that verifies the coordinators' serving
	// certificate.
	CAFile = "/etc/snapshot-coordinator/ca/ca.crt"

	// TokenFile is the agent's projected ServiceAccount token, for the
	// audience the coordinators admit.
	TokenFile = "/var/run/secrets/snapshot-coordinator/token"

	// DefaultPort is the coordinators' client port, on each node's IP.
	DefaultPort = 4224
)

// locateInterval spaces the reads of a Locator that waits for the
// coordinator's node or its agent Pod.
const locateInterval = 2 * time.Second

// Locator builds the coordinator.Config that a member of an attempt connects
// with. The coordinator of an attempt runs in the agent Pod on the node of the
// attempt's rank-0 member, as the group resource's status records it. That
// Pod uses the host's network, so its IP is the node's.
type Locator struct {
	// Client reads the agent Pods.
	Client kubernetes.Interface

	// Namespace and Selector select the agent Pods, one on each node.
	Namespace string
	Selector  labels.Selector

	// Port is the coordinators' client port.
	Port int

	// ServerName is the name the coordinators' serving certificate carries.
	ServerName string

	// CAFile and TokenFile are read on every connect, so a replaced CA or a
	// rotated token is used from the next connect on. Empty means CAFile and
	// TokenFile.
	CAFile, TokenFile string

	// Interval spaces the reads while the coordinator's node, or its agent
	// Pod's IP, is not known yet. Zero means two seconds.
	Interval time.Duration

	// Log receives the waits. It is handed to the connection too, which
	// never logs a key, a value, a barrier name or a token.
	Log logr.Logger
}

// SessionNode reads the node of an attempt's rank-0 member from its group
// resource, or "" while the status records none. It reports ErrNoAttempt once
// the group resource is gone or was replaced.
type SessionNode func(context.Context) (string, error)

// ErrNoAttempt reports that an attempt's group resource is gone, or another
// resource holds its name: no coordinator will ever hold its session.
var ErrNoAttempt = errors.New("the attempt's group resource is gone")

// Config returns the Config that connects member to the attempt, the UID of
// its group resource. It waits while sessionNode reports no node, and then
// while that node has no agent Pod with an IP, until ctx is done. A read that
// fails is waited out as well, except ErrNoAttempt, which ends the wait.
func (l *Locator) Config(ctx context.Context, attempt, member string, sessionNode SessionNode) (coordinator.Config, error) {
	log := l.Log.WithValues("attempt", attempt, "member", member)
	interval := l.Interval
	if interval <= 0 {
		interval = locateInterval
	}
	var waiting string
	for {
		ip, reason, err := l.locate(ctx, sessionNode)
		if err != nil {
			return coordinator.Config{}, err
		}
		if ip != "" {
			return l.config(attempt, member, ip), nil
		}
		if reason != waiting {
			log.Info("Waiting for the pod group's coordinator", "reason", reason)
			waiting = reason
		}
		select {
		case <-ctx.Done():
			return coordinator.Config{}, fmt.Errorf("find the coordinator of attempt %s: %s: %w", attempt, reason, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// locate returns the coordinator's IP, or why it is not known yet.
func (l *Locator) locate(ctx context.Context, sessionNode SessionNode) (ip, waiting string, err error) {
	node, err := sessionNode(ctx)
	if errors.Is(err, ErrNoAttempt) {
		return "", "", err
	}
	if err != nil {
		return "", fmt.Sprintf("reading the node of the rank-0 member failed: %v", err), nil
	}
	if node == "" {
		return "", "the rank-0 member has no node yet", nil
	}
	pods, err := l.Client.CoreV1().Pods(l.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: l.Selector.String(),
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node).String(),
	})
	if err != nil {
		return "", fmt.Sprintf("listing the agent Pods on node %s failed: %v", node, err), nil
	}
	if ip := agentIP(pods.Items); ip != "" {
		return ip, "", nil
	}
	return "", fmt.Sprintf("node %s has no agent Pod with an IP", node), nil
}

// agentIP returns the IP of an agent Pod on the node. During a rollout the old
// Pod and the new one share the node's IP, so any of them will do, and one
// that is not being deleted is taken first.
func agentIP(pods []corev1.Pod) string {
	ip := ""
	for i := range pods {
		switch {
		case pods[i].Status.PodIP == "":
		case pods[i].DeletionTimestamp == nil:
			return pods[i].Status.PodIP
		case ip == "":
			ip = pods[i].Status.PodIP
		}
	}
	return ip
}

func (l *Locator) config(attempt, member, ip string) coordinator.Config {
	caFile, tokenFile := l.CAFile, l.TokenFile
	if caFile == "" {
		caFile = CAFile
	}
	if tokenFile == "" {
		tokenFile = TokenFile
	}
	return coordinator.Config{
		URLs:       []string{"tls://" + net.JoinHostPort(ip, strconv.Itoa(l.Port))},
		RootCAs:    ReadCAs(caFile),
		ServerName: l.ServerName,
		Attempt:    attempt,
		Member:     member,
		Token:      ReadToken(tokenFile),
		Log:        l.Log,
	}
}

// ReadCAs returns a source of the CAs in the PEM bundle at path, which reads
// the file on every call.
func ReadCAs(path string) func() (*x509.CertPool, error) {
	return func() (*x509.CertPool, error) {
		bundle, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read the coordinator's CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(bundle) {
			return nil, fmt.Errorf("the coordinator's CA bundle %s holds no certificate", path)
		}
		return pool, nil
	}
}

// ReadToken returns a source of the token at path, which reads the file on
// every call. The error never holds the token.
func ReadToken(path string) func() (string, error) {
	return func() (string, error) {
		token, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read the coordinator token: %w", err)
		}
		token = bytes.TrimSpace(token)
		if len(token) == 0 {
			return "", errors.New("the coordinator token is empty")
		}
		return string(token), nil
	}
}
