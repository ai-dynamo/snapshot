// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// emptyRestoreManifest is the manifest of an artifact that holds nothing but
// its identity: content "content", container "main".
func emptyRestoreManifest() *types.CheckpointManifest {
	return types.NewCheckpointManifest("content", "main", types.CRIUDumpManifest{},
		types.SourcePodManifest{}, types.OverlayManifest{}, types.HostManifest{})
}

// writeRestoreArtifact writes manifest where Restore looks for the artifact
// of content "content" and container "main" under base, and returns that
// directory.
func writeRestoreArtifact(t *testing.T, base string, manifest *types.CheckpointManifest) string {
	t.Helper()
	directory, err := nsmount.ResolveArtifactPath(base, "content", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := types.WriteManifest(directory, manifest); err != nil {
		t.Fatal(err)
	}
	return directory
}

// stubNSRestore makes every nsrestore launch in the test return result and
// err.
func stubNSRestore(t *testing.T, result *RestoreInNamespaceResult, err error) {
	t.Helper()
	original := execNSRestoreFunc
	t.Cleanup(func() { execNSRestoreFunc = original })
	execNSRestoreFunc = func(context.Context, logr.Logger, RestoreRequest, *types.RestoreContainerSnapshot, nsmount.MountPoint, string, *pagebroker.CustomStorageExecution) (*RestoreInNamespaceResult, error) {
		return result, err
	}
}

// stubRestoredProcessCheck makes every check of the restored process in the
// test return err.
func stubRestoredProcessCheck(t *testing.T, err error) {
	t.Helper()
	original := validateRestoredProcessFunc
	t.Cleanup(func() { validateRestoredProcessFunc = original })
	validateRestoredProcessFunc = func(string, int, logr.Logger) error { return err }
}

// pageBrokerExchange is one connection the fake PageBroker answers: the
// command it expects and its reply. A nil reply closes the connection
// unanswered. received, when set, runs once the request arrives.
type pageBrokerExchange struct {
	command  string
	reply    *pagebroker.Response
	received func()
}

func stageExchange(directory string) pageBrokerExchange {
	return pageBrokerExchange{command: "StagedRestore", reply: &pagebroker.Response{
		Result: &pagebroker.Response_StagedRestoreDirectory{StagedRestoreDirectory: &pagebroker.StagedRestoreDirectory{ImageDirectory: &directory}},
	}}
}

func commitExchange() pageBrokerExchange {
	return pageBrokerExchange{command: "Commit", reply: &pagebroker.Response{
		Result: &pagebroker.Response_CommitComplete{CommitComplete: &pagebroker.CommitComplete{}},
	}}
}

func abortExchange() pageBrokerExchange {
	return pageBrokerExchange{command: "Abort", reply: &pagebroker.Response{
		Result: &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}},
	}}
}

func (e pageBrokerExchange) failWith(code pagebroker.Failure_Code, message string) pageBrokerExchange {
	e.reply = &pagebroker.Response{Result: &pagebroker.Response_Failure{Failure: &pagebroker.Failure{
		Code: code.Enum(), Message: proto.String(message),
	}}}
	return e
}

func (e pageBrokerExchange) unanswered() pageBrokerExchange {
	e.reply = nil
	return e
}

func (e pageBrokerExchange) onRequest(received func()) pageBrokerExchange {
	e.received = received
	return e
}

// servePageBroker answers one connection per exchange, in order. Its channel,
// for waitPageBrokerTest, gets the first error, or nil after the last
// exchange.
func servePageBroker(listener *net.UnixListener, exchanges ...pageBrokerExchange) <-chan error {
	result := make(chan error, 1)
	go func() {
		for i, exchange := range exchanges {
			if err := answerPageBroker(listener, exchange); err != nil {
				result <- fmt.Errorf("PageBroker exchange %d: %w", i+1, err)
				return
			}
		}
		result <- nil
	}()
	return result
}

func answerPageBroker(listener *net.UnixListener, exchange pageBrokerExchange) error {
	connection, err := listener.Accept()
	if err != nil {
		return err
	}
	defer connection.Close()
	request, err := readPageBrokerTestRequest(connection)
	if err != nil {
		return err
	}
	if got := strings.TrimPrefix(fmt.Sprintf("%T", request.GetCommand()), "*pagebroker.Request_"); got != exchange.command {
		return fmt.Errorf("got %s, want %s", got, exchange.command)
	}
	if exchange.received != nil {
		exchange.received()
	}
	if exchange.reply == nil {
		return nil
	}
	return replyPageBrokerTest(connection, request, exchange.reply)
}
