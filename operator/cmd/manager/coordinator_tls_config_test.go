// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"flag"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinatortls"
)

func TestCoordinatorTLSFlags(t *testing.T) {
	all := []string{
		"--coordinator-tls-namespace=snapshot-system",
		"--coordinator-ca-secret=snapshot-coordinator-ca",
		"--coordinator-tls-secret=snapshot-coordinator-tls",
		"--coordinator-ca-bundle=snapshot-coordinator-ca-bundle",
		"--coordinator-server-name=snapshot-coordinator",
	}
	// with returns all, with the last flag of the same name overriding.
	with := func(flag string) []string { return append(slices.Clone(all), flag) }
	for _, tc := range []struct {
		name       string
		args       []string
		wantAdded  bool
		wantBundle string
		wantErr    string
	}{
		{name: "none", args: nil},
		{name: "all", args: all, wantAdded: true},
		{name: "the bundle shares the CA Secret's name", args: with("--coordinator-ca-bundle=snapshot-coordinator-ca"),
			wantAdded: true, wantBundle: "snapshot-coordinator-ca"},
		{name: "some", args: all[:3],
			wantErr: "set every coordinator TLS flag or none: missing --coordinator-ca-bundle, --coordinator-server-name"},
		{name: "only the server name", args: all[4:],
			wantErr: "missing --coordinator-tls-namespace, --coordinator-ca-secret, --coordinator-tls-secret, " +
				"--coordinator-ca-bundle"},
		{name: "the CA and serving Secrets share a name", args: with("--coordinator-tls-secret=snapshot-coordinator-ca"),
			wantErr: `--coordinator-ca-secret and --coordinator-tls-secret both name "snapshot-coordinator-ca"`},
		{name: "a server name that is an IP address", args: with("--coordinator-server-name=127.0.0.1"),
			wantErr: `--coordinator-server-name "127.0.0.1" is an IP address`},
		{name: "a server name that is not a DNS name", args: with("--coordinator-server-name=Snapshot Coordinator"),
			wantErr: `--coordinator-server-name "Snapshot Coordinator"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			cfg := bindCoordinatorTLSFlags(flags)
			require.NoError(t, flags.Parse(tc.args))

			mgr := &recordingManager{}
			err := addCoordinatorTLSIssuer(mgr, cfg)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Empty(t, mgr.added)
				return
			}
			require.NoError(t, err)
			if !tc.wantAdded {
				assert.Empty(t, mgr.added, "without the flags, the operator issues nothing")
				return
			}
			require.Len(t, mgr.added, 1)
			issuer, ok := mgr.added[0].(*coordinatortls.Issuer)
			require.True(t, ok, "the runnable is the issuer")
			assert.True(t, issuer.NeedLeaderElection())
			assert.Equal(t, clock.RealClock{}, cfg.Clock)
			got := *cfg
			got.Clock, got.Log = nil, logr.Logger{}
			assert.Equal(t, coordinatortls.Config{
				Namespace:  "snapshot-system",
				CASecret:   "snapshot-coordinator-ca",
				TLSSecret:  "snapshot-coordinator-tls",
				CABundle:   cmp.Or(tc.wantBundle, "snapshot-coordinator-ca-bundle"),
				ServerName: "snapshot-coordinator",
			}, got)
		})
	}
}

// recordingManager records the runnables added to it; the issuer's
// construction reads its clients, and nothing else.
type recordingManager struct {
	manager.Manager
	added []manager.Runnable
}

func (m *recordingManager) Add(r manager.Runnable) error {
	m.added = append(m.added, r)
	return nil
}

func (m *recordingManager) GetAPIReader() client.Reader { return nil }

func (m *recordingManager) GetClient() client.Client { return nil }
