//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// Test adapters keep scenarios expressed by identity while exercising #395's public client.
type artifactTestClient struct{ Client }

func testTarget(id string) *ArtifactTarget {
	return &ArtifactTarget{StoreId: "test-store", Artifact: &ArtifactIdentity{ArtifactUid: id, ContainerName: "container"}}
}

func testArtifact(id string) *PublishedArtifact {
	input := ""
	for _, part := range []string{"test-store", id, "container"} {
		input += fmt.Sprintf("%d:%s", len(part), part)
	}
	digest := sha256.Sum256([]byte(input))
	return &PublishedArtifact{StoreId: "test-store", ArtifactHandle: hex.EncodeToString(digest[:])}
}

func (c artifactTestClient) prepareArtifact(ctx context.Context, tx, id string) (string, error) {
	return c.PrepareArtifactCheckpoint(ctx, tx, testTarget(id), nil)
}
func (c artifactTestClient) restoreArtifact(ctx context.Context, tx, id string) (string, error) {
	return c.StagedArtifactRestore(ctx, tx, testArtifact(id), nil)
}
func (c artifactTestClient) metadata(ctx context.Context, tx, id string) (string, error) {
	return c.GetArtifactMetadata(ctx, tx, testArtifact(id))
}

// Run with tests/checkpoint_integration.py against packaged native daemons.
func TestS3CheckpointDaemon(t *testing.T) {
	socket := os.Getenv("PAGEBROKER_CHECKPOINT_SOCKET")
	if socket == "" {
		t.Skip("requires the local S3 checkpoint integration fixture")
	}
	client := artifactTestClient{Client{ControlSocketPath: socket}}
	other := artifactTestClient{Client{ControlSocketPath: os.Getenv("PAGEBROKER_CHECKPOINT_SECOND_SOCKET")}}
	short := artifactTestClient{Client{ControlSocketPath: os.Getenv("PAGEBROKER_CHECKPOINT_SHORT_SOCKET")}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	httpClient := &http.Client{Timeout: 30 * time.Second}
	if ca := os.Getenv("PAGEBROKER_CHECKPOINT_CA"); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			t.Fatal("invalid fixture CA")
		}
		httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	}
	control := func(t *testing.T, action, id string) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, os.Getenv("PAGEBROKER_CHECKPOINT_CONTROL")+"/"+action+"/"+testArtifact(id).ArtifactHandle, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := httpClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("fixture control %s: %s", action, response.Status)
		}
	}
	// Bound completion waits below the fixture's 20-second hold limit, so its
	// timeout cannot satisfy tests that keep I/O blocked.
	await := func(t *testing.T, result <-chan error) error {
		t.Helper()
		select {
		case err := <-result:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("operation did not complete before the test deadline")
			return nil
		}
	}
	ids := map[string]string{}
	target := func(t *testing.T, scenario string) string {
		t.Helper()
		if id := ids[scenario]; id != "" {
			return id
		}
		id := uuid.NewString()
		ids[scenario] = id
		control(t, "label-"+scenario, id)
		return id
	}
	expectCode := func(t *testing.T, err error, code Failure_Code) {
		t.Helper()
		var failure *FailureError
		if !errors.As(err, &failure) || failure.Code() != code {
			t.Fatalf("wanted %s, got %v", code, err)
		}
	}
	write := func(t *testing.T, directory, payload string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(directory, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{"manifest.yaml": []byte("version: 1\n"), "nested/data": []byte(payload), "empty": {}} {
			if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	publish := func(t *testing.T, scenario, payload string) string {
		t.Helper()
		id := target(t, scenario)
		directory, err := client.prepareArtifact(ctx, scenario+"-capture", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, payload)
		published, err := client.CommitCheckpoint(ctx, scenario+"-capture")
		if err != nil {
			t.Fatal(err)
		}
		retained, err := client.CommitCheckpoint(ctx, scenario+"-capture")
		if err != nil || !proto.Equal(published, retained) || !proto.Equal(published, testArtifact(id)) {
			t.Fatalf("publication descriptor was not stable: %v %v %v", published, retained, err)
		}
		if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("committed staging remains: %v", err)
		}
		return id
	}
	verify := func(t *testing.T, transaction, id, payload string) {
		t.Helper()
		directory, err := client.metadata(ctx, transaction+"-metadata", id)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 1 || entries[0].Name() != "manifest.yaml" {
			t.Fatalf("metadata: %v %v", entries, err)
		}
		if err := client.Abort(ctx, transaction+"-metadata"); err != nil {
			t.Fatal(err)
		}
		directory, err = client.restoreArtifact(ctx, transaction+"-restore", id)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(directory, "nested/data"))
		if err != nil || !bytes.Equal(data, []byte(payload)) {
			t.Fatalf("restored payload differs: %v", err)
		}
		if info, err := os.Stat(filepath.Join(directory, "empty")); err != nil || info.Size() != 0 {
			t.Fatalf("empty payload: %v", err)
		}
		if err := client.Commit(ctx, transaction+"-restore"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("roundtrip and preflight", func(t *testing.T) {
		payload := strings.Repeat("binary\x00payload\xff", 600000)
		id := publish(t, "roundtrip", payload)
		verify(t, "roundtrip", id, payload)
		_, err := short.restoreArtifact(ctx, "over-capacity", id)
		expectCode(t, err, Failure_INSUFFICIENT_STORAGE)
		_, err = client.prepareArtifact(ctx, "recapture", id)
		expectCode(t, err, Failure_TRANSACTION_CONFLICT)
		artifact := testArtifact(id)
		_, err = client.StagedArtifactRestore(ctx, "wrong-engine", artifact, &IOEngine{Kind: &IOEngine_PosixCopy{PosixCopy: &PosixCopyIOEngine{}}})
		expectCode(t, err, Failure_INVALID_REQUEST)
		artifact.ArtifactHandle = strings.Repeat("A", 64)
		_, err = client.StagedArtifactRestore(ctx, "malformed-handle", artifact, nil)
		expectCode(t, err, Failure_INVALID_REQUEST)
		artifact = testArtifact(id)
		artifact.StoreId = "another-store"
		_, err = client.GetArtifactMetadata(ctx, "wrong-store", artifact)
		expectCode(t, err, Failure_STORE_MISMATCH)
		artifact = testArtifact(id)
		control(t, "unsupported-format", id)
		_, err = client.StagedArtifactRestore(ctx, "wrong-format", artifact, nil)
		expectCode(t, err, Failure_UNSUPPORTED_ARTIFACT)
		expectCode(t, client.Commit(ctx, "unknown"), Failure_TRANSACTION_NOT_FOUND)
	})
	t.Run("invalid selectors leave the transaction available", func(t *testing.T) {
		id := target(t, "selector-validation")
		destination := testTarget(id)
		destination.StoreId = "wrong-store"
		_, err := client.PrepareArtifactCheckpoint(ctx, "selector-validation", destination, nil)
		expectCode(t, err, Failure_STORE_MISMATCH)
		_, err = client.request(ctx, "selector-validation", &Request_PrepareStagedCheckpoint{
			PrepareStagedCheckpoint: &PrepareStagedCheckpointRequest{},
		})
		expectCode(t, err, Failure_INVALID_REQUEST)
		_, err = client.PrepareArtifactCheckpoint(ctx, "selector-validation", testTarget(id), &IOEngine{})
		expectCode(t, err, Failure_INVALID_REQUEST)
		_, err = client.prepareArtifact(ctx, "selector-validation", id)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Abort(ctx, "selector-validation"); err != nil {
			t.Fatal(err)
		}
		control(t, "assert-no-upload", id)
	})
	t.Run("publication rejects an artifact created after preparation", func(t *testing.T) {
		id := target(t, "published-after-prepare")
		directory, err := client.prepareArtifact(ctx, "late-publisher", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "attempted overwrite")
		otherDirectory, err := other.prepareArtifact(ctx, "first-publisher", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, otherDirectory, "published payload")
		if err := other.Commit(ctx, "first-publisher"); err != nil {
			t.Fatal(err)
		}
		expectCode(t, client.Commit(ctx, "late-publisher"), Failure_TRANSACTION_CONFLICT)
		control(t, "assert-no-reupload", id)
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("conflict removed staging: %v", err)
		}
		verify(t, "published-after-prepare", id, "published payload")
		if err := client.Abort(ctx, "late-publisher"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("lost index response is confirmed", func(t *testing.T) {
		id := publish(t, "lost-index", "immutable payload")
		verify(t, "lost-index", id, "immutable payload")
	})
	t.Run("failed payload can retry the same capture", func(t *testing.T) {
		id := target(t, "lost-payload")
		directory, err := client.prepareArtifact(ctx, "lost-payload", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "immutable payload")
		expectCode(t, client.Commit(ctx, "lost-payload"), Failure_STORAGE_UNAVAILABLE)
		control(t, "assert-unpublished", id)
		control(t, "confirm", id)
		if err := client.Commit(ctx, "lost-payload"); err != nil {
			t.Fatal(err)
		}
		verify(t, "lost-payload", id, "immutable payload")
	})
	t.Run("denial is not absence", func(t *testing.T) {
		_, err := client.prepareArtifact(ctx, "denied-access", target(t, "denied-access"))
		expectCode(t, err, Failure_ACCESS_DENIED)
	})
	t.Run("uncertain index retries never rewrite payloads", func(t *testing.T) {
		id := target(t, "unconfirmed-index")
		directory, err := client.prepareArtifact(ctx, "unconfirmed-index", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "one capture")
		expectCode(t, client.Commit(ctx, "unconfirmed-index"), Failure_OUTCOME_UNKNOWN)
		expectCode(t, client.Commit(ctx, "unconfirmed-index"), Failure_OUTCOME_UNKNOWN)
		control(t, "confirm", id)
		if err := client.Commit(ctx, "unconfirmed-index"); err != nil {
			t.Fatal(err)
		}
		verify(t, "unconfirmed-index", id, "one capture")
		control(t, "assert-no-reupload", id)
	})
	t.Run("a different published index is a conflict", func(t *testing.T) {
		id := target(t, "conflicting-index")
		directory, err := client.prepareArtifact(ctx, "conflicting-index", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "one capture")
		expectCode(t, client.Commit(ctx, "conflicting-index"), Failure_OUTCOME_UNKNOWN)
		control(t, "conflicting-index", id)
		expectCode(t, client.Commit(ctx, "conflicting-index"), Failure_TRANSACTION_CONFLICT)
		control(t, "assert-no-reupload", id)
		if err := client.Abort(ctx, "conflicting-index"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("complete preflight precedes every payload write", func(t *testing.T) {
		id := target(t, "missing-manifest")
		directory, err := client.prepareArtifact(ctx, "missing-manifest", id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "data"), []byte("unpublished"), 0600); err != nil {
			t.Fatal(err)
		}
		expectCode(t, client.Commit(ctx, "missing-manifest"), Failure_INVALID_REQUEST)
		control(t, "assert-no-upload", id)
		if err := client.Abort(ctx, "missing-manifest"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing objects and malformed indexes", func(t *testing.T) {
		for _, scenario := range []string{"missing-empty", "corrupt-index"} {
			id := publish(t, scenario, "valid data")
			control(t, scenario, id)
			_, err := client.restoreArtifact(ctx, scenario+"-bad-restore", id)
			expectCode(t, err, Failure_ARTIFACT_CORRUPT)
		}
	})
	t.Run("same-size payload replacement is not integrity checked", func(t *testing.T) {
		id := publish(t, "corrupt-data", "valid data")
		control(t, "corrupt-data", id)
		// The fixture re-PUTs a regular payload with its first byte changed.
		// Its size and transport checks remain valid; the test checks bytes independently.
		verify(t, "replaced-payload", id, "walid data")
	})
	t.Run("one writer owns a checkpoint destination", func(t *testing.T) {
		id := target(t, "owned")
		_, err := client.prepareArtifact(ctx, "owner", id)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.prepareArtifact(ctx, "second-owner", id)
		expectCode(t, err, Failure_TRANSACTION_CONFLICT)
		if err := client.Abort(ctx, "owner"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("credentials remain fixed until external restart", func(t *testing.T) {
		credentials := os.Getenv("PAGEBROKER_CHECKPOINT_CREDENTIALS")
		value := "[default]\naws_access_key_id=rotated-test-key\naws_secret_access_key=local-test-secret-key\n"
		if err := os.WriteFile(credentials+".next", []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(credentials+".next", credentials); err != nil {
			t.Fatal(err)
		}
		id := publish(t, "credentials-fixed", "startup credentials")
		verify(t, "credentials-fixed", id, "startup credentials")
	})
	t.Run("Abort during index preflight", func(t *testing.T) {
		for _, operation := range []string{"prepare", "restore", "metadata"} {
			t.Run(operation, func(t *testing.T) {
				transaction := "blocked-preflight-" + operation
				id := target(t, transaction)
				if operation != "prepare" {
					publish(t, transaction, "preflight cancellation")
				}
				control(t, "block", id)
				defer control(t, "release", id)
				staged := make(chan error, 1)
				go func() {
					var err error
					switch operation {
					case "prepare":
						_, err = client.prepareArtifact(ctx, transaction, id)
					case "restore":
						_, err = client.restoreArtifact(ctx, transaction, id)
					case "metadata":
						_, err = client.metadata(ctx, transaction, id)
					}
					staged <- err
				}()
				control(t, "wait", id)
				aborted := make(chan error, 1)
				go func() { aborted <- client.Abort(ctx, transaction) }()
				if err := await(t, aborted); err != nil {
					t.Fatal(err)
				}
				expectCode(t, await(t, staged), Failure_INTERNAL_ERROR)
				control(t, "release", id)
				expectCode(t, client.Commit(ctx, transaction), Failure_TRANSACTION_NOT_FOUND)
				if operation == "prepare" {
					control(t, "assert-no-upload", id)
				}
			})
		}
	})
	t.Run("read-only restored trees release their staging reservations", func(t *testing.T) {
		id := target(t, "read-only")
		directory, err := client.prepareArtifact(ctx, "read-only-capture", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "read-only contents")
		for _, path := range []string{directory, filepath.Join(directory, "nested")} {
			if err := os.Chmod(path, 0500); err != nil {
				t.Fatal(err)
			}
		}
		if err := client.Commit(ctx, "read-only-capture"); err != nil {
			t.Fatal(err)
		}
		for _, release := range []string{"commit", "abort"} {
			// More restores than the active-transaction limit detect leaked slots.
			for attempt := range 10 {
				transaction := fmt.Sprintf("read-only-%s-%d", release, attempt)
				directory, err = client.restoreArtifact(ctx, transaction, id)
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{directory, filepath.Join(directory, "nested")} {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0500 {
						t.Fatalf("restore did not preserve read-only mode: %s: %v", path, err)
					}
				}
				if release == "commit" {
					err = client.Commit(ctx, transaction)
				} else {
					err = client.Abort(ctx, transaction)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("released staging remains: %v", err)
				}
			}
		}
	})
	t.Run("Abort racing active restore drains the reader", func(t *testing.T) {
		id := publish(t, "blocked-restore", "a payload held by the native reader")
		control(t, "block", id)
		defer control(t, "release", id)
		restored := make(chan error, 1)
		var directory string
		go func() {
			var err error
			directory, err = client.restoreArtifact(ctx, "blocked-native", id)
			restored <- err
		}()
		control(t, "wait", id)
		aborted := make(chan error, 1)
		go func() { aborted <- client.Abort(ctx, "blocked-native") }()
		// Native teardown needs I/O to finish. Exact cancellation ordering is
		// covered by the controlled ModelStreamerSessionTest tests.
		control(t, "release", id)
		if err := await(t, aborted); err != nil {
			t.Fatal(err)
		}
		if err := await(t, restored); err != nil {
			expectCode(t, err, Failure_INTERNAL_ERROR)
		} else if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("aborted restore staging remains: %v", err)
		}
		verify(t, "after-abort", id, "a payload held by the native reader")
	})
	t.Run("Abort racing active upload", func(t *testing.T) {
		id := target(t, "blocked-upload")
		directory, err := client.prepareArtifact(ctx, "blocked-upload", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, strings.Repeat("A", 7*1024*1024))
		control(t, "block", id)
		defer control(t, "release", id)
		committed := make(chan error, 1)
		go func() { committed <- client.Commit(ctx, "blocked-upload") }()
		control(t, "wait", id)
		aborted := make(chan error, 1)
		go func() { aborted <- client.Abort(ctx, "blocked-upload") }()
		// Release native I/O before waiting for Abort to drain it. The S3
		// CancellationDrainsAndLeavesClientUsable test controls the token directly.
		control(t, "release", id)
		commitErr, abortErr := await(t, committed), await(t, aborted)
		if commitErr == nil {
			expectCode(t, abortErr, Failure_TRANSACTION_NOT_FOUND)
			verify(t, "upload-won", id, strings.Repeat("A", 7*1024*1024))
		} else {
			if abortErr != nil {
				t.Fatal(abortErr)
			}
			var failure *FailureError
			if errors.As(commitErr, &failure) && failure.Code() == Failure_INTERNAL_ERROR {
				control(t, "assert-unpublished", id)
			} else {
				// Cancellation during publication leaves either no index or a
				// complete checkpoint, never a partially restorable artifact.
				expectCode(t, commitErr, Failure_OUTCOME_UNKNOWN)
				_, err := client.metadata(ctx, "upload-outcome", id)
				if err != nil {
					expectCode(t, err, Failure_ARTIFACT_NOT_FOUND)
				} else {
					if err := client.Abort(ctx, "upload-outcome"); err != nil {
						t.Fatal(err)
					}
					verify(t, "upload-uncertain", id, strings.Repeat("A", 7*1024*1024))
				}
			}
		}
		if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("finished upload staging remains: %v", err)
		}
	})
	t.Run("Abort racing publication preserves the known checkpoint", func(t *testing.T) {
		id := target(t, "publication-abort")
		directory, err := client.prepareArtifact(ctx, "publication-abort", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "published before the response")
		// Hold the index response after the service has actually written it.
		control(t, "block", id)
		defer control(t, "release", id)
		committed := make(chan error, 1)
		go func() { committed <- client.Commit(ctx, "publication-abort") }()
		control(t, "wait", id)
		aborted := make(chan error, 1)
		go func() { aborted <- client.Abort(ctx, "publication-abort") }()
		control(t, "release", id)
		commitErr, abortErr := await(t, committed), await(t, aborted)
		if commitErr == nil {
			// A confirmed publication takes precedence over a concurrent Abort.
			expectCode(t, abortErr, Failure_TRANSACTION_NOT_FOUND)
		} else {
			expectCode(t, commitErr, Failure_OUTCOME_UNKNOWN)
			if abortErr != nil {
				t.Fatal(abortErr)
			}
		}
		verify(t, "publication-abort", id, "published before the response")
		control(t, "assert-no-reupload", id)
	})
	t.Run("expired transactions reject Commit and Abort until reaped", func(t *testing.T) {
		id := target(t, "expired-checkpoint")
		directory, err := short.prepareArtifact(ctx, "expired-checkpoint", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "captured once")
		time.Sleep(1100 * time.Millisecond)
		expectCode(t, short.Commit(ctx, "expired-checkpoint"), Failure_TRANSACTION_EXPIRED)
		expectCode(t, short.Abort(ctx, "expired-checkpoint"), Failure_TRANSACTION_EXPIRED)
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("expired requests must leave staging for the reaper: %v", err)
		}
		id = target(t, "deadline-upload")
		directory, err = short.prepareArtifact(ctx, "deadline-upload", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "active upload expires")
		control(t, "block", id)
		defer control(t, "release", id)
		committed := make(chan error, 1)
		go func() { committed <- short.Commit(ctx, "deadline-upload") }()
		control(t, "wait", id)
		time.Sleep(1100 * time.Millisecond)
		control(t, "release", id)
		expectCode(t, <-committed, Failure_TRANSACTION_EXPIRED)
		expectCode(t, short.Abort(ctx, "deadline-upload"), Failure_TRANSACTION_EXPIRED)
		expectCode(t, short.Abort(ctx, "deadline-upload"), Failure_TRANSACTION_EXPIRED)
	})
	t.Run("restart preserves publication and discards orphan staging", func(t *testing.T) {
		id := target(t, "restart")
		directory, err := other.prepareArtifact(ctx, "restart", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "durable bytes")
		if err := other.Commit(ctx, "restart"); err != nil {
			t.Fatal(err)
		}
		orphan, err := other.prepareArtifact(ctx, "orphan", target(t, "orphan"))
		if err != nil {
			t.Fatal(err)
		}
		write(t, orphan, "abandoned checkpoint")
		if err := os.Chmod(filepath.Join(orphan, "nested"), 0500); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(orphan, 0500); err != nil {
			t.Fatal(err)
		}
		control(t, "restart", id)
		expectCode(t, other.Commit(ctx, "restart"), Failure_TRANSACTION_NOT_FOUND)
		_, err = other.prepareArtifact(ctx, "restart-recapture", id)
		expectCode(t, err, Failure_TRANSACTION_CONFLICT)
		verify(t, "restart", id, "durable bytes")
		if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restart left orphan staging: %v", err)
		}
		if _, err := other.prepareArtifact(ctx, "orphan", target(t, "new-orphan")); err != nil {
			t.Fatalf("restart retained the old transaction: %v", err)
		}
		if err := other.Abort(ctx, "orphan"); err != nil {
			t.Fatal(err)
		}
		// The deployment restarts the daemon with replacement credentials for uploader and reader.
		id = target(t, "credentials-restarted")
		directory, err = other.prepareArtifact(ctx, "credentials-restarted", id)
		if err != nil {
			t.Fatal(err)
		}
		write(t, directory, "replacement credentials")
		if err := other.Commit(ctx, "credentials-restarted"); err != nil {
			t.Fatal(err)
		}
		directory, err = other.restoreArtifact(ctx, "credentials-restarted-restore", id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(directory, "nested/data")); err != nil {
			t.Fatal(err)
		}
		if err := other.Abort(ctx, "credentials-restarted-restore"); err != nil {
			t.Fatal(err)
		}
	})
}
