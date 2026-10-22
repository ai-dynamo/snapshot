// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// pagebroker-bench performs one bounded file-transfer operation for the Python runner.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pb "github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/google/uuid"
)

const (
	maxRequest     = 64 << 10
	maxReport      = 16 << 10
	maxManifest    = 4 << 20
	cleanupReserve = 30 * time.Second
)

type request struct {
	SchemaVersion   int    `json:"schema_version"`
	Action          string `json:"action"`
	RunID           string `json:"run_id"`
	CycleID         string `json:"cycle_id"`
	Socket          string `json:"socket"`
	StagingRoot     string `json:"staging_root"`
	Backend         string `json:"backend"`
	TransferEngine  string `json:"transfer_engine"`
	Destination     string `json:"destination"`
	StoreID         string `json:"store_id"`
	FixtureDir      string `json:"fixture_dir"`
	FixtureManifest string `json:"fixture_manifest"`
	ManifestSHA256  string `json:"manifest_sha256"`
	PayloadBytes    int64  `json:"payload_bytes"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
}

type report struct {
	SchemaVersion      int                   `json:"schema_version"`
	Action             string                `json:"action"`
	RunID              string                `json:"run_id"`
	CycleID            string                `json:"cycle_id"`
	Backend            string                `json:"backend"`
	TransferEngine     string                `json:"transfer_engine"`
	Status             string                `json:"status"`
	Publications       int                   `json:"publications_completed"`
	Restores           int                   `json:"restores_completed"`
	PayloadBytes       int64                 `json:"payload_bytes"`
	VerifiedBytes      int64                 `json:"verified_bytes"`
	TransactionsClosed bool                  `json:"transactions_closed"`
	Artifact           *pb.PublishedArtifact `json:"published_artifact,omitempty"`
	ManifestSHA256     string                `json:"manifest_sha256,omitempty"`
	Timings            map[string]float64    `json:"timings"`
	ErrorStage         string                `json:"error_stage,omitempty"`
	ErrorMessage       string                `json:"error_message,omitempty"`
	Retryable          bool                  `json:"retryable,omitempty"`
}

func readJSON(path string, maximum int, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(maximum+1)))
	if err != nil {
		return err
	}
	if len(data) > maximum {
		return errors.New("JSON input exceeds size limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func writeJSON(path string, value any, maximum int) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maximum {
		return errors.New("JSON output exceeds size limit")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func validateRequest(r request) error {
	if r.SchemaVersion != 1 || r.RunID == "" || len(r.RunID) > 128 || len(r.CycleID) > 128 {
		return errors.New("invalid schema or identity")
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > 7200 {
		return errors.New("invalid timeout")
	}
	switch r.Action {
	case "probe":
		if r.Socket == "" {
			return errors.New("probe requires socket")
		}
	case "inspect":
		if r.FixtureDir == "" || r.FixtureManifest == "" {
			return errors.New("inspect requires source and manifest")
		}
	case "cycle":
		return validateCycle(r)
	default:
		return errors.New("invalid action")
	}
	return nil
}

func execute(ctx context.Context, r request) (out report) {
	out = report{SchemaVersion: 1, Action: r.Action, RunID: r.RunID, CycleID: r.CycleID,
		Backend: r.Backend, TransferEngine: r.TransferEngine, Status: "failed", Timings: map[string]float64{}}
	var err error
	client := pb.Client{ControlSocketPath: r.Socket}
	switch r.Action {
	case "probe":
		_, err = client.Capabilities(ctx)
		out.Retryable = errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
	case "inspect":
		var m fixtureManifest
		m, err = inspect(ctx, r.FixtureDir)
		if err == nil {
			var wrapped fixtureManifest
			wrapped, err = wrappedManifest(m)
			out.PayloadBytes = wrapped.Bytes
		}
		if err == nil {
			err = writeJSON(r.FixtureManifest, m, maxManifest)
		}
		if err == nil {
			out.ManifestSHA256, err = hashFile(ctx, r.FixtureManifest)
		}
	case "cycle":
		err = cycle(ctx, client, r, &out)
	}
	if err != nil {
		out.ErrorMessage = fmt.Sprintf("%.2048s", err.Error())
	} else {
		out.Status = "success"
	}
	return out
}

func main() { os.Exit(command()) }

func command() int {
	input := flag.String("request", "", "owned JSON request path")
	output := flag.String("report", "", "owned JSON report path")
	flag.Parse()
	var r request
	if *output == "" || readJSON(*input, maxRequest, &r) != nil || validateRequest(r) != nil {
		fmt.Fprintln(os.Stderr, "invalid benchmark request")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.TimeoutSeconds)*time.Second)
	defer cancel()
	out := execute(ctx, r)
	if err := writeJSON(*output, out, maxReport); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write benchmark report")
		return 1
	}
	if out.Status != "success" {
		return 1
	}
	return 0
}

type brokerClient interface {
	PrepareCheckpoint(context.Context, string, string) (string, error)
	StagedRestore(context.Context, string, string) (string, error)
	PrepareArtifactCheckpoint(context.Context, string, *pb.ArtifactTarget, *pb.IOEngine) (string, error)
	CommitCheckpoint(context.Context, string) (*pb.PublishedArtifact, error)
	StagedArtifactRestore(context.Context, string, *pb.PublishedArtifact, *pb.IOEngine) (string, error)
	Commit(context.Context, string) error
	Abort(context.Context, string) error
}

const filesystemBackend = "filesystem"

func validateCycle(r request) error {
	if r.Socket == "" || !filepath.IsAbs(r.StagingRoot) || r.CycleID == "" || r.TimeoutSeconds <= 30 {
		return errors.New("cycle requires socket, absolute staging root, identity and timeout over 30s")
	}
	if r.TransferEngine != "posix-copy" && r.TransferEngine != "model-streamer" {
		return errors.New("unsupported transfer engine")
	}
	if r.Backend == "s3" {
		if r.TransferEngine != "model-streamer" || r.StoreID == "" {
			return errors.New("S3 requires store and Model Streamer")
		}
	} else if r.Backend != filesystemBackend || r.TransferEngine != "posix-copy" || !filepath.IsAbs(r.Destination) {
		return errors.New("filesystem requires POSIX and an absolute destination")
	}
	if r.FixtureDir != "" {
		if r.FixtureManifest == "" || len(r.ManifestSHA256) != 64 {
			return errors.New("missing admitted fixture manifest")
		}
	} else if r.PayloadBytes < 1 || r.PayloadBytes > 1<<40 {
		return errors.New("invalid payload size")
	}
	return nil
}

// A registered ID survives lost prepare replies. Unconfirmed cleanup never passes.
type cycleState struct {
	client   brokerClient
	r        request
	out      *report
	open     string
	manifest fixtureManifest
	engine   *pb.IOEngine
}

func cycle(ctx context.Context, client brokerClient, r request, out *report) (err error) {
	s := cycleState{client: client, r: r, out: out,
		engine: &pb.IOEngine{Kind: &pb.IOEngine_ModelStreamer{ModelStreamer: &pb.ModelStreamerIOEngine{}}}}
	// Leave time for Abort inside the parent's whole-helper deadline.
	deadline, _ := ctx.Deadline()
	opCtx, cancel := context.WithDeadline(ctx, deadline.Add(-cleanupReserve))
	defer cancel()
	defer func() {
		if s.open != "" {
			cleanupCtx, done := context.WithTimeout(context.Background(), cleanupReserve)
			defer done()
			cleanupErr := s.client.Abort(cleanupCtx, s.open)
			err = errors.Join(err, cleanupErr)
			out.TransactionsClosed = cleanupErr == nil
		}
	}()
	if err = s.publish(opCtx); err != nil {
		return err
	}
	for i := 1; i <= 2; i++ {
		if err = s.restore(opCtx, i); err != nil {
			return err
		}
	}
	out.TransactionsClosed = true
	return nil
}

func (s *cycleState) timed(stage string, fn func() error) error {
	s.out.ErrorStage = stage
	start := time.Now()
	err := fn()
	s.out.Timings[stage] = time.Since(start).Seconds()
	if err == nil {
		s.out.ErrorStage = ""
	}
	return err
}

func (s *cycleState) prepare(ctx context.Context) (string, error) {
	s.open = uuid.NewString()
	if s.r.Backend == filesystemBackend {
		return s.client.PrepareCheckpoint(ctx, s.open, s.r.Destination)
	}
	target := &pb.ArtifactTarget{StoreId: s.r.StoreID,
		Artifact: &pb.ArtifactIdentity{ArtifactUid: uuid.NewString(), ContainerName: "benchmark"}}
	return s.client.PrepareArtifactCheckpoint(ctx, s.open, target, s.engine)
}

func (s *cycleState) publish(ctx context.Context) error {
	var directory string
	if err := s.timed("prepare", func() (err error) { directory, err = s.prepare(ctx); return err }); err != nil {
		return err
	}
	if err := validateStaging(directory, s.r.StagingRoot); err != nil {
		return err
	}
	if err := s.timed("fixture", func() (err error) {
		if s.r.FixtureDir == "" {
			err = generate(ctx, directory, s.r)
			if err == nil {
				s.manifest, err = inspect(ctx, directory)
			}
		} else {
			var source fixtureManifest
			source, err = admittedManifest(ctx, s.r)
			if err == nil {
				s.manifest, err = copyCorpus(ctx, s.r.FixtureDir, directory, source)
			}
		}
		return err
	}); err != nil {
		return err
	}
	if err := s.timed("publish", func() error { return s.commitPublication(ctx) }); err != nil {
		return err
	}
	s.open = ""
	s.out.Publications = 1
	s.out.PayloadBytes = s.manifest.Bytes
	return requireAbsent(directory)
}

func (s *cycleState) commitPublication(ctx context.Context) error {
	if s.r.Backend == filesystemBackend {
		return s.client.Commit(ctx, s.open)
	}
	artifact, err := s.client.CommitCheckpoint(ctx, s.open)
	if err != nil {
		return err
	}
	s.out.Artifact = artifact
	if artifact.GetStoreId() != s.r.StoreID {
		return errors.New("publication names another store")
	}
	return nil
}

func (s *cycleState) restore(ctx context.Context, index int) error {
	s.open = uuid.NewString()
	var directory string
	if err := s.timed(fmt.Sprintf("restore_%d", index), func() (err error) {
		if s.r.Backend == filesystemBackend {
			directory, err = s.client.StagedRestore(ctx, s.open, s.r.Destination)
		} else {
			directory, err = s.client.StagedArtifactRestore(ctx, s.open, s.out.Artifact, s.engine)
		}
		return err
	}); err != nil {
		return err
	}
	if err := validateStaging(directory, s.r.StagingRoot); err != nil {
		return err
	}
	if err := s.timed(fmt.Sprintf("verify_%d", index), func() error { return verify(ctx, directory, s.manifest) }); err != nil {
		return err
	}
	if err := s.client.Commit(ctx, s.open); err != nil {
		return err
	}
	s.open = ""
	if err := requireAbsent(directory); err != nil {
		return err
	}
	s.out.Restores++
	s.out.VerifiedBytes += s.manifest.Bytes
	return nil
}

func requireAbsent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("transaction staging remains: %w", errors.Join(err, errors.New("expected absence")))
}

func validateStaging(path, root string) error {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(base, canonical)
	if err != nil || !filepath.IsAbs(path) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("staging path escapes owned root")
	}
	if canonical != filepath.Clean(path) {
		return errors.New("staging path contains symlink")
	}
	return nil
}
