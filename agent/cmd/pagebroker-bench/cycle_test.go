// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/stretchr/testify/require"
)

type fakeBroker struct {
	t                                     *testing.T
	root, publication                     string
	paths                                 map[string]string
	ids                                   []string
	artifact                              *pb.PublishedArtifact
	engine                                string
	prepareError, abortError, commitError error
	corrupt                               bool
	aborts                                int
}

func (f *fakeBroker) prepare(tx string, engine string) (string, error) {
	f.ids = append(f.ids, tx)
	if engine != f.engine {
		f.t.Fatalf("engine = %s", engine)
	}
	path := filepath.Join(f.root, tx)
	if err := os.Mkdir(path, 0700); err != nil {
		return "", err
	}
	f.paths[tx] = path
	return path, f.prepareError
}
func (f *fakeBroker) PrepareCheckpoint(_ context.Context, tx, dest string) (string, error) {
	f.publication = dest
	return f.prepare(tx, "posix-copy")
}
func (f *fakeBroker) PrepareArtifactCheckpoint(_ context.Context, tx string, target *pb.ArtifactTarget, engine *pb.IOEngine) (string, error) {
	if target.StoreId != "test-store" || engine.GetModelStreamer() == nil {
		f.t.Fatal("wrong S3 selection")
	}
	return f.prepare(tx, "model-streamer")
}
func (f *fakeBroker) Commit(_ context.Context, tx string) error {
	if f.commitError != nil {
		return f.commitError
	}
	path := f.paths[tx]
	if _, err := os.Stat(f.publication); errors.Is(err, os.ErrNotExist) {
		return os.Rename(path, f.publication)
	}
	return os.RemoveAll(path)
}
func (f *fakeBroker) CommitCheckpoint(ctx context.Context, tx, storeID string) (*pb.PublishedArtifact, error) {
	if storeID != "test-store" {
		f.t.Fatal("wrong commit store")
	}
	return f.artifact, f.Commit(ctx, tx)
}
func (f *fakeBroker) StagedRestore(ctx context.Context, tx, source string) (string, error) {
	if source != f.publication {
		f.t.Fatal("wrong publication")
	}
	return f.restore(ctx, tx, "posix-copy")
}
func (f *fakeBroker) StagedArtifactRestore(ctx context.Context, tx string, artifact *pb.PublishedArtifact, engine *pb.IOEngine) (string, error) {
	if artifact != f.artifact || engine.GetModelStreamer() == nil {
		f.t.Fatal("descriptor was replaced")
	}
	return f.restore(ctx, tx, "model-streamer")
}
func (f *fakeBroker) restore(ctx context.Context, tx string, engine string) (string, error) {
	path, err := f.prepare(tx, engine)
	if err != nil {
		return "", err
	}
	m, err := inspect(ctx, f.publication)
	if err != nil {
		return "", err
	}
	if err := copyFixture(ctx, f.publication, path, m); err != nil {
		return "", err
	}
	if f.corrupt {
		if err := os.WriteFile(filepath.Join(path, "nested/small.bin"), []byte("corrupt"), 0640); err != nil {
			return "", err
		}
	}
	return path, nil
}
func (f *fakeBroker) Abort(_ context.Context, tx string) error {
	f.aborts++
	if f.abortError != nil {
		return f.abortError
	}
	return os.RemoveAll(f.paths[tx])
}

func cycleFixture(t *testing.T, backend string, engine string) (request, *fakeBroker, context.Context) {
	t.Helper()
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	require.NoError(t, os.Mkdir(staging, 0700))
	r := request{SchemaVersion: 1, Action: "cycle", RunID: "run", CycleID: "cycle", Socket: "unused",
		StagingRoot: staging, Backend: backend, TransferEngine: engine, Destination: filepath.Join(root, "published"),
		StoreID: "test-store", PayloadBytes: 4099, TimeoutSeconds: 60}
	f := &fakeBroker{t: t, root: staging, publication: r.Destination, paths: map[string]string{}, engine: engine,
		artifact: &pb.PublishedArtifact{StoreId: "test-store", ArtifactHandle: "opaque-test-handle"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return r, f, ctx
}

func TestCycleSelections(t *testing.T) {
	for _, tc := range []struct {
		backend string
		engine  string
	}{
		{"filesystem", "posix-copy"}, {"s3", "model-streamer"},
	} {
		t.Run(tc.backend+"/"+tc.engine, func(t *testing.T) {
			r, f, ctx := cycleFixture(t, tc.backend, tc.engine)
			out := report{Timings: map[string]float64{}}
			require.NoError(t, cycle(ctx, f, r, &out))
			if out.Publications != 1 || out.Restores != 2 || !out.TransactionsClosed || out.VerifiedBytes != 2*out.PayloadBytes {
				t.Fatalf("bad counts: %+v", out)
			}
			seen := map[string]bool{}
			for _, id := range f.ids {
				if seen[id] {
					t.Fatal("transaction reused")
				}
				seen[id] = true
			}
			if len(seen) != 3 || f.aborts != 0 {
				t.Fatalf("transactions: %v, aborts: %d", f.ids, f.aborts)
			}
		})
	}
}

func TestCycleFailuresCloseOrRemainUncertain(t *testing.T) {
	for _, scenario := range []string{"lost prepare", "corrupt restore", "unknown commit", "failed abort"} {
		t.Run(scenario, func(t *testing.T) {
			r, f, ctx := cycleFixture(t, "filesystem", "posix-copy")
			switch scenario {
			case "lost prepare":
				f.prepareError = errors.New("lost response")
			case "corrupt restore":
				f.corrupt = true
			case "unknown commit":
				f.commitError = errors.New("outcome unknown")
			case "failed abort":
				f.prepareError = errors.New("lost response")
				f.abortError = errors.New("abort failed")
			}
			out := report{Timings: map[string]float64{}}
			if err := cycle(ctx, f, r, &out); err == nil {
				t.Fatal("failure passed")
			}
			if f.aborts != 1 {
				t.Fatalf("aborts=%d", f.aborts)
			}
			if scenario == "failed abort" && out.TransactionsClosed {
				t.Fatal("uncertain cleanup passed")
			}
			if scenario == "unknown commit" && len(f.ids) != 1 {
				t.Fatal("recaptured after unknown Commit")
			}
		})
	}
}

func TestFixtureVerification(t *testing.T) {
	for _, change := range []string{"byte", "missing", "extra", "size", "mode", "empty-directory", "symlink", "root-mode"} {
		t.Run(change, func(t *testing.T) {
			r, _, ctx := cycleFixture(t, "filesystem", "posix-copy")
			root := t.TempDir()
			require.NoError(t, generate(ctx, root, r))
			m, err := inspect(ctx, root)
			require.NoError(t, err)
			path := filepath.Join(root, "nested/small.bin")
			switch change {
			case "byte":
				err = os.WriteFile(path, []byte{1, 255, 1, 128, 10, 0, 42}, 0640)
			case "missing":
				err = os.Remove(path)
			case "extra":
				err = os.WriteFile(filepath.Join(root, "extra"), nil, 0600)
			case "size":
				err = os.WriteFile(path, []byte{1}, 0640)
			case "mode":
				err = os.Chmod(path, 0600)
			case "empty-directory":
				err = os.Remove(filepath.Join(root, "nested/empty-dir"))
			case "symlink":
				err = os.Symlink(path, filepath.Join(root, "link"))
			case "root-mode":
				err = os.Chmod(root, 0750)
			}
			require.NoError(t, err)
			if err := verify(ctx, root, m); err == nil {
				t.Fatal("corrupt tree accepted")
			}
		})
	}
}

func admitCorpus(t *testing.T, ctx context.Context, r *request) fixtureManifest {
	t.Helper()
	m, err := inspect(ctx, r.FixtureDir)
	require.NoError(t, err)
	r.FixtureManifest = filepath.Join(t.TempDir(), "fixture.json")
	require.NoError(t, writeJSON(r.FixtureManifest, m, maxManifest))
	r.ManifestSHA256, err = hashFile(ctx, r.FixtureManifest)
	require.NoError(t, err)
	return m
}

func TestAdmittedCorpus(t *testing.T) {
	for _, change := range []string{"byte", "mode", "root-mode", "extra", "missing", "symlink", "fifo", "admission-record"} {
		t.Run(change, func(t *testing.T) {
			r, f, ctx := cycleFixture(t, filesystemBackend, "posix-copy")
			r.FixtureDir = t.TempDir()
			require.NoError(t, generate(ctx, r.FixtureDir, r))
			m := admitCorpus(t, ctx, &r)
			destination := t.TempDir()
			require.NoError(t, copyFixture(ctx, r.FixtureDir, destination, m))
			for _, path := range []string{destination, r.FixtureDir} {
				if err := verify(ctx, path, m); err != nil {
					t.Fatal("copy or source differs", err)
				}
			}
			path := filepath.Join(r.FixtureDir, "nested/small.bin")
			var err error
			switch change {
			case "byte":
				err = os.WriteFile(path, []byte("changed"), 0640)
			case "mode":
				err = os.Chmod(path, 0400)
			case "root-mode":
				err = os.Chmod(r.FixtureDir, 0750)
			case "extra":
				err = os.WriteFile(filepath.Join(r.FixtureDir, "extra"), nil, 0600)
			case "missing":
				err = os.Remove(path)
			case "symlink":
				err = os.Symlink(path, filepath.Join(r.FixtureDir, "link"))
			case "fifo":
				err = syscall.Mkfifo(filepath.Join(r.FixtureDir, "fifo"), 0600)
			case "admission-record":
				err = os.WriteFile(r.FixtureManifest, []byte("{}"), 0600)
			}
			require.NoError(t, err)
			out := report{Timings: map[string]float64{}}
			if err := cycle(ctx, f, r, &out); err == nil || out.Publications != 0 || f.aborts != 1 {
				t.Fatalf("changed source reached publication or escaped cleanup: %+v, %v", out, err)
			}
		})
	}
}

func TestCapturedCorpusIncludesReservedGpuTree(t *testing.T) {
	r, f, ctx := cycleFixture(t, filesystemBackend, "posix-copy")
	r.FixtureDir = t.TempDir()
	require.NoError(t, generate(ctx, r.FixtureDir, r))
	require.NoError(t, os.Mkdir(filepath.Join(r.FixtureDir, "gpu"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(r.FixtureDir, "gpu/data"), []byte("captured GPU bytes"), 0400))
	// WalkDir visits children before the next sibling; sorting whole paths
	// would incorrectly move a.index before a/data in the wrapped manifest.
	for _, parent := range []string{"", "nested"} {
		require.NoError(t, os.Mkdir(filepath.Join(r.FixtureDir, parent, "a"), 0700))
		for _, name := range []string{"a/data", "a.index"} {
			require.NoError(t, os.WriteFile(filepath.Join(r.FixtureDir, parent, name), []byte(name), 0600))
		}
	}
	m := admitCorpus(t, ctx, &r)
	out := report{Timings: map[string]float64{}}
	require.NoError(t, cycle(ctx, f, r, &out))
	if _, err := os.Stat(filepath.Join(r.Destination, "gpu")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reserved root GPU directory exposed")
	}
	require.NoError(t, verify(ctx, filepath.Join(r.Destination, "corpus"), m))
	if err := verify(ctx, r.FixtureDir, m); err != nil {
		t.Fatal("source mutated", err)
	}
	wrapped, err := wrappedManifest(m)
	if err != nil || out.PayloadBytes != wrapped.Bytes || out.Restores != 2 || out.VerifiedBytes != 2*wrapped.Bytes {
		t.Fatal("wrapper byte accounting", err)
	}
}

func TestUnsafePathsAndInputs(t *testing.T) {
	r, _, _ := cycleFixture(t, "filesystem", "posix-copy")
	for _, path := range []string{r.StagingRoot, filepath.Dir(r.StagingRoot), "/", "relative"} {
		if err := validateStaging(path, r.StagingRoot); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	link := filepath.Join(r.StagingRoot, "link")
	require.NoError(t, os.Symlink(t.TempDir(), link))
	if err := validateStaging(link, r.StagingRoot); err == nil {
		t.Fatal("symlink accepted")
	}
	for _, path := range []string{"../escape", "a\\b", "a\nb", "/absolute", strings.Repeat("a", 513)} {
		if validPath(path) {
			t.Fatalf("accepted %q", path)
		}
	}
	input := filepath.Join(t.TempDir(), "input")
	for _, data := range []string{`{"unknown":1}`, `{ } { }`, strings.Repeat("x", maxRequest+1)} {
		require.NoError(t, os.WriteFile(input, []byte(data), 0600))
		if err := readJSON(input, maxRequest, &request{}); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := generate(ctx, t.TempDir(), r); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestRejectsFilesystemModelStreamer(t *testing.T) {
	r, f, _ := cycleFixture(t, filesystemBackend, "posix-copy")
	r.TransferEngine = "model-streamer"
	require.Error(t, validateCycle(r))
	require.Empty(t, f.ids)
}
