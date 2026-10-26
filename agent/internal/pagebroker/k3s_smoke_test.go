// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

const smokeCycles = 3
const smokePayloadBytes = (128 << 20) + 37

type smokeConfig struct {
	Mode        string `json:"mode"`
	Socket      string `json:"socket"`
	StagingRoot string `json:"stagingRoot"`
	StorageRoot string `json:"storageRoot,omitempty"`
	StoreID     string `json:"storeId,omitempty"`
	Report      string `json:"-"`
}

type smokeStep struct {
	Name    string        `json:"name"`
	Status  string        `json:"status"`
	Elapsed time.Duration `json:"elapsedNanoseconds"`
	Error   string        `json:"error,omitempty"`
}

type smokePublication struct {
	Engine      string             `json:"engine"`
	Destination string             `json:"destination,omitempty"`
	Artifact    *PublishedArtifact `json:"artifact,omitempty"`
}

type smokeReport struct {
	RunID        string             `json:"runId"`
	Status       string             `json:"status"`
	Config       smokeConfig        `json:"config"`
	PayloadBytes int64              `json:"payloadBytes"`
	Started      time.Time          `json:"started"`
	Finished     time.Time          `json:"finished"`
	Steps        []smokeStep        `json:"steps"`
	Publications []smokePublication `json:"publications"`
	Error        string             `json:"error,omitempty"`
}

type smokeRunner struct {
	client Client
	config smokeConfig
	report *smokeReport
	logf   func(string, ...any)
}

// TestPageBrokerK3sSmoke exercises an externally managed daemon through the public client.
func TestPageBrokerK3sSmoke(t *testing.T) {
	if os.Getenv("PAGEBROKER_SMOKE_MODE") == "" {
		t.Skip("requires PAGEBROKER_SMOKE_MODE and a shared PageBroker socket/staging mount")
	}
	config, err := readSmokeConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	report := smokeReport{
		RunID: uuid.NewString(), Status: "RUNNING", Config: config,
		PayloadBytes: smokePayloadBytes, Started: time.Now().UTC(),
	}
	if err := createSmokeReport(config.Report, &report); err != nil {
		t.Fatal(err)
	}
	runner := smokeRunner{client: Client{ControlSocketPath: config.Socket}, config: config, report: &report, logf: t.Logf}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	err = runner.run(ctx)
	report.Finished = time.Now().UTC()
	report.Status = "SUCCESS"
	if err != nil {
		report.Status = "ERROR"
		report.Error = err.Error()
	}
	if saveErr := saveSmokeReport(config.Report, &report); saveErr != nil {
		err = errors.Join(err, saveErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func readSmokeConfig(getenv func(string) string) (smokeConfig, error) {
	config := smokeConfig{
		Mode: getenv("PAGEBROKER_SMOKE_MODE"), Socket: getenv("PAGEBROKER_SMOKE_SOCKET"),
		StagingRoot: getenv("PAGEBROKER_SMOKE_STAGING_ROOT"), StorageRoot: getenv("PAGEBROKER_SMOKE_STORAGE_ROOT"),
		StoreID: getenv("PAGEBROKER_SMOKE_STORE_ID"), Report: getenv("PAGEBROKER_SMOKE_REPORT"),
	}
	if config.Mode != "filesystem" && config.Mode != "s3" {
		return config, errors.New("PAGEBROKER_SMOKE_MODE must be filesystem or s3")
	}
	paths := map[string]string{"SOCKET": config.Socket, "STAGING_ROOT": config.StagingRoot, "REPORT": config.Report}
	if config.Mode == "filesystem" {
		paths["STORAGE_ROOT"] = config.StorageRoot
	} else if config.StoreID == "" {
		return config, errors.New("PAGEBROKER_SMOKE_STORE_ID is required for S3")
	}
	for name, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return config, fmt.Errorf("PAGEBROKER_SMOKE_%s must be a normalized absolute path other than /", name)
		}
	}
	return config, nil
}

func (r *smokeRunner) run(ctx context.Context) (result error) {
	if err := r.step(ctx, "ready", func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for {
			if _, err := r.client.Capabilities(ctx); err == nil {
				return nil
			} else if !transientDialError(err) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}); err != nil {
		return err
	}
	if r.config.Mode == "s3" {
		return r.runEngine(ctx, "model-streamer", "")
	}
	// Only remove the private directory created by this run, never the supplied storage root.
	storage, err := os.MkdirTemp(r.config.StorageRoot, "pagebroker-smoke-")
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, os.RemoveAll(storage))
	}()
	return r.runEngine(ctx, "posix-copy", storage)
}

func (r *smokeRunner) step(ctx context.Context, name string, action func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	started := time.Now()
	err := action(ctx)
	step := smokeStep{Name: name, Status: "SUCCESS", Elapsed: time.Since(started)}
	if err != nil {
		step.Status, step.Error = "ERROR", err.Error()
	}
	r.report.Steps = append(r.report.Steps, step)
	r.logf("%s: %s (%s)", step.Status, name, step.Elapsed)
	return errors.Join(err, saveSmokeReport(r.config.Report, r.report))
}

func (r *smokeRunner) transaction(action func(string) error) (err error) {
	id := uuid.NewString()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanup := r.client.Abort(ctx, id)
		if !smokeFailureCode(cleanup, Failure_TRANSACTION_NOT_FOUND) {
			err = errors.Join(err, cleanup)
		}
	}()
	return action(id)
}

func (r *smokeRunner) prepare(ctx context.Context, tx string, publication smokePublication) (string, error) {
	var directory string
	var err error
	if r.config.Mode == "s3" {
		directory, err = r.client.PrepareArtifactCheckpoint(ctx, tx, &ArtifactTarget{
			StoreId:  r.config.StoreID,
			Artifact: &ArtifactIdentity{ArtifactUid: uuid.NewString(), ContainerName: "smoke"},
		}, smokeModelStreamer())
	} else {
		directory, err = r.client.PrepareCheckpoint(ctx, tx, publication.Destination)
	}
	if err == nil {
		err = validateSmokeStaging(r.config.StagingRoot, directory)
	}
	return directory, err
}

func (r *smokeRunner) runEngine(ctx context.Context, engine string, storage string) error {
	for cycle := 1; cycle <= smokeCycles; cycle++ {
		name := fmt.Sprintf("%s/cycle-%d", engine, cycle)
		publication := smokePublication{Engine: engine}
		if storage != "" {
			publication.Destination = filepath.Join(storage, fmt.Sprintf("%s-%d", engine, cycle))
		}
		var expected map[string]smokeEntry
		if err := r.step(ctx, name+"/publish", func(ctx context.Context) error {
			return r.transaction(func(tx string) error {
				directory, err := r.prepare(ctx, tx, publication)
				if err != nil {
					return err
				}
				if err := writeSmokeFixture(ctx, directory, smokePayloadBytes, r.report.RunID+name); err != nil {
					return err
				}
				expected, err = readSmokeTree(directory)
				if err != nil {
					return err
				}
				if r.config.Mode == "s3" {
					publication.Artifact, err = r.client.CommitCheckpoint(ctx, tx, r.config.StoreID)
				} else {
					err = r.client.Commit(ctx, tx)
				}
				if err != nil {
					return err
				}
				r.report.Publications = append(r.report.Publications, publication)
				return smokeRemoved(directory)
			})
		}); err != nil {
			return err
		}
		if r.config.Mode == "s3" {
			if err := r.step(ctx, name+"/metadata", func(ctx context.Context) error {
				return r.metadata(ctx, publication.Artifact, expected["manifest.yaml"])
			}); err != nil {
				return err
			}
		}
		for restore := 1; restore <= 2; restore++ {
			if err := r.step(ctx, fmt.Sprintf("%s/restore-%d", name, restore), func(ctx context.Context) error {
				return r.restore(ctx, publication, expected)
			}); err != nil {
				return err
			}
		}
	}
	if err := r.step(ctx, engine+"/abort-prepared", func(ctx context.Context) error {
		return r.transaction(func(tx string) error {
			publication := smokePublication{Engine: engine}
			if storage != "" {
				publication.Destination = filepath.Join(storage, engine+"-aborted")
			}
			directory, err := r.prepare(ctx, tx, publication)
			if err != nil {
				return err
			}
			if err := writeSmokeFixture(ctx, directory, 1024, "aborted"); err != nil {
				return err
			}
			if err := r.client.Abort(ctx, tx); err != nil {
				return err
			}
			if publication.Destination != "" {
				if err := smokeRemoved(publication.Destination); err != nil {
					return err
				}
			}
			return smokeRemoved(directory)
		})
	}); err != nil {
		return err
	}
	if r.config.Mode == "s3" {
		return r.step(ctx, "model-streamer/wrong-store", r.wrongStore)
	}
	return nil
}

func (r *smokeRunner) metadata(ctx context.Context, artifact *PublishedArtifact, expected smokeEntry) error {
	return r.transaction(func(tx string) error {
		directory, err := r.client.GetArtifactMetadata(ctx, tx, artifact)
		if err != nil {
			return err
		}
		if err := validateSmokeStaging(r.config.StagingRoot, directory); err != nil {
			return err
		}
		entries, err := readSmokeTree(directory)
		if err != nil {
			return err
		}
		manifest := entries["manifest.yaml"]
		if len(entries) != 2 || !entries["."].Directory || manifest.Directory || manifest.SHA256 != expected.SHA256 || manifest.Size != expected.Size {
			return errors.New("metadata must contain only the expected manifest.yaml")
		}
		if err := r.client.Abort(ctx, tx); err != nil {
			return err
		}
		return smokeRemoved(directory)
	})
}

func (r *smokeRunner) restore(ctx context.Context, publication smokePublication, expected map[string]smokeEntry) error {
	return r.transaction(func(tx string) error {
		var directory string
		var err error
		if publication.Artifact != nil {
			directory, err = r.client.StagedArtifactRestore(ctx, tx, publication.Artifact, smokeModelStreamer())
		} else {
			directory, err = r.client.StagedRestore(ctx, tx, publication.Destination)
		}
		if err != nil {
			return err
		}
		if err := validateSmokeStaging(r.config.StagingRoot, directory); err != nil {
			return err
		}
		if err := verifySmokeTree(directory, expected); err != nil {
			return err
		}
		if err := r.client.Commit(ctx, tx); err != nil {
			return err
		}
		return smokeRemoved(directory)
	})
}

func (r *smokeRunner) wrongStore(ctx context.Context) error {
	wrongID := uuid.NewString()
	published := r.report.Publications[0].Artifact
	wrong := &PublishedArtifact{StoreId: wrongID, ArtifactHandle: published.GetArtifactHandle()}
	for _, request := range []func(string) error{
		func(tx string) error {
			_, err := r.client.PrepareArtifactCheckpoint(ctx, tx, &ArtifactTarget{
				StoreId: wrongID, Artifact: &ArtifactIdentity{ArtifactUid: uuid.NewString(), ContainerName: "smoke"},
			}, smokeModelStreamer())
			return err
		},
		func(tx string) error { _, err := r.client.GetArtifactMetadata(ctx, tx, wrong); return err },
		func(tx string) error {
			_, err := r.client.StagedArtifactRestore(ctx, tx, wrong, smokeModelStreamer())
			return err
		},
	} {
		if err := r.transaction(func(tx string) error {
			err := request(tx)
			if !smokeFailureCode(err, Failure_STORE_MISMATCH) {
				return fmt.Errorf("expected STORE_MISMATCH, got %v", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func smokeModelStreamer() *IOEngine {
	return &IOEngine{Kind: &IOEngine_ModelStreamer{ModelStreamer: &ModelStreamerIOEngine{}}}
}

func smokeFailureCode(err error, code Failure_Code) bool {
	var failure *FailureError
	return errors.As(err, &failure) && failure.Code() == code
}

func createSmokeReport(path string, report *smokeReport) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	return errors.Join(json.NewEncoder(file).Encode(report), file.Close())
}

func saveSmokeReport(path string, report *smokeReport) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".smoke-report-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := errors.Join(json.NewEncoder(file).Encode(report), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
