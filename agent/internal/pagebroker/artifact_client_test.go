// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

func testPublication() *PublishedArtifact {
	return &PublishedArtifact{
		StoreId:               "store-v1-" + strings.Repeat("a", 64),
		ArtifactHandle:        "artifacts/content-1/containers/main/publications/p-1/index.json",
		ArtifactFormatVersion: "snapshot.pagebroker/v1",
	}
}

// Each connection carries one request/reply, including reconnects after lost replies.
func artifactTestServer(t *testing.T, handle func(net.Conn, *Request) error) Client {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			connection, err := listener.Accept()
			if errors.Is(err, net.ErrClosed) {
				done <- nil
				return
			}
			if err == nil {
				err = connection.SetDeadline(time.Now().Add(3 * time.Second))
				if err == nil {
					var message []byte
					message, err = readMessage(connection)
					if err == nil {
						request := new(Request)
						err = proto.Unmarshal(message, request)
						if err == nil {
							err = handle(connection, request)
						}
					}
				}
				_ = connection.Close()
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-done; err != nil {
			t.Errorf("fake PageBroker: %v", err)
		}
	})
	return Client{ControlSocketPath: listener.Addr().String()}
}

func replyFor(request *Request, result isResponse_Result) *Response {
	return &Response{RequestId: request.RequestId, TransactionId: request.TransactionId, Result: result}
}

func sendResponse(connection net.Conn, response *Response) error {
	message, err := proto.Marshal(response)
	if err != nil {
		return err
	}
	return writeMessage(connection, message)
}

func TestArtifactAndLegacySelectors(t *testing.T) {
	artifact := testPublication()
	target := &ArtifactTarget{StoreId: artifact.StoreId, Artifact: &ArtifactIdentity{ArtifactUid: "content-1", ContainerName: "main"}}
	for _, tc := range []struct {
		name    string
		command isRequest_Command
		call    func(Client, context.Context) (string, error)
		result  isResponse_Result
	}{
		{"artifact checkpoint", &Request_PrepareStagedCheckpoint{PrepareStagedCheckpoint: &PrepareStagedCheckpointRequest{Target: target}},
			func(c Client, ctx context.Context) (string, error) {
				return c.PrepareArtifactCheckpoint(ctx, "tx", target, nil)
			},
			&Response_StagedCheckpointDirectory{StagedCheckpointDirectory: &StagedCheckpointDirectory{ImageDirectory: proto.String("/staging/checkpoint/tx")}}},
		{"artifact restore", &Request_StagedRestore{StagedRestore: &StagedRestoreRequest{Artifact: artifact}},
			func(c Client, ctx context.Context) (string, error) {
				return c.StagedArtifactRestore(ctx, "tx", artifact, nil)
			},
			&Response_StagedRestoreDirectory{StagedRestoreDirectory: &StagedRestoreDirectory{ImageDirectory: proto.String("/staging/restore/tx")}}},
		{"metadata", &Request_GetArtifactMetadata{GetArtifactMetadata: &GetArtifactMetadataRequest{Artifact: artifact}},
			func(c Client, ctx context.Context) (string, error) { return c.GetArtifactMetadata(ctx, "tx", artifact) },
			&Response_GetArtifactMetadataComplete{GetArtifactMetadataComplete: &GetArtifactMetadataComplete{ManifestDirectory: "/staging/metadata/tx"}}},
		{"legacy checkpoint", &Request_PrepareStagedCheckpoint{PrepareStagedCheckpoint: &PrepareStagedCheckpointRequest{Destination: filesystem("/checkpoints/destination"), IoEngine: posixCopy()}},
			func(c Client, ctx context.Context) (string, error) {
				return c.PrepareCheckpoint(ctx, "tx", "/checkpoints/destination")
			},
			&Response_StagedCheckpointDirectory{StagedCheckpointDirectory: &StagedCheckpointDirectory{ImageDirectory: proto.String("/staging/checkpoint/tx")}}},
		{"legacy restore", &Request_StagedRestore{StagedRestore: &StagedRestoreRequest{Source: filesystem("/checkpoints/source"), IoEngine: posixCopy()}},
			func(c Client, ctx context.Context) (string, error) {
				return c.StagedRestore(ctx, "tx", "/checkpoints/source")
			},
			&Response_StagedRestoreDirectory{StagedRestoreDirectory: &StagedRestoreDirectory{ImageDirectory: proto.String("/staging/restore/tx")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := artifactTestServer(t, func(connection net.Conn, request *Request) error {
				want := &Request{RequestId: request.RequestId, TransactionId: proto.String("tx"), Command: tc.command}
				if request.GetRequestId() == "" || !proto.Equal(request, want) {
					return fmt.Errorf("unexpected selector: %v", request)
				}
				return sendResponse(connection, replyFor(request, tc.result))
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if directory, err := tc.call(client, ctx); err != nil || !strings.HasPrefix(directory, "/staging/") {
				t.Fatalf("directory = %q, error = %v", directory, err)
			}
		})
	}
}

func TestCheckpointCommitRetainsDescriptorAcrossLostReply(t *testing.T) {
	artifact := testPublication()
	var previousID string
	attempts := 0
	client := artifactTestServer(t, func(connection net.Conn, request *Request) error {
		attempts++
		if request.GetCommit() == nil || request.GetTransactionId() != "tx" || request.GetRequestId() == previousID {
			return fmt.Errorf("invalid Commit retry: %v", request)
		}
		previousID = request.GetRequestId()
		if attempts == 1 {
			return nil // Publication completed, but the reply was lost.
		}
		return sendResponse(connection, replyFor(request, &Response_CommitComplete{CommitComplete: &CommitComplete{PublishedArtifact: artifact}}))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for range 2 { // A later explicit Commit sees the same immutable descriptor too.
		got, err := client.CommitCheckpoint(ctx, "tx")
		if err != nil || !proto.Equal(got, artifact) {
			t.Fatalf("CommitCheckpoint() = %v, %v", got, err)
		}
	}
}

func TestArtifactResponsesRejectInvalidResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Response)
	}{
		{"wrong request ID", func(r *Response) { r.RequestId = proto.String("another") }},
		{"wrong transaction ID", func(r *Response) { r.TransactionId = proto.String("another") }},
		{"missing result", func(r *Response) { r.Result = nil }},
		{"wrong result", func(r *Response) { r.Result = &Response_AbortComplete{AbortComplete: &AbortComplete{}} }},
		{"missing descriptor", func(r *Response) { r.GetCommitComplete().PublishedArtifact = nil }},
		{"missing store", func(r *Response) { r.GetCommitComplete().PublishedArtifact.StoreId = "" }},
		{"missing handle", func(r *Response) { r.GetCommitComplete().PublishedArtifact.ArtifactHandle = "" }},
		{"missing version", func(r *Response) { r.GetCommitComplete().PublishedArtifact.ArtifactFormatVersion = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := artifactTestServer(t, func(connection net.Conn, request *Request) error {
				response := replyFor(request, &Response_CommitComplete{CommitComplete: &CommitComplete{PublishedArtifact: testPublication()}})
				tc.mutate(response)
				return sendResponse(connection, response)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if result, err := client.CommitCheckpoint(ctx, "tx"); err == nil || result != nil {
				t.Fatalf("accepted invalid result: %v, %v", result, err)
			}
		})
	}
}

func TestStorageFailuresRemainTypedAndAreNotRetried(t *testing.T) {
	for _, code := range []Failure_Code{Failure_STORE_MISMATCH, Failure_ACCESS_DENIED, Failure_STORAGE_UNAVAILABLE,
		Failure_ARTIFACT_NOT_FOUND, Failure_ARTIFACT_CORRUPT, Failure_UNSUPPORTED_ARTIFACT, Failure_OUTCOME_UNKNOWN,
		Failure_TRANSACTION_EXPIRED, Failure_Code(99)} {
		t.Run(code.String(), func(t *testing.T) {
			calls := 0
			client := artifactTestServer(t, func(connection net.Conn, request *Request) error {
				calls++
				if calls != 1 {
					return fmt.Errorf("retried definitive Failure")
				}
				return sendResponse(connection, replyFor(request, &Response_Failure{Failure: &Failure{Code: &code, Message: proto.String("failure")}}))
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := client.CommitCheckpoint(ctx, "tx")
			var failure *FailureError
			if !errors.As(err, &failure) || failure.Code() != failureCode(code) {
				t.Fatalf("classification lost: %v", err)
			}
		})
	}
}

func TestOversizedFailureMessageIsProtocolError(t *testing.T) {
	for _, code := range []Failure_Code{Failure_STORE_MISMATCH, Failure_ACCESS_DENIED, Failure_STORAGE_UNAVAILABLE,
		Failure_ARTIFACT_NOT_FOUND, Failure_ARTIFACT_CORRUPT, Failure_UNSUPPORTED_ARTIFACT,
		Failure_OUTCOME_UNKNOWN, Failure_TRANSACTION_EXPIRED} {
		message := strings.Repeat("x", maxFailureMessageSize+1)
		calls := 0
		client := artifactTestServer(t, func(connection net.Conn, request *Request) error {
			calls++
			if calls != 1 {
				return fmt.Errorf("retried invalid failure")
			}
			return sendResponse(connection, replyFor(request, &Response_Failure{
				Failure: &Failure{Code: &code, Message: &message},
			}))
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := client.CommitCheckpoint(ctx, "tx")
		cancel()
		var failure *FailureError
		if err == nil || errors.As(err, &failure) {
			t.Fatalf("invalid failure accepted as typed broker result: %v", err)
		}
	}
}

func TestFailureMessageLimitsPreserveLegacyDiagnostics(t *testing.T) {
	for _, code := range []Failure_Code{Failure_UNSPECIFIED, Failure_INVALID_REQUEST, Failure_TRANSACTION_NOT_FOUND,
		Failure_TRANSACTION_CONFLICT, Failure_INSUFFICIENT_STORAGE, Failure_STORAGE_ERROR, Failure_INTERNAL_ERROR,
		Failure_Code(99), Failure_STORAGE_UNAVAILABLE} {
		t.Run(code.String(), func(t *testing.T) {
			message := "filesystem error: " + strings.Repeat("a", 1200)
			if isStorageFailure(code) {
				// The inclusive extension bound counts UTF-8 bytes, not runes.
				message = strings.Repeat("é", maxFailureMessageSize/2)
			}
			client := artifactTestServer(t, func(connection net.Conn, request *Request) error {
				return sendResponse(connection, replyFor(request, &Response_Failure{
					Failure: &Failure{Code: &code, Message: &message},
				}))
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := client.StagedRestore(ctx, "tx", "/checkpoints/source")
			var failure *FailureError
			if !errors.As(err, &failure) || failure.Code() != failureCode(code) || !strings.Contains(err.Error(), message) {
				t.Fatalf("failure diagnostic/classification lost: %v", err)
			}
		})
	}
}

func TestMetadataRejectsOtherOrEmptyResults(t *testing.T) {
	for _, result := range []isResponse_Result{
		&Response_CommitComplete{CommitComplete: &CommitComplete{}},
		&Response_GetArtifactMetadataComplete{GetArtifactMetadataComplete: &GetArtifactMetadataComplete{}},
	} {
		client := artifactTestServer(t, func(connection net.Conn, request *Request) error {
			return sendResponse(connection, replyFor(request, result))
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := client.GetArtifactMetadata(ctx, "tx", testPublication())
		cancel()
		if err == nil {
			t.Fatal("accepted invalid metadata result")
		}
	}
}

func TestArtifactRequestsValidateBeforeConnecting(t *testing.T) {
	client := Client{ControlSocketPath: "/nonexistent-pagebroker.sock"}
	ctx := context.Background()
	for _, artifact := range []*PublishedArtifact{nil, {}, {StoreId: "store"}, {StoreId: "store", ArtifactHandle: "handle"}} {
		_, err := client.GetArtifactMetadata(ctx, "tx", artifact)
		if err == nil || isTransportError(err) {
			t.Fatalf("invalid metadata input: %v", err)
		}
		_, err = client.StagedArtifactRestore(ctx, "tx", artifact, nil)
		if err == nil || isTransportError(err) {
			t.Fatalf("invalid restore input: %v", err)
		}
	}
	for _, target := range []*ArtifactTarget{nil, {}, {StoreId: "store"}, {StoreId: "store", Artifact: &ArtifactIdentity{ArtifactUid: "uid"}}} {
		_, err := client.PrepareArtifactCheckpoint(ctx, "tx", target, nil)
		if err == nil || isTransportError(err) {
			t.Fatalf("invalid target: %v", err)
		}
	}
	artifact := testPublication()
	artifact.ArtifactHandle = strings.Repeat("x", maxMessageSize)
	_, err := client.StagedArtifactRestore(ctx, "tx", artifact, nil)
	if !errors.Is(err, errMessageTooLarge) || isTransportError(err) {
		t.Fatalf("oversized request: %v", err)
	}
}

func TestCheckpointCommitRejectsInvalidWireWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(net.Conn) error
	}{
		{"malformed protobuf", func(c net.Conn) error { return writeMessage(c, []byte{0xff}) }},
		{"oversized frame", func(c net.Conn) error { return binary.Write(c, binary.BigEndian, uint32(maxMessageSize+1)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := artifactTestServer(t, func(c net.Conn, _ *Request) error {
				calls++
				if calls != 1 {
					return fmt.Errorf("retried invalid response")
				}
				return tc.write(c)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := client.CommitCheckpoint(ctx, "tx"); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected immediate protocol error: %v", err)
			}
		})
	}
}
