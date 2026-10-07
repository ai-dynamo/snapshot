// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestMetadataAbortHasIndependentDeadline(t *testing.T) {
	for _, metadataFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadataFails=%t", metadataFails), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			manifest := &types.CheckpointManifest{Artifact: types.ArtifactManifest{ContentUID: "content", ContainerName: "main"}}
			require.NoError(t, types.WriteManifest(dir, manifest))
			listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
			require.NoError(t, err)
			defer listener.Close()
			abortReceived := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			server := make(chan error, 1)
			go func() {
				server <- serveMetadataWithStalledAbort(listener, dir, metadataFails, abortReceived, release)
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := fetchArtifactMetadata(ctx, pagebroker.Client{ControlSocketPath: listener.Addr().String()},
					pagebroker.FilesystemArtifact("store", "content", "main"))
				done <- err
			}()
			select {
			case <-abortReceived:
			case err := <-server:
				t.Fatalf("metadata server stopped before Abort: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("metadata cleanup did not send Abort")
			}
			select {
			case err := <-done:
				require.ErrorContains(t, err, "abort PageBroker metadata transaction")
				require.NoError(t, ctx.Err(), "cleanup must expire while the parent remains live")
			case <-time.After(pageBrokerAbortTimeout + 2*time.Second):
				t.Fatal("metadata Abort exceeded its cleanup deadline")
			}
		})
	}
}

func serveMetadataWithStalledAbort(
	listener net.Listener, dir string, metadataFails bool, abortReceived chan<- struct{}, release <-chan struct{},
) error {
	for i := range 2 {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		defer conn.Close()
		var size uint32
		if err := binary.Read(conn, binary.BigEndian, &size); err != nil {
			return err
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(conn, data); err != nil {
			return err
		}
		request := new(pagebroker.Request)
		if err := proto.Unmarshal(data, request); err != nil {
			return err
		}
		if i == 1 {
			if request.GetAbort() == nil {
				return fmt.Errorf("expected Abort, got %v", request.GetCommand())
			}
			close(abortReceived)
			<-release
			return nil
		}
		response := &pagebroker.Response{RequestId: request.RequestId, TransactionId: request.TransactionId}
		if metadataFails {
			response.Result = &pagebroker.Response_Failure{
				Failure: &pagebroker.Failure{
					Code: pagebroker.Failure_ARTIFACT_NOT_FOUND.Enum(), Message: proto.String("not published"),
				},
			}
		} else {
			response.Result = &pagebroker.Response_GetArtifactMetadataComplete{
				GetArtifactMetadataComplete: &pagebroker.GetArtifactMetadataComplete{ManifestDirectory: dir},
			}
		}
		data, err = proto.Marshal(response)
		if err != nil {
			return err
		}
		if err := binary.Write(conn, binary.BigEndian, uint32(len(data))); err != nil {
			return err
		}
		if _, err := conn.Write(data); err != nil {
			return err
		}
	}
	return nil
}
