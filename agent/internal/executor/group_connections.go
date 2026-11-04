// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/tcphold"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// keptConnections is the agent's side of a group member's restore, which keeps
// the member's TCP connections to the other members.
//
// A kept connection comes back in each member's own restore, and the members
// finish seconds apart. A packet that reaches a peer before the peer's socket
// exists draws a reset that kills the connection at both ends, and kernel
// timers send such packets even from a socket in repair mode, so the hold
// drops every packet of the kept connections from before CRIU runs. nsrestore
// restores those sockets without SO_KEEPALIVE, so no keepalive probe can abort
// them meanwhile, and stops at CRIU's post-restore, before CRIU takes the
// sockets out of repair mode and resumes the tasks. Once every member has
// restored its sockets, the agent lifts the hold, turns keepalive back on, and
// lets CRIU continue, so no task runs while a peer it talks to does not exist
// yet.
type keptConnections struct {
	log     logr.Logger
	attempt string
	// peers maps the other members' IPs at capture to their IPs now.
	peers criu.IPMap
	// socketsRestored returns once every member has restored its sockets.
	socketsRestored func(context.Context) error
	pid             int
	connections     []criu.TCPConnection
	// reported is set once nsrestore has reported its restored sockets.
	reported bool
	// waited is how long CRIU waited for the other members to restore their
	// sockets, once wait has returned.
	waited time.Duration
	// agent and nsrestore are the two ends of the peer sync socket. nsrestore
	// inherits its end.
	agent, nsrestore *os.File
	served           chan error
}

// keepConnections holds the connections a restore from checkpointPath keeps for
// g's other members, in the network namespace of the placeholder pid, and
// serves nsrestore's end of the peer sync socket until nsrestore reports its
// restored sockets. Every member passes the sockets-restored barrier, whether
// or not it keeps a connection.
func keepConnections(ctx context.Context, log logr.Logger, g *group.Member, manifest *types.CheckpointManifest,
	targetPodIP string, pid int, checkpointPath string,
) (*keptConnections, error) {
	peers, err := g.PeerIPs(manifest.Group.PodIPs)
	if err != nil {
		return nil, err
	}
	ipMap, err := criu.RestoreIPMap(manifest, targetPodIP, peers)
	if err != nil {
		return nil, err
	}
	connections, err := criu.PreservedConnections(checkpointPath, peers, ipMap)
	if err != nil {
		return nil, fmt.Errorf("list the preserved connections: %w", err)
	}
	agent, nsrestore, err := peerSyncSocket()
	if err != nil {
		return nil, err
	}
	k := &keptConnections{
		log:             log,
		attempt:         g.Attempt(),
		peers:           peers,
		socketsRestored: func(ctx context.Context) error { return g.Barrier(ctx, group.SocketsRestored) },
		pid:             pid,
		connections:     connections,
		agent:           agent,
		nsrestore:       nsrestore,
		served:          make(chan error, 1),
	}
	if len(connections) > 0 {
		if err := tcphold.Install(pid, k.attempt, connections); err != nil {
			k.close()
			return nil, err
		}
		log.Info("Holding the connections kept for the group's other members",
			"attempt", k.attempt, "connections", len(connections))
	}
	go func() { k.served <- k.serve(ctx) }()
	return k, nil
}

// peerSyncSocket creates the socket on which nsrestore reports its restored
// sockets and the agent lets it continue.
func peerSyncSocket() (agent, nsrestore *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("create the peer sync socket: %w", err)
	}
	// A non-blocking end lets closing it end a read in progress.
	if err := unix.SetNonblock(fds[0], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return nil, nil, fmt.Errorf("create the peer sync socket: %w", err)
	}
	return os.NewFile(uintptr(fds[0]), "peer-sync-agent"), os.NewFile(uintptr(fds[1]), "peer-sync-nsrestore"), nil
}

// inherit passes nsrestore the peer IPs and its end of the peer sync socket.
func (k *keptConnections) inherit(cmd *exec.Cmd) {
	cmd.Args = append(cmd.Args, "--peer-ips", k.peers.String(),
		"--peer-sync-fd", strconv.Itoa(firstExtraFileDescriptor+len(cmd.ExtraFiles)))
	cmd.ExtraFiles = append(cmd.ExtraFiles, k.nsrestore)
}

// serve waits for nsrestore to report its restored sockets, then for every
// member to have restored theirs, lifts the hold, turns keepalive back on, and
// lets nsrestore continue. Any error leaves nsrestore without an answer, which
// fails its CRIU restore before a task runs.
func (k *keptConnections) serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = k.agent.Close() })
	defer stop()
	if _, err := io.ReadFull(k.agent, make([]byte, 1)); err != nil {
		return fmt.Errorf("nsrestore did not report its restored sockets: %w", err)
	}
	k.reported = true
	start := time.Now()
	if err := k.socketsRestored(ctx); err != nil {
		return fmt.Errorf("wait for the other members to restore their sockets: %w", err)
	}
	k.waited = time.Since(start)
	if err := k.lift(); err != nil {
		return err
	}
	if err := tcphold.RestoreKeepalive(k.pid, k.connections); err != nil {
		return fmt.Errorf("turn keepalive back on: %w", err)
	}
	if _, err := k.agent.Write([]byte{0}); err != nil {
		return fmt.Errorf("let nsrestore continue: %w", err)
	}
	k.log.Info("Every member restored its sockets", "attempt", k.attempt, "waited", k.waited.String())
	return nil
}

// wait returns, once nsrestore has exited with nsrestoreErr, how the restore
// went. Once nsrestore has reported its sockets, a failure to keep the
// connections is why nsrestore failed, so it comes first. Before that,
// nsrestore's own failure comes first. Both are kept.
func (k *keptConnections) wait(nsrestoreErr error) error {
	// nsrestore has exited, so closing both ends ends a serve still reading.
	_ = k.nsrestore.Close()
	_ = k.agent.Close()
	keepErr := failStep(StepKeepPeerConnections, <-k.served)
	if keepErr == nil {
		return nsrestoreErr
	}
	if !k.reported && nsrestoreErr != nil {
		return errors.Join(nsrestoreErr, keepErr)
	}
	return errors.Join(keepErr, nsrestoreErr)
}

// lift removes the hold, if the member keeps any connection. Lifting a hold
// that is gone succeeds, so lifting twice is harmless.
func (k *keptConnections) lift() error {
	if len(k.connections) == 0 {
		return nil
	}
	return tcphold.Lift(k.pid, k.attempt)
}

// close lifts the hold and closes both ends of the peer sync socket.
func (k *keptConnections) close() {
	if err := k.lift(); err != nil {
		k.log.Error(err, "Failed to lift the hold", "attempt", k.attempt)
	}
	_ = k.nsrestore.Close()
	_ = k.agent.Close()
}

// awaitPeers returns the post-restore hook of a group member's nsrestore: it
// reports the restored sockets to the agent on sync, one byte, and waits for
// the agent's byte that lets the restore continue. No answer aborts the
// restore.
func awaitPeers(sync io.ReadWriter) func() error {
	return func() error {
		if _, err := sync.Write([]byte{0}); err != nil {
			return fmt.Errorf("report the restored sockets to the agent: %w", err)
		}
		if _, err := io.ReadFull(sync, make([]byte, 1)); err != nil {
			return fmt.Errorf("the agent did not let the restore continue: %w", err)
		}
		return nil
	}
}
